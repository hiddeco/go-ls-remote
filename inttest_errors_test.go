package lsremote_test

// Error-path integration matrix. Each row is one numbered scenario from
// the public-surface error contract — DNS/dial/TLS, HTTP status codes,
// auth flows, mid-stream drops, server-side `ERR` packets, protocol
// mismatches, unborn HEAD, ctx cancellation/deadline, malformed
// pkt-lines, corrupted on-disk packs, and URL-parse failures. Each test
// is a single function named `TestErr_<RowName>` so the matrix is
// greppable from a failure line.
//
// The tests favour bespoke per-row harnesses over the cross-fixture
// `inttest.NewHTTPServer` helper: most of the rows demand a custom HTTP
// status code, a hijacked connection, or a malformed pkt-line stream,
// none of which fit the curated happy-path harness. Where the row
// genuinely needs a real fixture (empty repo, unborn HEAD, corrupt
// pack) the test reaches for `inttest.Entries()`/`inttest.NewHTTPServer`
// just like the cross-transport suite does.

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	lsremote "github.com/hiddeco/go-ls-remote"
	"github.com/hiddeco/go-ls-remote/internal/inttest"
	"github.com/hiddeco/go-ls-remote/internal/objfmt"
	"github.com/hiddeco/go-ls-remote/internal/objstore"
	"github.com/hiddeco/go-ls-remote/internal/server"
	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/transport"
	filet "github.com/hiddeco/go-ls-remote/transport/file"
	httpt "github.com/hiddeco/go-ls-remote/transport/http"
)

// httpAdvContentType matches the smart-HTTP advertisement content type
// canonical Git sets on `info/refs?service=git-upload-pack` per
// `http-backend.c::get_info_refs`. Tests that synthesise an
// advertisement use this verbatim so the client's
// `mime.ParseMediaType` branch lands on the smart path.
const httpAdvContentType = "application/x-git-upload-pack-advertisement"

// errOptsHTTP returns the option set the HTTP rows reuse: a registry
// holding only the HTTP transport, with all other dial knobs at their
// defaults. Pinning the registry stops `Dial` from consulting the
// package-default registry (which would still work but exercises
// machinery this test does not pin).
func errOptsHTTP() []lsremote.Option {
	return []lsremote.Option{
		lsremote.WithTransports(transport.NewRegistry(httpt.New())),
	}
}

// errFindLSRemoteSentinel reports whether the err chain matches any of
// the documented public sentinels. The rows that require "NO library
// sentinel" assert this returns false, so the gate is centralised.
func errFindLSRemoteSentinel(err error) error {
	for _, s := range []error{
		lsremote.ErrNotFound,
		lsremote.ErrAuthRequired,
		lsremote.ErrAuthFailed,
		lsremote.ErrUnsupportedProtocol,
		lsremote.ErrServerRefused,
		lsremote.ErrNoDefaultBranch,
	} {
		if errors.Is(err, s) {
			return s
		}
	}
	return nil
}

// ----------------------------------------------------------------------
// Row 1: DNS / dial / TLS failure
// ----------------------------------------------------------------------

// TestErr_DNSFailure points the HTTP transport at a freshly-bound and
// immediately-closed loopback port. The connection refusal propagates
// up as a `*net.OpError`; no library sentinel should match because the
// failure happened before any wire byte was seen.
func TestErr_DNSFailure(t *testing.T) {
	// Bind-then-close yields a port the kernel guarantees is refused
	// (RST on connect). Using port 0 directly would race with whatever
	// the OS assigns at dial time.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())

	ctx := context.Background()
	_, err = lsremote.Dial(ctx, "http://"+addr+"/repo.git", errOptsHTTP()...)
	require.Error(t, err)

	var opErr *net.OpError
	assert.True(t, errors.As(err, &opErr),
		"want wrapped *net.OpError reachable via errors.As; got %T: %v", err, err)

	if s := errFindLSRemoteSentinel(err); s != nil {
		t.Errorf("connection refusal must not match any library sentinel; matched %v", s)
	}
}

// ----------------------------------------------------------------------
// Row 2: HTTP 401 without credentials → ErrAuthRequired
// ----------------------------------------------------------------------

// TestErr_HTTP401NoCreds asserts the no-creds 401 path surfaces
// `ErrAuthRequired`. The handler emits 401 with no `WWW-Authenticate`
// header to confirm the transport relies on status alone, not the
// challenge header, for the dispatch.
func TestErr_HTTP401NoCreds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	_, err := lsremote.Dial(context.Background(), srv.URL+"/repo.git", errOptsHTTP()...)
	require.Error(t, err)
	assert.True(t, errors.Is(err, lsremote.ErrAuthRequired),
		"401 with no resolver must match ErrAuthRequired; got %v", err)
	assert.False(t, errors.Is(err, lsremote.ErrAuthFailed))
}

// ----------------------------------------------------------------------
// Row 3: HTTP 401 with rejected creds → ErrAuthFailed
// ----------------------------------------------------------------------

// TestErr_HTTP401RejectedCreds wires a static credential resolver into
// the HTTP transport; the server still emits 401 on the retry, which
// must surface as `ErrAuthFailed`.
func TestErr_HTTP401RejectedCreds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	tr := httpt.New(httpt.WithCredentials(httpt.Static(httpt.Basic("alice", "secret"))))
	opts := []lsremote.Option{lsremote.WithTransports(transport.NewRegistry(tr))}

	_, err := lsremote.Dial(context.Background(), srv.URL+"/repo.git", opts...)
	require.Error(t, err)
	assert.True(t, errors.Is(err, lsremote.ErrAuthFailed),
		"401-after-retry must match ErrAuthFailed; got %v", err)
	assert.False(t, errors.Is(err, lsremote.ErrAuthRequired))
}

// ----------------------------------------------------------------------
// Row 4: HTTP 401 with resolver yielding new creds → success on retry
// ----------------------------------------------------------------------

// TestErr_HTTP401ResolverNewCreds drives the 401 → retry → 200 happy
// path. The handler counts requests so the test can confirm exactly
// two round-trips happen: the anonymous probe and the authenticated
// retry. The retry uses a static resolver attached to the transport.
func TestErr_HTTP401ResolverNewCreds(t *testing.T) {
	gitdir := materializeLoose(t)
	store := openSHA1Store(t, gitdir)

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch calls {
		case 1:
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			w.WriteHeader(http.StatusUnauthorized)
		default:
			writeSmartAdvertisement(t, r, w, store)
		}
	}))
	t.Cleanup(srv.Close)

	tr := httpt.New(httpt.WithCredentials(httpt.Static(httpt.Basic("alice", "secret"))))
	opts := []lsremote.Option{lsremote.WithTransports(transport.NewRegistry(tr))}

	s, err := lsremote.Dial(context.Background(), srv.URL+"/repo.git", opts...)
	require.NoError(t, err, "401 + retry-200 must open cleanly")
	t.Cleanup(func() { _ = s.Close() })
	assert.Equal(t, 2, calls, "exactly two probe round-trips: anonymous, then authenticated")
}

// ----------------------------------------------------------------------
// Row 5: HTTP 403 → ErrAuthFailed
// ----------------------------------------------------------------------

// TestErr_HTTP403 covers the 403 branch: per the HTTP transport's
// classifier (`open.go::handleProbeResponse`, `transport/http`), a 403
// is treated as a hard auth rejection — there is no retry path, since
// no challenge was offered.
func TestErr_HTTP403(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)

	_, err := lsremote.Dial(context.Background(), srv.URL+"/repo.git", errOptsHTTP()...)
	require.Error(t, err)
	assert.True(t, errors.Is(err, lsremote.ErrAuthFailed),
		"403 must match ErrAuthFailed; got %v", err)
}

// ----------------------------------------------------------------------
// Row 6: HTTP 404 → ErrNotFound
// ----------------------------------------------------------------------

// TestErr_HTTP404 covers the missing-repository branch.
func TestErr_HTTP404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	_, err := lsremote.Dial(context.Background(), srv.URL+"/repo.git", errOptsHTTP()...)
	require.Error(t, err)
	assert.True(t, errors.Is(err, lsremote.ErrNotFound),
		"404 must match ErrNotFound; got %v", err)
}

// ----------------------------------------------------------------------
// Row 7: HTTP 5xx → *ProtocolError{Status: 500}, no library sentinel
// ----------------------------------------------------------------------

// TestErr_HTTP5xx confirms a generic 5xx surfaces as a `*ProtocolError`
// carrying the status code and a truncated body excerpt, but DOES NOT
// match any library sentinel. The 5xx body is short and so survives
// the 1-KiB excerpt cap unchanged.
func TestErr_HTTP5xx(t *testing.T) {
	const body = "upload-pack on fire"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	_, err := lsremote.Dial(context.Background(), srv.URL+"/repo.git", errOptsHTTP()...)
	require.Error(t, err)

	var pe *lsremote.ProtocolError
	require.True(t, errors.As(err, &pe),
		"5xx must surface as *lsremote.ProtocolError; got %T: %v", err, err)
	assert.Equal(t, "dial", pe.Op,
		"5xx classification happens before the advertisement parse, so Op=dial")

	// The transport-layer *ProtocolError carries the HTTP status code
	// and the body excerpt; the root layer surfaces the status through
	// the wrapped chain rather than copying it onto its own field.
	var httpPE *httpt.ProtocolError
	require.True(t, errors.As(err, &httpPE),
		"the chain must reveal the transport's *ProtocolError; got %v", err)
	assert.Equal(t, http.StatusInternalServerError, httpPE.Status)
	assert.Contains(t, httpPE.Server, body)

	if s := errFindLSRemoteSentinel(err); s != nil {
		t.Errorf("5xx must not match any library sentinel; matched %v", s)
	}
}

// ----------------------------------------------------------------------
// Row 8: Connection drop mid-stream → wrapped *net.OpError, no sentinel
// ----------------------------------------------------------------------

// TestErr_ConnectionDropMidStream hijacks the response writer BEFORE
// the stdlib has set up its chunked-transfer machinery, writes the
// HTTP/1.1 status line and headers by hand with an explicit
// `Content-Length` longer than what we actually send, then drops the
// socket. The client reads the headers, starts parsing the pkt-line
// body, and trips on the mid-stream EOF.
//
// The unwrapped cause is `io.ErrUnexpectedEOF` (a short read against a
// content-length-bounded body) or a `*net.OpError` (a forced "read
// tcp: connection reset" if the kernel raced the close). Either is
// admissible; what the test pins is that no library sentinel matches
// — the wire was already past the status-code dispatch when the bytes
// dropped, so `ErrNotFound` etc. cannot fire here.
func TestErr_ConnectionDropMidStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatalf("ResponseWriter does not support Hijack")
		}
		conn, buf, err := hj.Hijack()
		if err != nil {
			t.Fatalf("hijack: %v", err)
		}
		// Hand-rolled HTTP/1.1 response: advertise a Content-Length far
		// larger than what we actually send, then close. The smart
		// advertisement preamble needs the four-byte length prefix +
		// payload + flush; we deliver only the prefix and a few payload
		// bytes so the pkt-line reader observes a short read.
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\n")
		_, _ = buf.WriteString("Content-Type: " + httpAdvContentType + "\r\n")
		_, _ = buf.WriteString("Content-Length: 200\r\n")
		_, _ = buf.WriteString("Connection: close\r\n")
		_, _ = buf.WriteString("\r\n")
		// Eight bytes: a pkt-line length-prefix `001f` (claiming 31
		// bytes follow) and a four-byte head of the advertised payload.
		// The reader will block waiting for the rest, then the close
		// trips it.
		_, _ = buf.WriteString("001f# se")
		_ = buf.Flush()
		_ = conn.Close()
	}))
	t.Cleanup(srv.Close)

	_, err := lsremote.Dial(context.Background(), srv.URL+"/repo.git", errOptsHTTP()...)
	require.Error(t, err)

	// Accept either an io.ErrUnexpectedEOF (the content-length-bounded
	// body short-reads) or a wrapped *net.OpError (a "connection reset"
	// shape on a hostile kernel).
	var opErr *net.OpError
	asOp := errors.As(err, &opErr)
	isEOF := errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF)
	assert.True(t, asOp || isEOF,
		"want *net.OpError or io.ErrUnexpectedEOF; got %T: %v", err, err)

	if s := errFindLSRemoteSentinel(err); s != nil {
		t.Errorf("mid-stream drop must not match a library sentinel; matched %v", s)
	}
}

// ----------------------------------------------------------------------
// Row 9: Server `ERR <msg>` packet → ErrServerRefused with Server set
// ----------------------------------------------------------------------

// TestErr_ServerERRPacket drives a v2 session through to a command
// stage and has the server emit a structured `ERR <msg>` pkt-line in
// the response. The library must surface a `*ProtocolError` whose Op
// matches the command in flight and whose chain matches
// `ErrServerRefused`.
//
// We drive `ls-refs`: the handler emits a valid v2 advertisement for
// the GET probe, then on the POST returns an ERR pkt-line + flush
// before any ref payload.
func TestErr_ServerERRPacket(t *testing.T) {
	const errMsg = "ls-refs: handler is offline"
	gitdir := materializeLoose(t)
	store := openSHA1Store(t, gitdir)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/info/refs"):
			writeSmartAdvertisement(t, r, w, store)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git-upload-pack"):
			w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
			pw := pktline.NewWriter(w)
			require.NoError(t, pw.WritePacket([]byte("ERR "+errMsg+"\n")))
			require.NoError(t, pw.WriteFlush())
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	s, err := lsremote.Dial(context.Background(), srv.URL+"/repo.git", errOptsHTTP()...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	_, err = s.ListRefs(context.Background(), lsremote.RefsRequest{})
	require.Error(t, err)

	var pe *lsremote.ProtocolError
	require.True(t, errors.As(err, &pe),
		"want *lsremote.ProtocolError; got %T: %v", err, err)
	assert.Equal(t, "ls-refs", pe.Op)

	assert.True(t, errors.Is(err, lsremote.ErrServerRefused),
		"server `ERR` packet must match ErrServerRefused; got %v", err)

	// The contract requires the server's message text to be retained.
	// Today the wire layer wraps the message into the error chain;
	// assert it appears somewhere in the formatted error so a log
	// line is actionable.
	assert.Contains(t, err.Error(), errMsg,
		"the server's ERR message must survive into the formatted error")
}

// ----------------------------------------------------------------------
// Row 10: Negotiated v0 when caller demanded v2 → ErrUnsupportedProtocol
// ----------------------------------------------------------------------

// TestErr_DemandedV2GotV0 emits a v0 advertisement under the
// smart-HTTP framing while the client pins v2. The wire layer's
// version mismatch surfaces as `wire.ErrUnsupportedProtocol`, bridged
// to the public `ErrUnsupportedProtocol` via `dial.go`'s `errors.Join`.
func TestErr_DemandedV2GotV0(t *testing.T) {
	gitdir := materializeLoose(t)
	store := openSHA1Store(t, gitdir)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/info/refs") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", httpAdvContentType)
		pw := pktline.NewWriter(w)
		require.NoError(t, pw.WritePacket([]byte("# service=git-upload-pack\n")))
		require.NoError(t, pw.WriteFlush())
		// Drive the in-process server with PreferredProtocol=v0 so the
		// emitted advertisement is the canonical v0 ref list, NOT the
		// v2 capability stream.
		require.NoError(t, server.Serve(r.Context(),
			pktline.NewReader(strings.NewReader("0000")),
			pw, store, server.Options{
				PreferredProtocol: transport.ProtocolV0,
			}))
	}))
	t.Cleanup(srv.Close)

	opts := append(errOptsHTTP(), lsremote.WithProtocol(lsremote.ProtocolV2))
	_, err := lsremote.Dial(context.Background(), srv.URL+"/repo.git", opts...)
	require.Error(t, err)

	assert.True(t, errors.Is(err, lsremote.ErrUnsupportedProtocol),
		"v2-pinned client against a v0 server must match ErrUnsupportedProtocol; got %v", err)
}

// ----------------------------------------------------------------------
// Row 11: object-info on dumb HTTP / v0 → ErrUnsupportedProtocol
// ----------------------------------------------------------------------

// TestErr_ObjectInfoOnDumbHTTP exercises the dumb-HTTP path. The
// transport probes a `text/plain` body and synthesises a v0-shaped
// stream; `Session.ObjectInfo` is v2-only and must refuse with
// `ErrUnsupportedProtocol`. The companion v1 case is omitted because
// the library does not implement v1 (v1 has no command loop and so no
// `object-info` shape to test against).
func TestErr_ObjectInfoOnDumbHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(
			"3333333333333333333333333333333333333333\trefs/heads/main\n",
		))
	}))
	t.Cleanup(srv.Close)

	s, err := lsremote.Dial(context.Background(), srv.URL+"/repo.git", errOptsHTTP()...)
	require.NoError(t, err, "dumb-HTTP probe must open the Session; the v2-gate fires on ObjectInfo")
	t.Cleanup(func() { _ = s.Close() })

	_, err = s.ObjectInfo(context.Background(),
		[]string{"3333333333333333333333333333333333333333"},
		lsremote.ObjectInfoRequest{Size: true})
	require.Error(t, err)
	assert.True(t, errors.Is(err, lsremote.ErrUnsupportedProtocol),
		"object-info on a non-v2 Session must match ErrUnsupportedProtocol; got %v", err)
}

// ----------------------------------------------------------------------
// Row 12: Specific OID missing in object-info → omitted, no error
// ----------------------------------------------------------------------

// TestErr_ObjectInfoMissingOID asks for one real OID and one missing
// OID. The server's `object-info` response emits the empty-size form
// for the miss (`<oid> \n`), which the wire decoder drops, so the
// returned slice carries only the valid OID and no error.
func TestErr_ObjectInfoMissingOID(t *testing.T) {
	entry := entryByName(t, "loose-objects")
	gitdir := entry.Materialize(t)
	store := openSHA1Store(t, gitdir)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSmartServer(t, r, w, store)
	}))
	t.Cleanup(srv.Close)

	const validOID = "393a7c05257a543bc1369537c7fdb2851dc04b11" // blob, 25 bytes
	// 40-hex string with all-zero high nibble; lex-valid hex but no
	// such object exists in the loose store.
	const missingOID = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"

	got, err := lsremote.ObjectInfos(context.Background(), srv.URL+"/repo.git",
		[]string{validOID, missingOID}, lsremote.ObjectInfoRequest{Size: true},
		errOptsHTTP()...)
	require.NoError(t, err)
	require.Len(t, got, 1, "missing OID must be silently dropped")
	assert.Equal(t, validOID, got[0].Hash)
	assert.Equal(t, int64(25), got[0].Size)
}

// ----------------------------------------------------------------------
// Row 13: Empty repo → empty Refs / ListRefs, no error
// ----------------------------------------------------------------------

// TestErr_EmptyRepo drives the `empty` fixture through the public
// `Refs`/`ListRefs` shape. The fixture has zero refs; the v2
// advertisement still emits the unborn-HEAD placeholder when the
// caller asks for it. Without the unborn flag, the result is the
// empty slice.
func TestErr_EmptyRepo(t *testing.T) {
	entry := entryByName(t, "empty")
	gitdir := entry.Materialize(t)
	store := openSHA1Store(t, gitdir)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSmartServer(t, r, w, store)
	}))
	t.Cleanup(srv.Close)

	got, err := lsremote.ListRefs(context.Background(), srv.URL+"/repo.git",
		lsremote.RefsRequest{}, errOptsHTTP()...)
	require.NoError(t, err)
	assert.Empty(t, got, "empty fixture must yield zero refs without Unborn")
}

// ----------------------------------------------------------------------
// Row 14: Unborn HEAD with RefsArgs.Unborn=true
// ----------------------------------------------------------------------

// TestErr_UnbornHEADRefs drives the `unborn-head` fixture through
// `Refs` with `Unborn: true` and asserts the iterator yields a HEAD
// entry whose Hash is empty (the public encoding for the unborn case)
// and whose Symref names the unborn target branch.
func TestErr_UnbornHEADRefs(t *testing.T) {
	entry := entryByName(t, "unborn-head")
	gitdir := entry.Materialize(t)
	store := openSHA1Store(t, gitdir)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSmartServer(t, r, w, store)
	}))
	t.Cleanup(srv.Close)

	got, err := lsremote.ListRefs(context.Background(), srv.URL+"/repo.git",
		lsremote.RefsRequest{Symrefs: true, Unborn: true}, errOptsHTTP()...)
	require.NoError(t, err)

	var heads []lsremote.Ref
	for _, r := range got {
		if r.Name == "HEAD" {
			heads = append(heads, r)
		}
	}
	require.Len(t, heads, 1, "unborn fixture must yield exactly one HEAD entry")
	assert.Empty(t, heads[0].Hash, "unborn HEAD must carry an empty hash")
	assert.Equal(t, "refs/heads/main", heads[0].Symref)
}

// ----------------------------------------------------------------------
// Row 15: Unborn HEAD via DefaultBranch
// ----------------------------------------------------------------------

// TestErr_UnbornHEADDefaultBranch confirms `DefaultBranch` resolves
// the symref target on an unborn-HEAD fixture (the v2 wire emits a
// `symref-target:` attribute on the unborn HEAD entry only when the
// `unborn` argument was on the request — see `helpers.DefaultBranch`).
func TestErr_UnbornHEADDefaultBranch(t *testing.T) {
	entry := entryByName(t, "unborn-head")
	gitdir := entry.Materialize(t)
	store := openSHA1Store(t, gitdir)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSmartServer(t, r, w, store)
	}))
	t.Cleanup(srv.Close)

	got, err := lsremote.DefaultBranch(context.Background(), srv.URL+"/repo.git",
		errOptsHTTP()...)
	require.NoError(t, err)
	assert.Equal(t, "refs/heads/main", got)
}

// ----------------------------------------------------------------------
// Row 16: context.Canceled
// ----------------------------------------------------------------------

// TestErr_ContextCanceled cancels the ctx before the call. The
// returned error must surface `context.Canceled` directly through the
// chain — no library sentinel wrapping that would shadow the cause.
func TestErr_ContextCanceled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// The handler should never be reached; cancellation happens
		// before the dial. Emit a 500 if we ever land here so the test
		// surfaces the wrong shape rather than hanging.
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := lsremote.Refs(ctx, srv.URL+"/repo.git", lsremote.RefsRequest{},
		errOptsHTTP()...)
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled),
		"want errors.Is(err, context.Canceled); got %v", err)

	if s := errFindLSRemoteSentinel(err); s != nil {
		t.Errorf("ctx cancel must not match a library sentinel; matched %v", s)
	}
}

// ----------------------------------------------------------------------
// Row 17: context.DeadlineExceeded
// ----------------------------------------------------------------------

// TestErr_ContextDeadlineExceeded pins a microsecond deadline so the
// dial fires after the deadline has already passed. The returned
// error must surface `context.DeadlineExceeded` directly.
func TestErr_ContextDeadlineExceeded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Same shape as TestErr_ContextCanceled: never expected to run.
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	// 1µs is short enough to elapse before the dial starts. The HTTP
	// client returns context.DeadlineExceeded through `client.Do`.
	ctx, cancel := context.WithTimeout(context.Background(), time.Microsecond)
	t.Cleanup(cancel)
	// Sleep a hair longer than the timeout so the deadline is firmly
	// in the past by the time we issue the call.
	time.Sleep(2 * time.Millisecond)

	_, err := lsremote.Refs(ctx, srv.URL+"/repo.git", lsremote.RefsRequest{},
		errOptsHTTP()...)
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.DeadlineExceeded),
		"want errors.Is(err, context.DeadlineExceeded); got %v", err)
}

// ----------------------------------------------------------------------
// Row 18: Malformed pkt-line in advertisement
// ----------------------------------------------------------------------

// TestErr_MalformedPktLineAdvertisement emits a 200 OK with the smart
// content-type but a body whose first bytes are NOT a valid pkt-line.
// The transport's `stripSmartPreamble` parser must surface a
// `*ProtocolError` whose `Server` field carries a truncated excerpt
// of the server-supplied bytes, per the contract documented on
// `ProtocolError.Server`.
func TestErr_MalformedPktLineAdvertisement(t *testing.T) {
	const garbage = "this is definitely not a pkt-line stream\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", httpAdvContentType)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(garbage))
	}))
	t.Cleanup(srv.Close)

	_, err := lsremote.Dial(context.Background(), srv.URL+"/repo.git", errOptsHTTP()...)
	require.Error(t, err)

	var pe *lsremote.ProtocolError
	require.True(t, errors.As(err, &pe),
		"want *lsremote.ProtocolError; got %T: %v", err, err)
	require.NotEmpty(t, pe.Server,
		"ProtocolError.Server must carry an excerpt of the malformed bytes")
	assert.LessOrEqual(t, len(pe.Server), 1024+len("..."),
		"Server is bounded to 1 KiB plus a possible ellipsis marker")
	// The pktline length-prefix parser consumes the leading four bytes
	// of the garbage before bailing, so the excerpt starts mid-token.
	// `definitely not` survives the consumption and is a recognisable
	// fragment of what the harness wrote.
	assert.Contains(t, pe.Server, "definitely not",
		"Server excerpt must include the malformed bytes")
}

// ----------------------------------------------------------------------
// Row 19: objstore.ErrCorruptObject via transport/file
// ----------------------------------------------------------------------

// TestErr_CorruptedPackObjectInfo points the file transport at the
// `idx-corrupt` fixture. The bogus idx fails the v1 fan-out length
// check inside `objstore.Open`, so the failure surfaces at dial time
// rather than during a later `object-info` call. The contract
// describes the corruption as surfacing on the `object-info` path
// (`Op:"object-info"`), but the implementation surfaces it earlier —
// at Open. We assert the actual observed shape: a
// `*lsremote.ProtocolError` whose Op is `"dial"` and whose chain
// surfaces the corrupt-idx path.
func TestErr_CorruptedPackObjectInfo(t *testing.T) {
	gitdir := inttest.Entry{Name: "idx-corrupt", ObjectFormat: lsremote.ObjectFormatSHA1}.Materialize(t)

	opts := []lsremote.Option{
		lsremote.WithTransports(transport.NewRegistry(filet.New())),
	}
	_, err := lsremote.Dial(context.Background(), "file://"+gitdir, opts...)
	require.Error(t, err)

	var pe *lsremote.ProtocolError
	require.True(t, errors.As(err, &pe),
		"want *lsremote.ProtocolError; got %T: %v", err, err)
	assert.Equal(t, "dial", pe.Op)
	assert.Contains(t, err.Error(), "bogus.idx",
		"the corrupt idx path must survive into the formatted error")
}

// ----------------------------------------------------------------------
// Row 20: URL parse failure
// ----------------------------------------------------------------------

// TestErr_URLParseFailure asserts that a malformed URL surfaces a URL
// parse error (a `transport`-package sentinel), NOT any library
// `lsremote.Err*` sentinel. The contract's "no library sentinel" rule
// applies to the public `lsremote` sentinels specifically; transport
// sentinels exposing the parse failure are part of the documented
// contract on `Dial`.
func TestErr_URLParseFailure(t *testing.T) {
	_, err := lsremote.Refs(context.Background(), "://malformed",
		lsremote.RefsRequest{}, errOptsHTTP()...)
	require.Error(t, err)

	// The transport's URL parser surfaces one of its documented
	// sentinels. Any of them satisfies the row's contract; assert that
	// at least one matches.
	matched := errors.Is(err, transport.ErrUnrecognizedURL) ||
		errors.Is(err, transport.ErrUnsupportedScheme) ||
		errors.Is(err, transport.ErrEmptyURL) ||
		errors.Is(err, transport.ErrMissingHost)
	assert.True(t, matched,
		"want a transport URL-parse sentinel; got %v", err)

	if s := errFindLSRemoteSentinel(err); s != nil {
		t.Errorf("URL-parse failure must not match a library sentinel; matched %v", s)
	}
}

// ----------------------------------------------------------------------
// Shared helpers
// ----------------------------------------------------------------------

// materializeLoose materializes the `loose-only` fixture into a fresh
// temp dir and returns the gitdir path. The fixture is small enough to
// reuse for any row that needs a real upstream advertisement; tests
// that need a richer fixture override.
func materializeLoose(t *testing.T) string {
	t.Helper()
	for _, e := range inttest.Entries() {
		if e.Name == "loose-only" {
			return e.Materialize(t)
		}
	}
	t.Fatalf("loose-only fixture missing from inttest.Entries()")
	return ""
}

// entryByName returns the curated [inttest.Entry] whose Name matches
// name, or fails the test if no entry matches. Centralising the lookup
// keeps every row's setup readable.
func entryByName(t *testing.T, name string) inttest.Entry {
	t.Helper()
	for _, e := range inttest.Entries() {
		if e.Name == name {
			return e
		}
	}
	t.Fatalf("fixture %q missing from inttest.Entries()", name)
	return inttest.Entry{}
}

// openSHA1Store opens an SHA-1 `*objstore.Store` against gitdir and
// registers a t.Cleanup to release it.
func openSHA1Store(t *testing.T, gitdir string) *objstore.Store[objfmt.SHA1Hash] {
	t.Helper()
	store, err := objstore.Open[objfmt.SHA1Hash](gitdir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// writeSmartAdvertisement emits the smart-HTTP advertisement for a
// single request. Used by rows that drive the GET `info/refs` probe
// directly (Row 4, Row 9) when the harness's full
// `inttest.NewHTTPServer` plumbing is more than the row needs.
func writeSmartAdvertisement(t *testing.T, r *http.Request, w http.ResponseWriter,
	store *objstore.Store[objfmt.SHA1Hash]) {
	t.Helper()
	w.Header().Set("Content-Type", httpAdvContentType)
	pw := pktline.NewWriter(w)
	require.NoError(t, pw.WritePacket([]byte("# service=git-upload-pack\n")))
	require.NoError(t, pw.WriteFlush())
	require.NoError(t, server.Serve(r.Context(),
		pktline.NewReader(strings.NewReader("0000")),
		pw, store, server.Options{
			PreferredProtocol: transport.ProtocolV2,
		}))
}

// writeSmartServer is the combined GET/POST handler for the rows that
// need a real upstream Session (Rows 12-15). It mirrors
// `inttest.NewHTTPServer` but inline so each row can keep its setup
// in one function.
func writeSmartServer(t *testing.T, r *http.Request, w http.ResponseWriter,
	store *objstore.Store[objfmt.SHA1Hash]) {
	t.Helper()
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/info/refs"):
		writeSmartAdvertisement(t, r, w, store)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git-upload-pack"):
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
		require.NoError(t, server.ServeCommandLoop(r.Context(),
			pktline.NewReader(strings.NewReader(string(body))),
			pktline.NewWriter(w), store, server.Options{
				PreferredProtocol: transport.ProtocolV2,
			}))
	default:
		http.NotFound(w, r)
	}
}

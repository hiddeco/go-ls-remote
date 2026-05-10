package lsremote

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
	"github.com/hiddeco/go-ls-remote/internal/objstore"
	"github.com/hiddeco/go-ls-remote/internal/server"
	"github.com/hiddeco/go-ls-remote/internal/testfixture"
	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/trace"
	"github.com/hiddeco/go-ls-remote/transport"
	filet "github.com/hiddeco/go-ls-remote/transport/file"
	httpt "github.com/hiddeco/go-ls-remote/transport/http"
)

// smartAdvHeader is the content type a real smart-HTTP server replies
// with on the discovery probe — `application/x-git-upload-pack-advertisement`.
// The HTTP transport keys its smart/dumb dispatch off this value.
const smartAdvHeader = "application/x-git-upload-pack-advertisement"

// commandResultHeader is the content type a real smart-HTTP server
// replies with on the v2 command POST — `application/x-git-upload-pack-result`.
const commandResultHeader = "application/x-git-upload-pack-result"

// openFixtureStore materialises the named fixture from `testdata/repos/`
// and returns an opened `[objstore.Store]` over it. The
// `objects/pack/` directory is created on demand because some ref-only
// fixtures ship without one — mirroring `transport/http/command_test.go`.
func openFixtureStore(t *testing.T, name string) *objstore.Store[objfmt.SHA1Hash] {
	t.Helper()
	gitdir := testfixture.MaterializeRepo(t, name)
	require.NoError(t, os.MkdirAll(filepath.Join(gitdir, "objects", "pack"), 0o755))
	store, err := objstore.Open[objfmt.SHA1Hash](gitdir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// serveHandlerV2 returns an `http.Handler` that emulates a v2-speaking
// smart-HTTP server backed by the materialised fixture store. The shape
// mirrors `transport/http/command_test.go::serveHandler`: a GET on
// `<repoPath>/info/refs` returns the `# service=git-upload-pack`
// preamble plus the v2 advertisement, and a POST on
// `<repoPath>/git-upload-pack` runs the v2 command loop.
func serveHandlerV2(t *testing.T, store *objstore.Store[objfmt.SHA1Hash], repoPath string) http.Handler {
	t.Helper()
	infoRefsPath := repoPath + "/info/refs"
	uploadPackPath := repoPath + "/git-upload-pack"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == infoRefsPath:
			w.Header().Set("Content-Type", smartAdvHeader)
			pw := pktline.NewWriter(w)
			require.NoError(t, pw.WritePacket([]byte("# service=git-upload-pack\n")))
			require.NoError(t, pw.WriteFlush())
			err := server.Serve(r.Context(),
				pktline.NewReader(bytes.NewReader([]byte("0000"))),
				pw, store, server.Options{
					Agent:             "test-server/0.0",
					PreferredProtocol: transport.ProtocolV2,
				})
			require.NoError(t, err)
		case r.Method == http.MethodPost && r.URL.Path == uploadPackPath:
			body, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			w.Header().Set("Content-Type", commandResultHeader)
			require.NoError(t, server.Serve(r.Context(),
				pktline.NewReader(bytes.NewReader(body)),
				pktline.NewWriter(w), store,
				server.Options{
					Agent:             "test-server/0.0",
					PreferredProtocol: transport.ProtocolV2,
				}))
		default:
			http.NotFound(w, r)
		}
	})
}

// serveHandlerV0 returns an `http.Handler` that emulates a v0 server.
// Only the discovery probe is implemented; v0 has no command loop, so
// the POST endpoint is unreachable from the Dial flow.
func serveHandlerV0(t *testing.T, store *objstore.Store[objfmt.SHA1Hash], repoPath string) http.Handler {
	t.Helper()
	infoRefsPath := repoPath + "/info/refs"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == infoRefsPath {
			w.Header().Set("Content-Type", smartAdvHeader)
			pw := pktline.NewWriter(w)
			require.NoError(t, pw.WritePacket([]byte("# service=git-upload-pack\n")))
			require.NoError(t, pw.WriteFlush())
			require.NoError(t, server.Serve(r.Context(),
				pktline.NewReader(bytes.NewReader([]byte("0000"))),
				pw, store, server.Options{
					Agent:             "test-server/0.0",
					PreferredProtocol: transport.ProtocolV0,
				}))
			return
		}
		http.NotFound(w, r)
	})
}

// recordingTracerEvents is a `trace.Tracer` that records every event
// for later assertions; we only need pointer identity for the
// option-passthrough test, but a real Tracer makes the intent clear.
type recordingTracerEvents struct {
	events []trace.Event
}

func (r *recordingTracerEvents) OnEvent(e trace.Event) { r.events = append(r.events, e) }

// captureTransport is a stub `transport.Transport` that records the
// `transport.OpenOptions` it was called with so a test can verify the
// dial-config plumbing forwards `tracer`, `userAgent` and `protocol`
// verbatim. It returns `conn`/`err` on Open without doing any I/O.
type captureTransport struct {
	schemes []string
	gotOpts transport.OpenOptions
	gotURL  *transport.URL
	conn    transport.Conn
	err     error
}

func (c *captureTransport) Schemes() []string { return c.schemes }

func (c *captureTransport) Open(_ context.Context, u *transport.URL, opts transport.OpenOptions) (transport.Conn, error) {
	c.gotOpts = opts
	c.gotURL = u
	return c.conn, c.err
}

// stubConn satisfies `transport.Conn` with a caller-supplied
// `pktline.Reader` for its advertisement. `Command` and `Close` are
// inert no-ops so a test can keep a returned `*Session` alive across
// assertions without worrying about resource cleanup.
type stubConn struct {
	adv     *pktline.Reader
	closed  bool
	closeFn func() error
}

func (s *stubConn) Advertisement() *pktline.Reader { return s.adv }
func (s *stubConn) Command(context.Context, string, []string, []string) (*pktline.Reader, error) {
	return nil, errors.New("stubConn: Command not implemented")
}
func (s *stubConn) Close() error {
	s.closed = true
	if s.closeFn != nil {
		return s.closeFn()
	}
	return nil
}

// buildV2Advertisement returns a byte slice carrying a minimal v2
// capability advertisement so a `stubConn` can hand a positioned
// `pktline.Reader` back to `ParseAdvertisement`. The shape mirrors the
// emitter in `internal/server/advertise.go`:
//
//	pkt:  "version 2\n"
//	pkt:  "agent=test/0\n"
//	pkt:  "object-format=sha1\n"
//	pkt:  "ls-refs=unborn\n"
//	pkt:  "fetch\n"
//	pkt:  "object-info\n"
//	flush
func buildV2Advertisement(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	pw := pktline.NewWriter(&b)
	for _, line := range []string{
		"version 2\n",
		"agent=test/0\n",
		"object-format=sha1\n",
		"ls-refs=unborn\n",
		"fetch\n",
		"object-info\n",
	} {
		require.NoError(t, pw.WritePacket([]byte(line)))
	}
	require.NoError(t, pw.WriteFlush())
	return b.Bytes()
}

// TestDial_invalidURL pins the `transport.ParseURL` failure mode: a
// URL that cannot be parsed must surface a parse error that does NOT
// match `lsremote.ErrUnsupportedProtocol` (which is reserved for
// missing-transport and negotiated-wrong-version conditions). The
// error must propagate from `transport.ParseURL`, not be wrapped in a
// `*ProtocolError` — the connection never reached the wire.
func TestDial_invalidURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
	}{
		{name: "empty", url: ""},
		{name: "unsupported scheme", url: "gopher://example.com/repo"},
		{name: "unrecognised form", url: "not a url at all"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, err := Dial(context.Background(), tc.url)
			assert.Nil(t, s)
			require.Error(t, err)

			var pe *ProtocolError
			assert.False(t, errors.As(err, &pe),
				"URL parse failures must not surface as *ProtocolError; got %T", err)
			assert.False(t, errors.Is(err, ErrUnsupportedProtocol),
				"URL parse failures must not match ErrUnsupportedProtocol; got %v", err)
		})
	}
}

// TestDial_unsupportedScheme pins the no-registered-transport branch:
// a syntactically valid URL whose scheme has no entry in the registry
// must surface a `*ProtocolError` with `Op == "dial"`, `Status == 0`,
// the redacted URL stored, and `errors.Is(err, ErrUnsupportedProtocol)`
// true.
func TestDial_unsupportedScheme(t *testing.T) {
	// `ssh://` parses but is not in the default HTTP-only registry.
	s, err := Dial(context.Background(), "ssh://user@example.com/repo.git")
	assert.Nil(t, s)
	require.Error(t, err)

	assert.True(t, errors.Is(err, ErrUnsupportedProtocol),
		"missing-transport must surface as ErrUnsupportedProtocol; got %v", err)

	var pe *ProtocolError
	require.True(t, errors.As(err, &pe),
		"missing-transport must surface as *ProtocolError; got %T", err)
	assert.Equal(t, "dial", pe.Op)
	assert.Equal(t, 0, pe.Status,
		"no HTTP context yet, so Status must be zero")
	assert.Equal(t, "ssh://user@example.com/repo.git", pe.URL,
		"the redacted URL must be stored verbatim (no password to redact)")
	assert.NotEmpty(t, pe.Server,
		"Server must carry a short message naming the missing scheme")
}

// TestDial_unsupportedScheme_redactsPassword pins the URL-redaction
// contract on the unsupported-scheme path: a `user:password@` userinfo
// must be redacted before storage on `*ProtocolError.URL`.
func TestDial_unsupportedScheme_redactsPassword(t *testing.T) {
	_, err := Dial(context.Background(), "ssh://alice:secret@example.com/repo.git")
	require.Error(t, err)

	var pe *ProtocolError
	require.True(t, errors.As(err, &pe))
	assert.NotContains(t, pe.URL, "secret",
		"the password must be redacted before storage on *ProtocolError.URL")
	assert.Contains(t, pe.URL, "alice:***",
		"the userinfo must read alice:*** post-redaction")
}

// TestDial_advertisementHappyPath_V2 pins the success path against an
// in-process v2 server. The returned `*Session` must carry the
// negotiated capabilities and have `refs == nil` (v2 leaves the
// advertisement-time ref slice empty; callers issue `ls-refs`
// separately).
func TestDial_advertisementHappyPath_V2(t *testing.T) {
	store := openFixtureStore(t, "loose-only")
	srv := httptest.NewServer(serveHandlerV2(t, store, "/repo.git"))
	defer srv.Close()

	s, err := Dial(context.Background(), srv.URL+"/repo.git")
	require.NoError(t, err)
	require.NotNil(t, s)
	t.Cleanup(func() { _ = s.conn.Close() })

	assert.Equal(t, ProtocolV2, s.caps.Version,
		"the v2 advertisement must negotiate to ProtocolV2")
	assert.NotEmpty(t, s.caps.Agent,
		"the server's `agent=` capability must populate Capabilities.Agent")
	assert.Contains(t, s.caps.Commands, "ls-refs",
		"a v2 server must advertise `ls-refs`")
	assert.Contains(t, s.caps.Commands, "object-info",
		"the in-process server advertises `object-info`")
	assert.Nil(t, s.refs,
		"a v2 advertisement leaves Session.refs nil; refs are fetched on demand")

	assert.Equal(t, srv.URL+"/repo.git", s.url,
		"the redacted URL must be stored on the Session for later diagnostics")
}

// TestDial_advertisementHappyPath_V0 pins the v0 path: the server
// advertises refs inline on its capability list and the client
// populates `Session.refs`.
func TestDial_advertisementHappyPath_V0(t *testing.T) {
	store := openFixtureStore(t, "loose-only")
	srv := httptest.NewServer(serveHandlerV0(t, store, "/repo.git"))
	defer srv.Close()

	s, err := Dial(context.Background(), srv.URL+"/repo.git")
	require.NoError(t, err)
	require.NotNil(t, s)
	t.Cleanup(func() { _ = s.conn.Close() })

	assert.Equal(t, ProtocolV0, s.caps.Version,
		"the v0 advertisement must negotiate to ProtocolV0")
	assert.NotEmpty(t, s.refs,
		"a v0 advertisement carries refs inline; the loose-only fixture has HEAD and refs/heads/main")
}

// TestDial_versionPin_mismatch pins the negotiation-mismatch branch:
// when the server speaks v0 but the caller pinned v2 via
// `WithProtocol`, the wire layer rejects the advertisement with its
// own `wire.ErrUnsupportedProtocol`. Dial must surface a
// `*ProtocolError` with `Op == "advertisement"` whose error chain
// reaches the public `lsremote.ErrUnsupportedProtocol` via
// `errors.Is`.
func TestDial_versionPin_mismatch(t *testing.T) {
	store := openFixtureStore(t, "loose-only")
	srv := httptest.NewServer(serveHandlerV0(t, store, "/repo.git"))
	defer srv.Close()

	s, err := Dial(context.Background(), srv.URL+"/repo.git",
		WithProtocol(ProtocolV2))
	assert.Nil(t, s)
	require.Error(t, err)

	assert.True(t, errors.Is(err, ErrUnsupportedProtocol),
		"a version pin mismatch must reach ErrUnsupportedProtocol via errors.Is; got %v", err)

	var pe *ProtocolError
	require.True(t, errors.As(err, &pe),
		"a version pin mismatch must surface as *ProtocolError; got %T", err)
	assert.Equal(t, "advertisement", pe.Op,
		"the failure happened while parsing the advertisement, not while dialling")
}

// TestDial_options_passthrough pins that the dial config's tracer,
// user agent, and protocol pin reach the underlying `Transport.Open`
// verbatim. A `captureTransport` records the `OpenOptions` it was
// called with so we can assert each field round-trips through the
// dial layer.
func TestDial_options_passthrough(t *testing.T) {
	cap := &captureTransport{
		schemes: []string{"https"},
		conn: &stubConn{
			adv: pktline.NewReader(bytes.NewReader(buildV2Advertisement(t))),
		},
	}
	reg := transport.NewRegistry(cap)

	tr := &recordingTracerEvents{}
	pinned := ProtocolV2

	s, err := Dial(context.Background(), "https://example.com/repo.git",
		WithTransports(reg),
		WithTracer(tr),
		WithUserAgent("ua/1"),
		WithProtocol(pinned),
	)
	require.NoError(t, err)
	require.NotNil(t, s)

	assert.Equal(t, tr, cap.gotOpts.Tracer,
		"WithTracer must reach Transport.Open via OpenOptions.Tracer")
	assert.Equal(t, "ua/1", cap.gotOpts.UserAgent,
		"WithUserAgent must reach Transport.Open via OpenOptions.UserAgent")
	require.NotNil(t, cap.gotOpts.PreferredProtocol,
		"WithProtocol must set OpenOptions.PreferredProtocol to a non-nil pointer")
	assert.Equal(t, pinned, *cap.gotOpts.PreferredProtocol,
		"OpenOptions.PreferredProtocol must echo the pinned version")
	require.NotNil(t, cap.gotURL,
		"Dial must hand a parsed *transport.URL to the underlying Open")
	assert.Equal(t, "https", cap.gotURL.Scheme)
	assert.Equal(t, "example.com", cap.gotURL.Host)
	assert.Equal(t, "/repo.git", cap.gotURL.Path)
}

// TestDial_options_defaultsPassthrough pins the zero-value path: when
// neither `WithTracer`, `WithUserAgent`, nor `WithProtocol` is passed,
// `Transport.Open` sees the corresponding `OpenOptions` zero values
// (nil tracer, empty user-agent, nil preferred protocol).
func TestDial_options_defaultsPassthrough(t *testing.T) {
	cap := &captureTransport{
		schemes: []string{"https"},
		conn: &stubConn{
			adv: pktline.NewReader(bytes.NewReader(buildV2Advertisement(t))),
		},
	}
	reg := transport.NewRegistry(cap)

	_, err := Dial(context.Background(), "https://example.com/repo.git",
		WithTransports(reg))
	require.NoError(t, err)

	assert.Nil(t, cap.gotOpts.Tracer,
		"omitting WithTracer must leave OpenOptions.Tracer nil")
	assert.Empty(t, cap.gotOpts.UserAgent,
		"omitting WithUserAgent must leave OpenOptions.UserAgent empty")
	assert.Nil(t, cap.gotOpts.PreferredProtocol,
		"omitting WithProtocol must leave OpenOptions.PreferredProtocol nil")
}

// TestDial_defaultRegistryFallback pins that omitting `WithTransports`
// falls back to the package's HTTP-only default registry. The flip
// side — `WithTransports(custom)` routes through `custom` — is
// exercised by `TestDial_options_passthrough`.
func TestDial_defaultRegistryFallback(t *testing.T) {
	store := openFixtureStore(t, "loose-only")
	srv := httptest.NewServer(serveHandlerV2(t, store, "/repo.git"))
	defer srv.Close()

	s, err := Dial(context.Background(), srv.URL+"/repo.git")
	require.NoError(t, err, "without WithTransports, Dial must use the HTTP-only default registry")
	require.NotNil(t, s)
	t.Cleanup(func() { _ = s.conn.Close() })

	assert.Equal(t, ProtocolV2, s.caps.Version)
}

// TestDial_transportErrorPreservesSentinels pins the transport-layer
// error translation: when `Transport.Open` returns an error wrapping
// one of the HTTP transport's sentinels, the resulting
// `*lsremote.ProtocolError` must let `errors.Is` walk transitively to
// the equivalent `lsremote.Err*` sentinel.
//
// We exercise the 404 path: the in-process server returns a `404` on
// the discovery GET, the HTTP transport wraps that as
// `httpt.ErrNotFound`, and Dial wraps that in turn as a
// `*lsremote.ProtocolError`. `errors.Is(err, lsremote.ErrNotFound)`
// must succeed because the HTTP-layer sentinel re-wraps to the public
// one via its own error chain.
func TestDial_transportErrorPreservesSentinels(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repo.git/info/refs", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	s, err := Dial(context.Background(), srv.URL+"/repo.git")
	assert.Nil(t, s)
	require.Error(t, err)

	assert.True(t, errors.Is(err, ErrNotFound),
		"a 404 from the transport must reach lsremote.ErrNotFound via errors.Is; got %v", err)

	var pe *ProtocolError
	require.True(t, errors.As(err, &pe),
		"a transport-level failure must surface as *ProtocolError; got %T", err)
	assert.Equal(t, "dial", pe.Op,
		"the failure happened during the dial (transport open), not advertisement parsing")
}

// TestDial_transportOpenError_genericWrapping pins the generic
// transport-error wrap: when `Transport.Open` returns an error that
// matches none of the public sentinels, Dial must still wrap it as a
// `*ProtocolError` so callers see consistent surface area, and the
// wrap must preserve the underlying error via `Unwrap` so callers can
// inspect with `errors.As`.
func TestDial_transportOpenError_genericWrapping(t *testing.T) {
	sentinel := errors.New("captureTransport: synthetic open failure")
	cap := &captureTransport{
		schemes: []string{"https"},
		err:     sentinel,
	}
	reg := transport.NewRegistry(cap)

	s, err := Dial(context.Background(), "https://example.com/repo.git",
		WithTransports(reg))
	assert.Nil(t, s)
	require.Error(t, err)

	assert.True(t, errors.Is(err, sentinel),
		"the underlying transport error must remain reachable via errors.Is; got %v", err)

	var pe *ProtocolError
	require.True(t, errors.As(err, &pe))
	assert.Equal(t, "dial", pe.Op)
}

// TestDial_advertisementError_closesConn pins the cleanup contract on
// the advertisement-parse path: a malformed advertisement must cause
// Dial to close the underlying `transport.Conn` before returning, so
// the caller never sees a leaked half-open connection.
func TestDial_advertisementError_closesConn(t *testing.T) {
	closed := false
	// An empty pkt-line stream causes ParseAdvertisement to surface
	// `io.ErrUnexpectedEOF`, which is the cleanest way to drive the
	// advertisement-error branch without a transport-level failure.
	conn := &stubConn{
		adv:     pktline.NewReader(bytes.NewReader(nil)),
		closeFn: func() error { closed = true; return nil },
	}
	cap := &captureTransport{schemes: []string{"https"}, conn: conn}
	reg := transport.NewRegistry(cap)

	s, err := Dial(context.Background(), "https://example.com/repo.git",
		WithTransports(reg))
	assert.Nil(t, s)
	require.Error(t, err)
	assert.True(t, closed, "Dial must close the Conn when advertisement parsing fails")

	var pe *ProtocolError
	require.True(t, errors.As(err, &pe))
	assert.Equal(t, "advertisement", pe.Op)
}

// TestSession_zeroValue documents the opaque-struct contract: a
// zero-value `*Session` is not useful but must not panic when its
// unexported fields are inspected by tests in the same package. This
// is a sanity test, not a behavioural one.
func TestSession_zeroValue(t *testing.T) {
	var s Session
	assert.Nil(t, s.conn)
	assert.Empty(t, s.url)
	assert.Nil(t, s.refs)
	assert.Equal(t, Capabilities{}, s.caps)
}

// TestDial_bridgeOpenError_httpSentinels pins every branch of the
// `bridgeOpenError` switch against the HTTP transport's real sentinel
// values. Each `httpt.Err*` sentinel must bridge to the matching
// `lsremote.Err*` via `errors.Is`; the underlying transport sentinel
// must remain reachable on the joined chain so callers who want the
// scheme-specific identity (for example to read an HTTP status code
// off a wrapping `*httpt.ProtocolError`) still get there.
func TestDial_bridgeOpenError_httpSentinels(t *testing.T) {
	cases := []struct {
		name       string
		open       error
		wantPublic error
	}{
		{"ErrNotFound", httpt.ErrNotFound, ErrNotFound},
		{"ErrAuthRequired", httpt.ErrAuthRequired, ErrAuthRequired},
		{"ErrAuthFailed", httpt.ErrAuthFailed, ErrAuthFailed},
		{"ErrUnsupportedProtocol", httpt.ErrUnsupportedProtocol, ErrUnsupportedProtocol},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cap := &captureTransport{
				schemes: []string{"https"},
				err:     tc.open,
			}
			reg := transport.NewRegistry(cap)

			s, err := Dial(context.Background(), "https://example.com/repo.git",
				WithTransports(reg))
			assert.Nil(t, s)
			require.Error(t, err)

			assert.True(t, errors.Is(err, tc.wantPublic),
				"transport sentinel %v must bridge to %v via errors.Is; got %v",
				tc.open, tc.wantPublic, err)
			assert.True(t, errors.Is(err, tc.open),
				"the underlying transport sentinel must stay reachable on the joined chain; got %v", err)

			var pe *ProtocolError
			require.True(t, errors.As(err, &pe),
				"every bridged error must surface as *ProtocolError; got %T", err)
			assert.Equal(t, "dial", pe.Op)
		})
	}
}

// TestDial_bridgeOpenError_fileNotFound pins the regression the
// generic-sentinel bridge was introduced for: a `filet.ErrNotFound`
// returned by the real local-filesystem transport must reach
// `lsremote.ErrNotFound` via `errors.Is`. Before the bridge moved to
// generic identities the dial layer hard-coded `httpt.Err*` and this
// path failed silently.
func TestDial_bridgeOpenError_fileNotFound(t *testing.T) {
	dir := t.TempDir() // empty — not a Git repository
	reg := transport.NewRegistry(filet.New())

	s, err := Dial(context.Background(), "file://"+dir,
		WithTransports(reg))
	assert.Nil(t, s)
	require.Error(t, err)

	assert.True(t, errors.Is(err, ErrNotFound),
		"filet.ErrNotFound must bridge to lsremote.ErrNotFound via errors.Is; got %v", err)
	assert.True(t, errors.Is(err, filet.ErrNotFound),
		"the underlying filet sentinel must stay reachable on the joined chain; got %v", err)

	var pe *ProtocolError
	require.True(t, errors.As(err, &pe))
	assert.Equal(t, "dial", pe.Op)
}

// TestDial_bridgeOpenError_userTransport pins the open extension
// contract: a user-defined transport that declares its sentinel as a
// `*transport.SchemeError` bound to `transport.ErrNotFound` bridges
// into `lsremote.ErrNotFound` without any library change. This is
// what makes the bridge work for third-party transports.
func TestDial_bridgeOpenError_userTransport(t *testing.T) {
	userSentinel := &transport.SchemeError{
		Parent: transport.ErrNotFound,
		Msg:    "transport/ssh: repository not found",
	}
	cap := &captureTransport{
		schemes: []string{"ssh"},
		err:     userSentinel,
	}
	reg := transport.NewRegistry(cap)

	s, err := Dial(context.Background(), "ssh://user@example.com/repo.git",
		WithTransports(reg))
	assert.Nil(t, s)
	require.Error(t, err)

	assert.True(t, errors.Is(err, ErrNotFound),
		"a user-defined SchemeError must bridge through transport.ErrNotFound; got %v", err)
	assert.True(t, errors.Is(err, userSentinel),
		"the user-defined sentinel identity must stay reachable; got %v", err)
}

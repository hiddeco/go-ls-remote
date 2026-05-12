package lsremote

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hiddeco/go-ls-remote/internal/wire"
	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/transport"
	filet "github.com/hiddeco/go-ls-remote/transport/file"
	httpt "github.com/hiddeco/go-ls-remote/transport/http"
	ssht "github.com/hiddeco/go-ls-remote/transport/ssh"
)

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

// TestDial_transportOpenError_propagatesServerExcerpt pins the
// session-layer aggregation contract: when `Transport.Open` returns an
// `*httpt.ProtocolError` carrying a `Server` excerpt and HTTP status,
// Dial must lift both fields onto the outer `*lsremote.ProtocolError`
// it wraps the cause in. The library's public `ProtocolError.Server`
// is the documented surface for server-supplied diagnostic bytes; an
// empty outer field while the inner error carries the excerpt would
// force callers to reach into transport internals via `errors.As`
// just to read a value the public contract already promises.
//
// The transport-level fix lives in `transport/http`'s `handleSmart`;
// this test exercises the session-layer propagation by driving the
// same path end-to-end through Dial.
func TestDial_transportOpenError_propagatesServerExcerpt(t *testing.T) {
	const garbage = "this is definitely not a pkt-line stream\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type",
			"application/x-git-upload-pack-advertisement")
		_, _ = w.Write([]byte(garbage))
	}))
	defer srv.Close()

	_, err := Dial(context.Background(), srv.URL+"/repo.git")
	require.Error(t, err)

	var pe *ProtocolError
	require.True(t, errors.As(err, &pe),
		"want *lsremote.ProtocolError; got %T: %v", err, err)
	assert.Equal(t, "dial", pe.Op)
	assert.Equal(t, http.StatusOK, pe.Status,
		"the HTTP status from the inner *httpt.ProtocolError must "+
			"propagate onto the public *lsremote.ProtocolError")
	require.NotEmpty(t, pe.Server,
		"the inner *httpt.ProtocolError.Server excerpt must "+
			"propagate onto the public *lsremote.ProtocolError.Server")
	assert.LessOrEqual(t, len(pe.Server), 1024+len("..."),
		"Server is bounded to 1 KiB plus a possible ellipsis marker")

	// The inner transport error must remain reachable via `errors.As`
	// so callers who already match on `*httpt.ProtocolError` keep
	// working — the propagation is additive, not a replacement.
	var inner *httpt.ProtocolError
	require.True(t, errors.As(err, &inner),
		"the inner *httpt.ProtocolError must remain reachable")
	assert.Equal(t, pe.Server, inner.Server,
		"the outer Server must mirror the inner Server verbatim")
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

// Test_bridgeWireSentinel_joinsKnownSentinels pins that every wire
// sentinel listed in [wireSentinelBridges] is joined onto a public
// sentinel and that an unknown error passes through unchanged.
func Test_bridgeWireSentinel_joinsKnownSentinels(t *testing.T) {
	cases := []struct {
		name   string
		in     error
		public error // nil ⇒ pass through unchanged
	}{
		{"server-refused", wire.ErrServerRefused, ErrServerRefused},
		{"unsupported-protocol", wire.ErrUnsupportedProtocol, ErrUnsupportedProtocol},
		{"wrapped-server-refused",
			fmt.Errorf("decode: %w", wire.ErrServerRefused), ErrServerRefused},
		{"unknown", errors.New("something else"), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := bridgeWireSentinel(tc.in)
			require.ErrorIs(t, got, tc.in, "original cause must remain reachable")
			if tc.public != nil {
				require.ErrorIs(t, got, tc.public,
					"public sentinel must be joined onto the chain")
			} else {
				require.Same(t, tc.in, got,
					"unknown error must be returned unchanged")
			}
		})
	}
}

// stubTransport is a test fake whose `Open` always returns the
// preconfigured error. It exists so each per-scheme branch of
// `populateFromTransportError` can be exercised without standing up
// a real ssh/file backend.
type stubTransport struct {
	schemes []string
	err     error
}

func (s stubTransport) Schemes() []string { return s.schemes }
func (s stubTransport) Open(_ context.Context, _ *transport.URL,
	_ transport.OpenOptions) (transport.Conn, error) {
	return nil, s.err
}

// TestPopulateFromTransportError_SSH pins that an SSH dial failure
// whose underlying `*ssht.ProtocolError` carries a `Server` excerpt
// lifts that excerpt onto the outer `*lsremote.ProtocolError.Server`.
func TestPopulateFromTransportError_SSH(t *testing.T) {
	want := "ssh: handshake failed: server rejected every offered method"
	stub := stubTransport{
		schemes: []string{"ssh"},
		err: &ssht.ProtocolError{
			URL:    "ssh://git@example.com/repo.git",
			Op:     "dial",
			Server: want,
			Err:    ssht.ErrAuthFailed,
		},
	}
	reg := transport.NewRegistry(stub)

	_, err := Dial(context.Background(), "ssh://git@example.com/repo.git",
		WithTransports(reg))
	require.Error(t, err)

	var pe *ProtocolError
	require.True(t, errors.As(err, &pe))
	assert.Equal(t, want, pe.Server,
		"SSH transport Server excerpt must surface on the public ProtocolError")
	assert.True(t, errors.Is(err, ErrAuthFailed),
		"the SSH ErrAuthFailed sentinel must bridge to the public sentinel")
}

// TestPopulateFromTransportError_File pins the same lift for the
// `file://` transport's corrupt-object branch (transport/file/open.go).
func TestPopulateFromTransportError_File(t *testing.T) {
	want := "objstore: corrupt object 0123abcd..."
	stub := stubTransport{
		schemes: []string{"file"},
		err: &filet.ProtocolError{
			URL:    "file:///tmp/repo",
			Op:     "dial",
			Server: want,
			Err:    errors.New("objstore: corrupt object"),
		},
	}
	reg := transport.NewRegistry(stub)

	_, err := Dial(context.Background(), "file:///tmp/repo",
		WithTransports(reg))
	require.Error(t, err)

	var pe *ProtocolError
	require.True(t, errors.As(err, &pe))
	assert.Equal(t, want, pe.Server,
		"file transport Server excerpt must surface on the public ProtocolError")
}

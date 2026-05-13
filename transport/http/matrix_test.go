package httpt

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hiddeco/go-ls-remote/internal/server"
	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/transport"
)

// TestFixtureMatrix is the consolidated end-to-end matrix for the HTTP
// transport. Each scenario runs the public `Open` path against an
// `httptest.Server` (or a stub round-tripper for cases that cannot be
// modelled with two real listeners), confirms the open succeeds or
// fails as expected, drains the advertisement on success, and — where
// applicable — exercises one v2 command POST through `Conn.Command`.
//
// The individual unit tests in this package cover the same ground at
// finer grain. The matrix re-confirms each scenario through the public
// surface so a regression that only manifests when probe + command run
// in sequence is caught here.
//
// Scenarios included:
//
//   - smart_v2_happy: probe → drain → ls-refs round-trip.
//   - smart_v0_fallback: probe a v0-advertising server, drain v0 frame.
//   - dumb_http: probe a text/plain body, command returns
//     `ErrUnsupportedProtocol`.
//   - auth_401_then_200: anonymous probe 401, retry with creds 200.
//   - auth_401_then_401: probe 401 twice → `ErrAuthFailed`.
//   - status_404 → `ErrNotFound`.
//   - status_5xx → `*ProtocolError` with status and body excerpt.
//   - redirect_chain_initial: 2 same-origin hops to a smart-v2 server.
//   - redirect_on_post_rejected_by_default: 302 on POST → rejected.
//   - redirect_cross_origin_auth_stripped: A→B with creds for A only.
func TestFixtureMatrix(t *testing.T) {
	t.Parallel()

	t.Run("smart_v2_happy", testMatrixSmartV2Happy)
	t.Run("smart_v0_fallback", testMatrixSmartV0Fallback)
	t.Run("dumb_http", testMatrixDumbHTTP)
	t.Run("auth_401_then_200", testMatrixAuth401Then200)
	t.Run("auth_401_then_401", testMatrixAuth401Then401)
	t.Run("status_404", testMatrixStatus404)
	t.Run("status_5xx", testMatrixStatus5xx)
	t.Run("redirect_chain_initial", testMatrixRedirectChainInitial)
	t.Run("redirect_on_post_rejected_by_default", testMatrixRedirectOnPostRejected)
	t.Run("redirect_cross_origin_auth_stripped", testMatrixRedirectCrossOriginAuthStripped)
}

func testMatrixSmartV2Happy(t *testing.T) {
	t.Parallel()

	store := openFixtureStore(t, "loose-only")
	srv := httptest.NewServer(serveHandler(t, store, "/repo.git"))
	defer srv.Close()

	tr := newTestTransport(t)
	u := parseTestURL(t, srv, "/repo.git")
	conn, err := tr.Open(t.Context(), u, transport.OpenOptions{})
	require.NoError(t, err)
	require.NotNil(t, conn)
	t.Cleanup(func() { _ = conn.Close() })

	c, ok := conn.(*Conn)
	require.True(t, ok)
	assert.False(t, c.dumb, "smart-v2 must not flag the Conn dumb")
	drainAdvertisement(t, c)

	rdr, err := c.Command(t.Context(), "ls-refs",
		cmdBody("ls-refs", []string{"peel"}, []string{"object-format=sha1"}))
	require.NoError(t, err)
	require.NotNil(t, rdr)
	pkts := readAllPackets(t, rdr)
	require.NotEmpty(t, pkts, "ls-refs must emit at least one packet")

	var hasRef bool
	for _, p := range pkts {
		if p.Kind == pktline.Data && strings.Contains(string(p.Data), "refs/heads/") {
			hasRef = true
			break
		}
	}
	assert.True(t, hasRef, "ls-refs must produce a refs/heads/ line")
}

func testMatrixSmartV0Fallback(t *testing.T) {
	t.Parallel()

	store := openFixtureStore(t, "loose-only")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// `info/refs?service=git-upload-pack` with a v0 advertisement.
		// Smart-HTTP framing wraps the v0 ref-list with the standard
		// `# service=` preamble, then the v0 stream emits `<oid>
		// <ref>\0<caps>\n` for HEAD followed by per-ref data lines and
		// a trailing flush.
		if r.URL.Path != "/repo.git/info/refs" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", smartAdvHeader)
		pw := pktline.NewWriter(w)
		assert.NoError(t, pw.WritePacket([]byte("# service=git-upload-pack\n")))
		assert.NoError(t, pw.WriteFlush())
		assert.NoError(t, server.Serve(r.Context(),
			pktline.NewReader(bytes.NewReader([]byte("0000"))),
			pw, store, server.Options{
				Agent:             "test-server/0.0",
				PreferredProtocol: transport.ProtocolV0,
			}))
	}))
	defer srv.Close()

	tr := newTestTransport(t)
	u := parseTestURL(t, srv, "/repo.git")
	conn, err := tr.Open(t.Context(), u, transport.OpenOptions{})
	require.NoError(t, err, "a v0 smart advertisement must still open")
	require.NotNil(t, conn)
	t.Cleanup(func() { _ = conn.Close() })

	c, ok := conn.(*Conn)
	require.True(t, ok)
	assert.False(t, c.dumb,
		"a v0 smart advertisement reports as smart, not dumb (the preamble is present)")

	rdr := c.Advertisement()
	pkts := readAllPackets(t, rdr)
	require.NotEmpty(t, pkts, "v0 advertisement must produce packets")
	var first string
	for _, p := range pkts {
		if p.Kind == pktline.Data {
			first = string(p.Data)
			break
		}
	}
	assert.NotEmpty(t, first, "v0 advertisement must carry a data packet")
	// The first v0 data packet carries an oid and `\x00` separator
	// before the cap list (canonical [upload-pack.c::write_v0_ref]).
	//
	// [upload-pack.c::write_v0_ref]: https://github.com/git/git/blob/v2.54.0/upload-pack.c#L1231
	assert.Contains(t, first, "\x00",
		"v0 first ref packet must carry the NUL-delimited cap list")
}

func testMatrixDumbHTTP(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(
			"3333333333333333333333333333333333333333\trefs/heads/main\n",
		))
	}))
	defer srv.Close()

	tr := newTestTransport(t)
	u := parseTestURL(t, srv, "/repo.git")
	conn, err := tr.Open(t.Context(), u, transport.OpenOptions{})
	require.NoError(t, err)
	require.NotNil(t, conn)
	t.Cleanup(func() { _ = conn.Close() })

	c, ok := conn.(*Conn)
	require.True(t, ok)
	assert.True(t, c.dumb, "dumb-HTTP probe must flag Conn.dumb")

	rdr := c.Advertisement()
	pkts := readAllPackets(t, rdr)
	require.NotEmpty(t, pkts)
	var sawMain bool
	for _, p := range pkts {
		if p.Kind == pktline.Data && strings.Contains(string(p.Data), "refs/heads/main") {
			sawMain = true
			break
		}
	}
	assert.True(t, sawMain, "the synthesised v0 stream must surface refs/heads/main")

	cmdRdr, err := c.Command(t.Context(), "ls-refs", cmdBody("ls-refs", nil, nil))
	assert.Nil(t, cmdRdr)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnsupportedProtocol,
		"dumb-HTTP Conn.Command must return ErrUnsupportedProtocol; got %v", err)
}

func testMatrixAuth401Then200(t *testing.T) {
	t.Parallel()

	store := openFixtureStore(t, "loose-only")

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			w.WriteHeader(http.StatusUnauthorized)
		default:
			w.Header().Set("Content-Type", smartAdvHeader)
			pw := pktline.NewWriter(w)
			assert.NoError(t, pw.WritePacket([]byte("# service=git-upload-pack\n")))
			assert.NoError(t, pw.WriteFlush())
			assert.NoError(t, server.Serve(r.Context(),
				pktline.NewReader(bytes.NewReader([]byte("0000"))),
				pw, store, server.Options{
					PreferredProtocol: transport.ProtocolV2,
				}))
		}
	}))
	defer srv.Close()

	tr := newTestTransport(t, WithCredentials(Static(Basic("alice", "secret"))))
	u := parseTestURL(t, srv, "/repo.git")
	conn, err := tr.Open(t.Context(), u, transport.OpenOptions{})
	require.NoError(t, err, "401 followed by an authenticated 200 must open cleanly")
	require.NotNil(t, conn)
	t.Cleanup(func() { _ = conn.Close() })

	c := conn.(*Conn)
	drainAdvertisement(t, c)
	assert.Equal(t, int32(2), calls.Load(),
		"exactly two probe round-trips: anonymous then authenticated")
}

func testMatrixAuth401Then401(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	tr := newTestTransport(t, WithCredentials(Static(Basic("alice", "secret"))))
	u := parseTestURL(t, srv, "/repo.git")
	conn, err := tr.Open(t.Context(), u, transport.OpenOptions{})
	assert.Nil(t, conn)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrAuthFailed,
		"401-after-retry must surface as ErrAuthFailed; got %v", err)
}

func testMatrixStatus404(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	tr := newTestTransport(t)
	u := parseTestURL(t, srv, "/repo.git")
	conn, err := tr.Open(t.Context(), u, transport.OpenOptions{})
	assert.Nil(t, conn)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound,
		"404 must map to ErrNotFound; got %v", err)
}

func testMatrixStatus5xx(t *testing.T) {
	t.Parallel()

	const body = "upload-pack on fire"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	tr := newTestTransport(t)
	u := parseTestURL(t, srv, "/repo.git")
	conn, err := tr.Open(t.Context(), u, transport.OpenOptions{})
	assert.Nil(t, conn)
	require.Error(t, err)
	var pe *ProtocolError
	require.ErrorAs(t, err, &pe,
		"5xx must surface as *ProtocolError; got %T: %v", err, err)
	assert.Equal(t, http.StatusServiceUnavailable, pe.Status)
	assert.Equal(t, "probe", pe.Op)
	assert.Contains(t, pe.Server, body,
		"the *ProtocolError must carry the server's body excerpt")
}

func testMatrixRedirectChainInitial(t *testing.T) {
	t.Parallel()

	store := openFixtureStore(t, "loose-only")
	mux := http.NewServeMux()
	mux.HandleFunc("/hop-0/info/refs", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/hop-1/info/refs?"+r.URL.RawQuery, http.StatusMovedPermanently)
	})
	mux.HandleFunc("/hop-1/info/refs", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/repo.git/info/refs?"+r.URL.RawQuery, http.StatusMovedPermanently)
	})
	mux.HandleFunc("/repo.git/info/refs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", smartAdvHeader)
		pw := pktline.NewWriter(w)
		assert.NoError(t, pw.WritePacket([]byte("# service=git-upload-pack\n")))
		assert.NoError(t, pw.WriteFlush())
		assert.NoError(t, server.Serve(r.Context(),
			pktline.NewReader(bytes.NewReader([]byte("0000"))),
			pw, store, server.Options{
				PreferredProtocol: transport.ProtocolV2,
			}))
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	tr := newTestTransport(t)
	u := parseTestURL(t, srv, "/hop-0")
	conn, err := tr.Open(t.Context(), u, transport.OpenOptions{})
	require.NoError(t, err, "two same-origin hops must follow under the default policy")
	require.NotNil(t, conn)
	t.Cleanup(func() { _ = conn.Close() })

	c, ok := conn.(*Conn)
	require.True(t, ok)
	assert.Contains(t, c.url.String(), "/repo.git/info/refs",
		"Conn.url must reflect the final hop, not the original or any intermediate")
	assert.NotContains(t, c.url.String(), "/hop-0",
		"the original hop path must not survive on the Conn")
}

func testMatrixRedirectOnPostRejected(t *testing.T) {
	t.Parallel()

	store := openFixtureStore(t, "loose-only")
	mux := http.NewServeMux()
	mux.HandleFunc("/repo.git/info/refs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", smartAdvHeader)
		pw := pktline.NewWriter(w)
		assert.NoError(t, pw.WritePacket([]byte("# service=git-upload-pack\n")))
		assert.NoError(t, pw.WriteFlush())
		assert.NoError(t, server.Serve(r.Context(),
			pktline.NewReader(bytes.NewReader([]byte("0000"))),
			pw, store, server.Options{
				PreferredProtocol: transport.ProtocolV2,
			}))
	})
	mux.HandleFunc("/repo.git/git-upload-pack", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere.git/git-upload-pack", http.StatusTemporaryRedirect)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	tr := newTestTransport(t)
	u := parseTestURL(t, srv, "/repo.git")
	conn, err := tr.Open(t.Context(), u, transport.OpenOptions{})
	require.NoError(t, err, "the probe GET must succeed under the default Initial policy")
	require.NotNil(t, conn)
	t.Cleanup(func() { _ = conn.Close() })

	c := conn.(*Conn)
	drainAdvertisement(t, c)

	rdr, err := c.Command(t.Context(), "ls-refs", cmdBody("ls-refs", nil, nil))
	assert.Nil(t, rdr)
	require.Error(t, err)
	var pe *ProtocolError
	require.ErrorAs(t, err, &pe,
		"a default-rejected POST redirect must surface as *ProtocolError; got %T: %v", err, err)
	assert.Equal(t, "command", pe.Op)
	assert.ErrorIs(t, pe.Err, errRedirectRejected,
		"the rejection cause must wrap errRedirectRejected")
}

func testMatrixRedirectCrossOriginAuthStripped(t *testing.T) {
	t.Parallel()

	var bAuth string
	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", smartAdvHeader)
		_, _ = w.Write(smartAdvBody(t))
	}))
	defer srvB.Close()

	bURL, err := url.Parse(srvB.URL)
	require.NoError(t, err)

	var aCalls atomic.Int32
	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch aCalls.Add(1) {
		case 1:
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			w.WriteHeader(http.StatusUnauthorized)
		default:
			http.Redirect(w, r,
				srvB.URL+"/repo.git/info/refs?service=git-upload-pack",
				http.StatusFound)
		}
	}))
	defer srvA.Close()

	resolver := credentialResolverFunc(func(_ context.Context, u *url.URL) (Credentials, error) {
		if u.Host == bURL.Host {
			// Returning `(nil, nil)` is the documented "no credential
			// available" signal; isolating the strip step from any
			// resolver-supply step.
			return nil, nil
		}
		return Basic("alice", "secret"), nil
	})

	tr := newTestTransport(t, WithCredentials(resolver))
	u := parseTestURL(t, srvA, "/repo.git")
	conn, err := tr.Open(t.Context(), u, transport.OpenOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	assert.Empty(t, bAuth,
		"a cross-origin redirect must strip Authorization before reaching server B")
}

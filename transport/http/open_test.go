package httpt

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hiddeco/go-ls-remote/internal/wire"
	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/transport"
)

// smartAdvBody returns a minimal but realistic smart-HTTP advertisement
// body: the `# service=git-upload-pack\n` preamble pkt-line, a flush
// packet, and a single trailing data packet so tests can read past the
// preamble and assert the post-preamble pktline.Reader is positioned
// correctly.
//
// The trailing data packet's payload is opaque at this layer; the wire
// layer parses it downstream. We use a recognisable sentinel so the
// happy-path test can assert the post-preamble read returned the
// expected bytes.
func smartAdvBody(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := pktline.NewWriter(&buf)
	require.NoError(t, w.WritePacket([]byte("# service=git-upload-pack\n")))
	require.NoError(t, w.WriteFlush())
	require.NoError(t, w.WritePacket([]byte("post-preamble payload\n")))
	return buf.Bytes()
}

// smartAdvHeader is the content type a real smart-HTTP server replies
// with. The probe must accept it case-insensitively and tolerate
// trailing parameters such as `; charset=utf-8`.
const smartAdvHeader = "application/x-git-upload-pack-advertisement"

// parseTestURL parses raw and replaces u.Host with the httptest
// server's host:port so the probe dials the test server. The path is
// preserved verbatim.
func parseTestURL(t *testing.T, srv *httptest.Server, repoPath string) *transport.URL {
	t.Helper()
	su, err := url.Parse(srv.URL)
	require.NoError(t, err)
	host := su.Hostname()
	port := su.Port()
	raw := "http://" + su.Host + repoPath
	tu, err := transport.ParseURL(raw)
	require.NoError(t, err)
	require.Equal(t, host, tu.Host)
	require.Equal(t, port, tu.Port)
	return tu
}

func TestOpen_Smart200_Success(t *testing.T) {
	t.Parallel()
	body := smartAdvBody(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/repo.git/info/refs", r.URL.Path)
		assert.Equal(t, "git-upload-pack", r.URL.Query().Get("service"))
		assert.Equal(t, "version=2", r.Header.Get("Git-Protocol"),
			"Git-Protocol defaults to version=2 per HTTPProtocolHeader(nil)")
		assert.NotEmpty(t, r.Header.Get("User-Agent"),
			"User-Agent must be set; package default applies when nothing overrides")
		w.Header().Set("Content-Type", smartAdvHeader)
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	tr := New()
	u := parseTestURL(t, srv, "/repo.git")

	conn, err := tr.Open(t.Context(), u, transport.OpenOptions{})
	require.NoError(t, err)
	require.NotNil(t, conn)
	t.Cleanup(func() { _ = conn.Close() })

	rdr := conn.Advertisement()
	require.NotNil(t, rdr, "Advertisement must return the cached reader")
	pkt, err := rdr.ReadPacket()
	require.NoError(t, err)
	assert.Equal(t, pktline.Data, pkt.Kind,
		"after the preamble strip, the next packet is the first advertisement data line")
	assert.Equal(t, "post-preamble payload\n", string(pkt.Data),
		"the cached reader must be positioned past the preamble + flush")
}

func TestOpen_Smart200_ContentTypeWithCharset(t *testing.T) {
	t.Parallel()
	body := smartAdvBody(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", smartAdvHeader+"; charset=utf-8")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	tr := New()
	u := parseTestURL(t, srv, "/repo.git")

	conn, err := tr.Open(t.Context(), u, transport.OpenOptions{})
	require.NoError(t, err, "trailing parameters on the content type must not break detection")
	t.Cleanup(func() { _ = conn.Close() })
}

func TestOpen_Smart200_PreferredProtocolPinned(t *testing.T) {
	t.Parallel()
	body := smartAdvBody(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "version=0", r.Header.Get("Git-Protocol"),
			"a pinned PreferredProtocol must travel verbatim on Git-Protocol")
		w.Header().Set("Content-Type", smartAdvHeader)
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	tr := New()
	u := parseTestURL(t, srv, "/repo.git")

	v := transport.ProtocolV0
	conn, err := tr.Open(t.Context(), u, transport.OpenOptions{PreferredProtocol: &v})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
}

func TestOpen_Smart200_UserAgent_OpenOptionsWins(t *testing.T) {
	t.Parallel()
	body := smartAdvBody(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "ua-from-opts/1", r.Header.Get("User-Agent"),
			"OpenOptions.UserAgent must win over the per-Transport value")
		w.Header().Set("Content-Type", smartAdvHeader)
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	tr := New(WithUserAgent("ua-from-transport/1"))
	u := parseTestURL(t, srv, "/repo.git")

	conn, err := tr.Open(t.Context(), u, transport.OpenOptions{UserAgent: "ua-from-opts/1"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
}

func TestOpen_Smart200_UserAgent_TransportFallsBackToPackageDefault(t *testing.T) {
	t.Parallel()
	body := smartAdvBody(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, wire.DefaultUserAgent, r.Header.Get("User-Agent"),
			"with no override, the package default applies")
		w.Header().Set("Content-Type", smartAdvHeader)
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	tr := New()
	u := parseTestURL(t, srv, "/repo.git")

	conn, err := tr.Open(t.Context(), u, transport.OpenOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
}

func TestOpen_Smart200_BadPreambleService(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	w := pktline.NewWriter(&buf)
	require.NoError(t, w.WritePacket([]byte("# service=git-receive-pack\n")))
	require.NoError(t, w.WriteFlush())

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", smartAdvHeader)
		_, _ = w.Write(buf.Bytes())
	}))
	defer srv.Close()

	tr := New()
	u := parseTestURL(t, srv, "/repo.git")

	conn, err := tr.Open(t.Context(), u, transport.OpenOptions{})
	assert.Nil(t, conn)
	require.Error(t, err)
	var pe *ProtocolError
	require.ErrorAs(t, err, &pe, "want *ProtocolError, got %T: %v", err, err)
	assert.Equal(t, 200, pe.Status)
	assert.Equal(t, "probe", pe.Op)
}

func TestOpen_Smart200_MissingFlush(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	w := pktline.NewWriter(&buf)
	require.NoError(t, w.WritePacket([]byte("# service=git-upload-pack\n")))
	// No flush — the second packet is another data line, which violates
	// the smart-HTTP framing.
	require.NoError(t, w.WritePacket([]byte("garbage\n")))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", smartAdvHeader)
		_, _ = w.Write(buf.Bytes())
	}))
	defer srv.Close()

	tr := New()
	u := parseTestURL(t, srv, "/repo.git")

	conn, err := tr.Open(t.Context(), u, transport.OpenOptions{})
	assert.Nil(t, conn)
	require.Error(t, err)
	var pe *ProtocolError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, 200, pe.Status)
}

// TestOpen_Smart200_MalformedPreamble_PopulatesServerExcerpt covers the
// 200-with-malformed-body branch of [handleSmart]: a server replies with
// the smart-HTTP content type but a body whose first bytes are not a
// valid pkt-line. The resulting [*ProtocolError] must carry the
// server-sent bytes in its `Server` field, the same way the `5xx`
// branch already does — the doc comment on [ProtocolError.Server]
// promises an excerpt is provided whenever one is available.
//
// `stripSmartPreamble` may have already consumed some bytes by the
// time it returns the error, so the excerpt covers whatever remains on
// the body — the SPEC §7 contract names "a truncated excerpt", not
// "the full body verbatim".
func TestOpen_Smart200_MalformedPreamble_PopulatesServerExcerpt(t *testing.T) {
	t.Parallel()
	const garbage = "this is definitely not a pkt-line stream\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", smartAdvHeader)
		_, _ = w.Write([]byte(garbage))
	}))
	defer srv.Close()

	tr := New()
	u := parseTestURL(t, srv, "/repo.git")

	conn, err := tr.Open(t.Context(), u, transport.OpenOptions{})
	assert.Nil(t, conn)
	require.Error(t, err)
	var pe *ProtocolError
	require.ErrorAs(t, err, &pe, "want *ProtocolError, got %T: %v", err, err)
	assert.Equal(t, http.StatusOK, pe.Status)
	assert.Equal(t, "probe", pe.Op)
	require.NotEmpty(t, pe.Server,
		"ProtocolError.Server must carry an excerpt of the malformed body")
	assert.LessOrEqual(t, len(pe.Server), 1024+len("..."),
		"Server is bounded to 1 KiB plus a possible ellipsis marker")
}

func TestOpen_Dumb200_AdapterWired(t *testing.T) {
	t.Parallel()
	// A minimal but realistic dumb-HTTP `info/refs` body: one ref
	// per line, fields HTAB-separated, terminated by LF. The adapter
	// synthesises a v0-shaped pkt-line stream over this body; the
	// post-Open Conn must be flagged dumb and Advertisement must
	// yield a parseable pkt-line stream.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(
			"3333333333333333333333333333333333333333\trefs/heads/main\n",
		))
	}))
	defer srv.Close()

	tr := New()
	u := parseTestURL(t, srv, "/repo.git")

	conn, err := tr.Open(t.Context(), u, transport.OpenOptions{})
	require.NoError(t, err, "the dumb-HTTP adapter must wrap a 200 + non-smart body")
	require.NotNil(t, conn)
	t.Cleanup(func() { _ = conn.Close() })

	c, ok := conn.(*Conn)
	require.True(t, ok, "concrete type must be *Conn")
	assert.True(t, c.dumb, "Conn.dumb must be true on the dumb branch")

	rdr := conn.Advertisement()
	require.NotNil(t, rdr)
	pkt, err := rdr.ReadPacket()
	require.NoError(t, err)
	assert.Equal(t, pktline.Data, pkt.Kind,
		"the synthesised adapter must yield a data packet for the first ref")
	assert.Contains(t, string(pkt.Data), "refs/heads/main",
		"the first synthesised pkt-line must carry the dumb body's ref")
}

func TestOpen_401_NoCreds_ReturnsErrAuthRequired(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	tr := New()
	u := parseTestURL(t, srv, "/repo.git")

	conn, err := tr.Open(t.Context(), u, transport.OpenOptions{})
	assert.Nil(t, conn)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrAuthRequired,
		"401 with no resolver must return ErrAuthRequired; got %v", err)
}

func TestOpen_401_StaticResolver_AcceptsOnRetry(t *testing.T) {
	t.Parallel()
	body := smartAdvBody(t)
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:secret"))

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch calls {
		case 1:
			assert.Empty(t, r.Header.Get("Authorization"),
				"the first request must be anonymous; credentials only fly on the retry")
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			w.WriteHeader(http.StatusUnauthorized)
		case 2:
			assert.Equal(t, want, r.Header.Get("Authorization"),
				"the retry must carry the resolver-supplied Basic credential")
			w.Header().Set("Content-Type", smartAdvHeader)
			_, _ = w.Write(body)
		default:
			t.Fatalf("unexpected third request to the probe")
		}
	}))
	defer srv.Close()

	tr := New(WithCredentials(Static(Basic("alice", "secret"))))
	u := parseTestURL(t, srv, "/repo.git")

	conn, err := tr.Open(t.Context(), u, transport.OpenOptions{})
	require.NoError(t, err)
	require.NotNil(t, conn)
	t.Cleanup(func() { _ = conn.Close() })

	assert.Equal(t, 2, calls, "exactly two requests: anonymous probe then authenticated retry")
}

func TestOpen_401_StaticResolver_RejectsOnRetry(t *testing.T) {
	t.Parallel()
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	tr := New(WithCredentials(Static(Basic("alice", "secret"))))
	u := parseTestURL(t, srv, "/repo.git")

	conn, err := tr.Open(t.Context(), u, transport.OpenOptions{})
	assert.Nil(t, conn)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrAuthFailed,
		"401-after-retry must return ErrAuthFailed; got %v", err)
	assert.Equal(t, 2, calls,
		"the retry runs at most once: two requests total, no third probe")
}

func TestOpen_401_StaticResolver_NilCred_ReturnsErrAuthRequired(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	// A resolver that produces (nil, nil) is the documented "no
	// credential available" signal; the probe must surface
	// ErrAuthRequired rather than retrying with no Authorization
	// header set.
	tr := New(WithCredentials(Static(nil)))
	u := parseTestURL(t, srv, "/repo.git")

	conn, err := tr.Open(t.Context(), u, transport.OpenOptions{})
	assert.Nil(t, conn)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrAuthRequired,
		"a (nil, nil) resolver outcome must mean ErrAuthRequired; got %v", err)
}

// errResolver returns a deterministic error from Resolve; the probe
// must surface that error verbatim rather than retrying or masking.
type errResolver struct{ err error }

func (r errResolver) Resolve(_ context.Context, _ *url.URL) (Credentials, error) {
	return nil, r.err
}

func TestOpen_401_ResolverError_Propagated(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	want := errors.New("resolver kaboom")
	tr := New(WithCredentials(errResolver{err: want}))
	u := parseTestURL(t, srv, "/repo.git")

	conn, err := tr.Open(t.Context(), u, transport.OpenOptions{})
	assert.Nil(t, conn)
	require.Error(t, err)
	assert.ErrorIs(t, err, want,
		"a resolver error must propagate via errors.Is; got %v", err)
}

func TestOpen_403_ReturnsErrAuthFailed(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	tr := New()
	u := parseTestURL(t, srv, "/repo.git")

	conn, err := tr.Open(t.Context(), u, transport.OpenOptions{})
	assert.Nil(t, conn)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrAuthFailed,
		"403 maps to ErrAuthFailed; got %v", err)
}

func TestOpen_404_ReturnsErrNotFound(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	tr := New()
	u := parseTestURL(t, srv, "/repo.git")

	conn, err := tr.Open(t.Context(), u, transport.OpenOptions{})
	assert.Nil(t, conn)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound,
		"404 maps to ErrNotFound; got %v", err)
}

func TestOpen_500_ReturnsProtocolError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("the server is on fire"))
	}))
	defer srv.Close()

	tr := New()
	u := parseTestURL(t, srv, "/repo.git")

	conn, err := tr.Open(t.Context(), u, transport.OpenOptions{})
	assert.Nil(t, conn)
	require.Error(t, err)
	var pe *ProtocolError
	require.ErrorAs(t, err, &pe,
		"5xx must surface as *ProtocolError; got %T: %v", err, err)
	assert.Equal(t, 500, pe.Status)
	assert.Equal(t, "probe", pe.Op)
	assert.Contains(t, pe.Server, "server is on fire",
		"the server-supplied body excerpt must travel on the error")
}

func TestOpen_500_TruncatesServerBody(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 4096)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(long))
	}))
	defer srv.Close()

	tr := New()
	u := parseTestURL(t, srv, "/repo.git")

	_, err := tr.Open(t.Context(), u, transport.OpenOptions{})
	require.Error(t, err)
	var pe *ProtocolError
	require.ErrorAs(t, err, &pe)
	assert.LessOrEqual(t, len(pe.Server), 1024+len("..."),
		"Server is bounded to 1 KiB plus a possible ellipsis marker")
	assert.Contains(t, pe.Server, "...",
		"truncation must be marked so callers can spot it in logs")
}

func TestOpen_UnexpectedStatus_418(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	defer srv.Close()

	tr := New()
	u := parseTestURL(t, srv, "/repo.git")

	conn, err := tr.Open(t.Context(), u, transport.OpenOptions{})
	assert.Nil(t, conn)
	require.Error(t, err)
	var pe *ProtocolError
	require.ErrorAs(t, err, &pe,
		"unexpected status maps to *ProtocolError; got %T: %v", err, err)
	assert.Equal(t, http.StatusTeapot, pe.Status)
}

func TestBuildInfoRefsURL_IPv6_WithPort(t *testing.T) {
	t.Parallel()
	u := &transport.URL{
		Scheme: "https",
		Host:   "fe80::1",
		Port:   "8443",
		Path:   "/repo.git",
	}
	got := buildInfoRefsURL(u)
	assert.Equal(t,
		"https://[fe80::1]:8443/repo.git/info/refs?service=git-upload-pack",
		got,
		"IPv6 literal with an explicit port must be bracketed before the colon-port suffix")
}

func TestBuildInfoRefsURL_IPv6_NoPort(t *testing.T) {
	t.Parallel()
	u := &transport.URL{
		Scheme: "https",
		Host:   "fe80::1",
		Path:   "/repo.git",
	}
	got := buildInfoRefsURL(u)
	// `(&url.URL{Host: "[fe80::1]"}).String()` preserves the brackets,
	// so an IPv6 literal in u.Host comes out bracketed in the rendered
	// URL even when no port disambiguates it. The bracket is harmless
	// for a host-only URL and matches what the function emits today.
	assert.Equal(t,
		"https://[fe80::1]/repo.git/info/refs?service=git-upload-pack",
		got,
		"a port-less IPv6 host renders as `[fe80::1]` per net/url's host handling")
}

func TestOpen_URL_StripsTrailingSlash(t *testing.T) {
	t.Parallel()
	body := smartAdvBody(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/repo.git/info/refs", r.URL.Path,
			"a trailing slash on the user URL must not double up to `//info/refs`")
		w.Header().Set("Content-Type", smartAdvHeader)
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	tr := New()
	u := parseTestURL(t, srv, "/repo.git/")

	conn, err := tr.Open(t.Context(), u, transport.OpenOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
}

func TestOpen_URL_RedactsCredentialsInProtocolError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	// Carry userinfo in the parsed transport.URL; the request must
	// not put it in the URL it dials, and the *ProtocolError must
	// redact the password before logging.
	su, err := url.Parse(srv.URL)
	require.NoError(t, err)
	raw := "http://alice:secret@" + su.Host + "/repo.git"
	u, err := transport.ParseURL(raw)
	require.NoError(t, err)

	tr := New()
	_, err = tr.Open(t.Context(), u, transport.OpenOptions{})
	require.Error(t, err)
	var pe *ProtocolError
	require.ErrorAs(t, err, &pe)
	assert.NotContains(t, pe.URL, "secret",
		"the password must never travel in *ProtocolError.URL")
}

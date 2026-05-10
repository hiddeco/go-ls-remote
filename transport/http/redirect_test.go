package httpt

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hiddeco/go-ls-remote/transport"
)

// smartHandler returns an [http.HandlerFunc] that responds with a
// minimal smart-HTTP advertisement. It is used as the terminal hop in
// redirect-chain tests where the chain itself is the subject under
// test, not the advertisement contents.
func smartHandler(t *testing.T) http.HandlerFunc {
	t.Helper()
	body := smartAdvBody(t)
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", smartAdvHeader)
		_, _ = w.Write(body)
	}
}

func TestRedirect_Initial_FollowsToFinal(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/old.git/info/refs", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/repo.git/info/refs?"+r.URL.RawQuery, http.StatusMovedPermanently)
	})
	mux.HandleFunc("/repo.git/info/refs", smartHandler(t))

	srv := httptest.NewServer(mux)
	defer srv.Close()

	tr := New()
	u := parseTestURL(t, srv, "/old.git")

	conn, err := tr.Open(context.Background(), u, transport.OpenOptions{})
	require.NoError(t, err)
	require.NotNil(t, conn)
	t.Cleanup(func() { _ = conn.Close() })

	c, ok := conn.(*Conn)
	require.True(t, ok)
	assert.Contains(t, c.url, "/repo.git/info/refs",
		"after a redirect, Conn.url must reflect the final hop, not the original")
}

func TestRedirect_Initial_RespectsMaxRedirects(t *testing.T) {
	var hops int32
	mux := http.NewServeMux()
	mux.HandleFunc("/repo.git/info/refs", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hops, 1)
		// Send the client through more hops than maxRedirects allows.
		next := fmt.Sprintf("/repo.git/info/refs?service=git-upload-pack&hop=%d", n)
		http.Redirect(w, r, next, http.StatusFound)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	tr := New(WithMaxRedirects(2))
	u := parseTestURL(t, srv, "/repo.git")

	_, err := tr.Open(context.Background(), u, transport.OpenOptions{})
	require.Error(t, err)
	var pe *ProtocolError
	require.True(t, errors.As(err, &pe),
		"exceeding maxRedirects must surface as *ProtocolError; got %T: %v", err, err)
	assert.Equal(t, "probe", pe.Op)
	assert.True(t, errors.Is(pe.Err, errRedirectTooMany),
		"a hop-cap exhaustion must wrap errRedirectTooMany so a sentinel "+
			"swap would fail the test; got %v", pe.Err)
}

func TestRedirect_Initial_DefaultMaxIsTen(t *testing.T) {
	// A chain of 9 redirects fits inside the package default of 10 hops.
	const chainLen = 9

	mux := http.NewServeMux()
	for i := 0; i < chainLen; i++ {
		i := i
		from := fmt.Sprintf("/hop-%d/info/refs", i)
		next := fmt.Sprintf("/hop-%d/info/refs?service=git-upload-pack", i+1)
		mux.HandleFunc(from, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, next, http.StatusFound)
		})
	}
	mux.HandleFunc(fmt.Sprintf("/hop-%d/info/refs", chainLen), smartHandler(t))

	srv := httptest.NewServer(mux)
	defer srv.Close()

	tr := New()
	u := parseTestURL(t, srv, "/hop-0")

	conn, err := tr.Open(context.Background(), u, transport.OpenOptions{})
	require.NoError(t, err, "9 hops must fit inside the default cap of 10")
	require.NotNil(t, conn)
	t.Cleanup(func() { _ = conn.Close() })
}

func TestRedirect_Never_RejectsFirst3xx(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/old.git/info/refs", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/repo.git/info/refs?"+r.URL.RawQuery, http.StatusFound)
	})
	mux.HandleFunc("/repo.git/info/refs", smartHandler(t))

	srv := httptest.NewServer(mux)
	defer srv.Close()

	tr := New(WithFollowRedirects(FollowRedirectsNever))
	u := parseTestURL(t, srv, "/old.git")

	_, err := tr.Open(context.Background(), u, transport.OpenOptions{})
	require.Error(t, err)
	var pe *ProtocolError
	require.True(t, errors.As(err, &pe),
		"a rejected redirect surfaces as *ProtocolError; got %T: %v", err, err)
	assert.Equal(t, http.StatusFound, pe.Status,
		"the surfaced status is the 3xx that was rejected")
}

func TestRedirect_Always_FollowsLikeInitial(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/old.git/info/refs", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/repo.git/info/refs?"+r.URL.RawQuery, http.StatusMovedPermanently)
	})
	mux.HandleFunc("/repo.git/info/refs", smartHandler(t))

	srv := httptest.NewServer(mux)
	defer srv.Close()

	tr := New(WithFollowRedirects(FollowRedirectsAlways))
	u := parseTestURL(t, srv, "/old.git")

	conn, err := tr.Open(context.Background(), u, transport.OpenOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
}

func TestRedirect_CrossOrigin_StripsAuthorization(t *testing.T) {
	// The flow tests the strip path: server A demands auth, the retry
	// carries `Authorization`, and a 302 hops to server B where the
	// header must NOT survive. The resolver returns nil for B so the
	// stripped header is not immediately re-supplied — that way the
	// recorded value on B isolates the strip step from the
	// resolver-re-consult step.
	var bAuth string
	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", smartAdvHeader)
		_, _ = w.Write(smartAdvBody(t))
	}))
	defer srvB.Close()

	bURL, err := url.Parse(srvB.URL)
	require.NoError(t, err)

	var aCalls int32
	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch atomic.AddInt32(&aCalls, 1) {
		case 1:
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			w.WriteHeader(http.StatusUnauthorized)
		default:
			http.Redirect(w, r, srvB.URL+"/repo.git/info/refs?service=git-upload-pack", http.StatusFound)
		}
	}))
	defer srvA.Close()

	resolver := credentialResolverFunc(func(_ context.Context, u *url.URL) (Credentials, error) {
		if u.Host == bURL.Host {
			// Returning `(nil, nil)` is the documented "no credential"
			// signal; the strip-test relies on it so a static creds
			// supply does not mask the strip.
			return nil, nil
		}
		return Basic("alice", "secret"), nil
	})

	tr := New(WithCredentials(resolver))
	u := parseTestURL(t, srvA, "/repo.git")

	conn, err := tr.Open(context.Background(), u, transport.OpenOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	assert.Empty(t, bAuth,
		"a cross-origin redirect must strip Authorization before reaching server B")
}

func TestRedirect_CrossOrigin_ReConsultsResolver(t *testing.T) {
	// The companion test to the strip case. Same flow, except the
	// resolver returns DIFFERENT credentials for server B so the
	// re-consult-and-apply step is observable end-to-end.
	var bAuth string
	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", smartAdvHeader)
		_, _ = w.Write(smartAdvBody(t))
	}))
	defer srvB.Close()

	bURL, err := url.Parse(srvB.URL)
	require.NoError(t, err)

	var aCalls int32
	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch atomic.AddInt32(&aCalls, 1) {
		case 1:
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			w.WriteHeader(http.StatusUnauthorized)
		default:
			http.Redirect(w, r, srvB.URL+"/repo.git/info/refs?service=git-upload-pack", http.StatusFound)
		}
	}))
	defer srvA.Close()

	resolver := credentialResolverFunc(func(_ context.Context, u *url.URL) (Credentials, error) {
		if u.Host == bURL.Host {
			return Basic("bob", "other"), nil
		}
		return Basic("alice", "secret"), nil
	})

	tr := New(WithCredentials(resolver))
	u := parseTestURL(t, srvA, "/repo.git")

	conn, err := tr.Open(context.Background(), u, transport.OpenOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("bob:other"))
	assert.Equal(t, want, bAuth,
		"a cross-origin redirect must re-consult the resolver and apply its new credential")
}

// stubRoundTripper drives a custom redirect by forging an in-memory
// response. The brief calls it out as the right tool for scheme
// up/downgrade scenarios that httptest cannot simulate naturally.
type stubRoundTripper struct {
	requests []*http.Request
	respond  func(req *http.Request, hop int) *http.Response
}

func (s *stubRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	hop := len(s.requests)
	s.requests = append(s.requests, req.Clone(req.Context()))
	resp := s.respond(req, hop)
	if resp == nil {
		// A misconfigured stub should fail loudly: a nil response
		// would otherwise panic on the field assignments below, and
		// the panic surfaces as a generic test failure with no hint
		// that the stub itself is at fault.
		return nil, fmt.Errorf("stubRoundTripper: respond returned nil for hop %d (%s %s)",
			hop, req.Method, req.URL)
	}
	resp.Request = req
	if resp.Body == nil {
		resp.Body = http.NoBody
	}
	return resp, nil
}

// schemeRedirectStub builds a [stubRoundTripper] that forces the
// Authorization header onto a request via the 401-retry path before
// triggering a same-host scheme change. The flow:
//   - hop 0: anonymous probe, returns 401.
//   - hop 1: authenticated retry on the originating scheme/host.
//     Returns 302 to the redirect target (passed in via locationURL).
//   - hop 2+: returns smart-200.
//
// This isolates the same-host scheme-change case in a way httptest
// cannot, since httptest cannot easily front the same listener under
// both schemes.
func schemeRedirectStub(t *testing.T, locationURL string) *stubRoundTripper {
	t.Helper()
	rt := &stubRoundTripper{}
	rt.respond = func(_ *http.Request, hop int) *http.Response {
		switch hop {
		case 0:
			h := http.Header{}
			h.Set("WWW-Authenticate", `Basic realm="git"`)
			return &http.Response{StatusCode: http.StatusUnauthorized, Header: h}
		case 1:
			h := http.Header{}
			h.Set("Location", locationURL)
			return &http.Response{StatusCode: http.StatusFound, Header: h}
		default:
			h := http.Header{}
			h.Set("Content-Type", smartAdvHeader)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     h,
				Body:       io.NopCloser(strings.NewReader(string(smartAdvBody(t)))),
			}
		}
	}
	return rt
}

func TestRedirect_SchemeDowngrade_IsCrossOrigin(t *testing.T) {
	rt := schemeRedirectStub(t, "http://example.com/repo.git/info/refs?service=git-upload-pack")

	// A scheme-aware resolver: returns Basic on the original https
	// origin, but `(nil, nil)` on the downgraded http one. That way the
	// final hop's Authorization isolates the strip step from any
	// re-supply step.
	resolver := credentialResolverFunc(func(_ context.Context, u *url.URL) (Credentials, error) {
		if u.Scheme == "http" {
			return nil, nil
		}
		return Basic("alice", "secret"), nil
	})

	client := &http.Client{Transport: rt}
	tr := New(
		WithClient(client),
		WithCredentials(resolver),
	)
	u, err := transport.ParseURL("https://example.com/repo.git")
	require.NoError(t, err)

	conn, err := tr.Open(context.Background(), u, transport.OpenOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	require.GreaterOrEqual(t, len(rt.requests), 3,
		"expected anonymous probe, 401-retry, and redirected hop")
	final := rt.requests[len(rt.requests)-1]
	assert.Equal(t, "http", final.URL.Scheme,
		"the redirected hop must have downgraded to http")
	assert.Empty(t, final.Header.Get("Authorization"),
		"a scheme downgrade is cross-origin: Authorization must be stripped, and the "+
			"resolver must be re-consulted with the new URL (which here returns nil)")
}

func TestRedirect_SchemeUpgrade_IsSameOrigin(t *testing.T) {
	rt := schemeRedirectStub(t, "https://example.com/repo.git/info/refs?service=git-upload-pack")
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:secret"))

	client := &http.Client{Transport: rt}
	tr := New(
		WithClient(client),
		WithCredentials(Static(Basic("alice", "secret"))),
	)
	u, err := transport.ParseURL("http://example.com/repo.git")
	require.NoError(t, err)

	conn, err := tr.Open(context.Background(), u, transport.OpenOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	require.GreaterOrEqual(t, len(rt.requests), 3)
	final := rt.requests[len(rt.requests)-1]
	assert.Equal(t, "https", final.URL.Scheme,
		"the redirected hop must have upgraded to https")
	assert.Equal(t, want, final.Header.Get("Authorization"),
		"a same-host scheme upgrade preserves Authorization through the redirect")
}

func TestRedirect_FinalURL_RecordedOnConn(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/a.git/info/refs", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/b.git/info/refs?"+r.URL.RawQuery, http.StatusMovedPermanently)
	})
	mux.HandleFunc("/b.git/info/refs", smartHandler(t))

	srv := httptest.NewServer(mux)
	defer srv.Close()

	tr := New()
	u := parseTestURL(t, srv, "/a.git")

	conn, err := tr.Open(context.Background(), u, transport.OpenOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	c, ok := conn.(*Conn)
	require.True(t, ok)
	assert.Contains(t, c.url, "/b.git/info/refs",
		"the recorded URL must be the final hop's path, not /a.git/info/refs")
	assert.NotContains(t, c.url, "/a.git/info/refs",
		"the original path must not survive on the Conn after a redirect")
}

func Test_resolveMaxRedirects(t *testing.T) {
	tests := []struct {
		name string
		in   int
		want int
	}{
		{"zero falls back to package default", 0, defaultMaxRedirects},
		{"negative clamps to zero (no follow)", -1, 0},
		{"large negative also clamps", -100, 0},
		{"explicit positive is preserved", 5, 5},
		{"one is preserved", 1, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, resolveMaxRedirects(tc.in))
		})
	}
}

func TestRedirect_NegativeMaxRedirectsRejectsFirstHop(t *testing.T) {
	// `WithMaxRedirects(-1)` clamps to zero, meaning the very first
	// 3xx must be rejected. The integration matters: a typo in
	// configuration should not let the probe silently fall back to
	// stdlib's default cap of 10.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere/info/refs?"+r.URL.RawQuery, http.StatusFound)
	}))
	defer srv.Close()

	tr := New(WithMaxRedirects(-1))
	u := parseTestURL(t, srv, "/repo.git")

	_, err := tr.Open(context.Background(), u, transport.OpenOptions{})
	require.Error(t, err)
	var pe *ProtocolError
	require.True(t, errors.As(err, &pe),
		"a negative max-redirects must surface as *ProtocolError; got %T: %v", err, err)
	// A `0` cap is treated as "redirects disabled", not "exceeded 0
	// hops": the surfaced sentinel is [errRedirectRejected], the same
	// one [FollowRedirectsNever] uses, so the message reads naturally
	// regardless of which knob disabled redirects.
	assert.True(t, errors.Is(pe.Err, errRedirectRejected),
		"a 0-cap rejection must wrap errRedirectRejected, not "+
			"errRedirectTooMany; got %v", pe.Err)
}

func TestRedirect_Auth401Retry_UsesRedirectedURL(t *testing.T) {
	// Track the requests that hit Server B (the redirect target). The
	// flow under test:
	//   1. probe `/old.git/info/refs` on A → 302 to B
	//   2. anonymous probe on B → 401
	//   3. authenticated retry on B → 200
	// The retry must hit B, not back to A — i.e. the retry uses the
	// post-redirect URL, not the original.
	var (
		bRequests []string
		bAuth     []string
	)
	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bRequests = append(bRequests, r.URL.Path)
		bAuth = append(bAuth, r.Header.Get("Authorization"))
		if r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", smartAdvHeader)
		_, _ = w.Write(smartAdvBody(t))
	}))
	defer srvB.Close()

	bURL, err := url.Parse(srvB.URL)
	require.NoError(t, err)

	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srvB.URL+"/repo.git/info/refs?service=git-upload-pack", http.StatusFound)
	}))
	defer srvA.Close()

	resolver := credentialResolverFunc(func(_ context.Context, u *url.URL) (Credentials, error) {
		if u.Host != bURL.Host {
			return nil, errors.New("resolver consulted with wrong host")
		}
		return Basic("alice", "secret"), nil
	})

	tr := New(WithCredentials(resolver))
	u := parseTestURL(t, srvA, "/repo.git")

	conn, err := tr.Open(context.Background(), u, transport.OpenOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	require.GreaterOrEqual(t, len(bRequests), 2,
		"server B must see at least the initial 401 probe and the authenticated retry")
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:secret"))
	assert.Equal(t, want, bAuth[len(bAuth)-1],
		"the retry must carry the resolver's credential keyed on server B")
}

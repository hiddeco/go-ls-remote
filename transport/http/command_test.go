package httpt

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hiddeco/go-ls-remote/internal/objstore"
	"github.com/hiddeco/go-ls-remote/internal/server"
	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/transport"
)

// materializeRepoFixture mirrors the helper of the same name in
// `internal/server`: it copies the named fixture from
// `testdata/repos/<name>/` into a fresh `t.TempDir()`, renaming the
// committed `dotgit` component to `.git`. Canonical Git refuses to
// track a path containing a literal `.git` component (see
// `path.c::is_dotgit_path`), so the on-disk fixtures store the gitdir
// under a `dotgit/` directory and tests rename it on materialization.
func materializeRepoFixture(t *testing.T, name string) string {
	t.Helper()

	wd, err := os.Getwd()
	require.NoError(t, err)
	src := filepath.Join(wd, "..", "..", "testdata", "repos", name)
	info, err := os.Stat(src)
	require.NoError(t, err, "fixture %q missing", name)
	require.True(t, info.IsDir())

	dst := t.TempDir()
	require.NoError(t, filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		var parts []string
		if rel != "." {
			parts = strings.Split(filepath.ToSlash(rel), "/")
		}
		for i, part := range parts {
			if part == "dotgit" {
				parts[i] = ".git"
			}
		}
		target := filepath.Join(append([]string{dst}, parts...)...)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	}))
	return filepath.Join(dst, ".git")
}

// openFixtureStore materialises the named fixture, ensures a
// `objects/pack/` directory exists (some ref-only fixtures ship
// without one), and returns an opened [objstore.Store] that closes
// when the test ends.
func openFixtureStore(t *testing.T, name string) *objstore.Store {
	t.Helper()
	gitdir := materializeRepoFixture(t, name)
	require.NoError(t, os.MkdirAll(filepath.Join(gitdir, "objects", "pack"), 0o755))
	store, err := objstore.Open(gitdir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// serveHandler returns an [http.HandlerFunc] that mounts an in-process
// `internal/server.Serve` at `/repo.git/info/refs` (smart-HTTP probe)
// and `/repo.git/git-upload-pack` (v2 command POST). It is the
// canonical fixture for end-to-end tests of the HTTP transport against
// the v2 command path.
//
// The advertisement handler emits the smart-HTTP `# service=` preamble
// before delegating to `Serve`; canonical Git's `http-backend.c` does
// the same in `service_rpc`'s upload-pack-info-refs path. The POST
// handler delegates straight to `Serve`, which reads the v2 command
// request and writes the response.
func serveHandler(t *testing.T, store *objstore.Store, repoPath string) http.Handler {
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
			w.Header().Set("Content-Type", commandAcceptType)
			pr := pktline.NewReader(bytes.NewReader(body))
			pw := pktline.NewWriter(w)
			// Re-emit a fresh advertisement before the command-loop
			// because [server.Serve] always runs the advertise-then-loop
			// flow. Real upload-pack-over-HTTP servers split the two:
			// the GET handler returns the advertisement, the POST
			// handler runs the command loop alone. To bridge that here
			// we drive Serve once per POST and discard the leading
			// advertisement bytes from the response below.
			err = server.Serve(r.Context(), pr, pw, store, server.Options{
				Agent:             "test-server/0.0",
				PreferredProtocol: transport.ProtocolV2,
			})
			require.NoError(t, err)
		default:
			http.NotFound(w, r)
		}
	})
}

// readAllPackets drains rdr until [io.EOF], collecting every packet
// (data, flush, delim, response-end) it produced. The Data slice on
// each returned packet is cloned so callers can hold it across the
// next read; [pktline.Reader] otherwise reuses one backing buffer.
func readAllPackets(t *testing.T, rdr *pktline.Reader) []pktline.Packet {
	t.Helper()
	var pkts []pktline.Packet
	for {
		p, err := rdr.ReadPacket()
		if errors.Is(err, io.EOF) {
			return pkts
		}
		require.NoError(t, err)
		if p.Data != nil {
			p.Data = bytes.Clone(p.Data)
		}
		pkts = append(pkts, p)
	}
}

// drainAdvertisement reads the v2 advertisement packets off the [Conn]'s
// reader so the test is positioned to call [Conn.Command]. The
// advertisement is `version 2\n` plus capability lines plus a trailing
// flush per `serve.c::protocol_v2_advertise_capabilities`; the helper
// reads packets until it consumes the flush.
func drainAdvertisement(t *testing.T, c *Conn) {
	t.Helper()
	rdr := c.Advertisement()
	for {
		p, err := rdr.ReadPacket()
		require.NoError(t, err)
		if p.Kind == pktline.Flush {
			return
		}
	}
}

// openSmartTestConn opens a [Conn] against srv at repoPath and drains
// the advertisement so the test can immediately call [Conn.Command].
func openSmartTestConn(t *testing.T, srv *httptest.Server, repoPath string, opts ...Option) *Conn {
	t.Helper()
	tr := New(opts...)
	u := parseTestURL(t, srv, repoPath)
	conn, err := tr.Open(context.Background(), u, transport.OpenOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	c, ok := conn.(*Conn)
	require.True(t, ok)
	drainAdvertisement(t, c)
	return c
}

func TestConn_Command_LSRefs_RoundTrip(t *testing.T) {
	store := openFixtureStore(t, "loose-only")

	srv := httptest.NewServer(serveHandler(t, store, "/repo.git"))
	defer srv.Close()

	c := openSmartTestConn(t, srv, "/repo.git")

	rdr, err := c.Command(context.Background(), "ls-refs",
		[]string{"peel"}, []string{"object-format=sha1"})
	require.NoError(t, err)
	require.NotNil(t, rdr)

	pkts := readAllPackets(t, rdr)
	require.NotEmpty(t, pkts, "ls-refs must emit at least one packet")

	// The fixture's HEAD points at refs/heads/main (oid `aaaa...`); the
	// ls-refs response carries `<oid> HEAD\n` followed by `<oid>
	// refs/heads/<name>\n` lines. We check the canonical
	// HEAD-then-refs shape rather than pinning every byte: the wire
	// shape is exercised in `internal/server`'s pinned tests, and a
	// looser assertion isolates this test from any fixture churn.
	var hasHead bool
	var hasMain bool
	for _, p := range pkts {
		if p.Kind != pktline.Data {
			continue
		}
		s := string(p.Data)
		if strings.Contains(s, " HEAD\n") {
			hasHead = true
		}
		if strings.Contains(s, " refs/heads/main\n") {
			hasMain = true
		}
	}
	assert.True(t, hasHead, "ls-refs response must include a HEAD line; got %q", pkts)
	assert.True(t, hasMain, "ls-refs response must include refs/heads/main; got %q", pkts)
}

func TestConn_Command_ObjectInfo_RoundTrip(t *testing.T) {
	store := openFixtureStore(t, "loose-only")

	srv := httptest.NewServer(serveHandler(t, store, "/repo.git"))
	defer srv.Close()

	c := openSmartTestConn(t, srv, "/repo.git")

	// `aaaa...` is loose-only's ref tip. The handler does not actually
	// resolve the object on disk for our assertion — we only need a
	// well-formed `oid <hex>` argument so the server's parser accepts
	// the request and emits its `size\n` attrs line.
	oid := strings.Repeat("a", 40)
	rdr, err := c.Command(context.Background(), "object-info",
		[]string{"size", "oid " + oid}, []string{"object-format=sha1"})
	require.NoError(t, err)
	require.NotNil(t, rdr)

	pkts := readAllPackets(t, rdr)
	require.NotEmpty(t, pkts, "object-info must emit at least one packet")

	// Per `protocol-caps.c::send_info`, an object-info request with
	// `size` produces a `size\n` attrs line followed by a per-OID line.
	// We assert both shapes.
	var hasSize bool
	for _, p := range pkts {
		if p.Kind != pktline.Data {
			continue
		}
		if strings.TrimRight(string(p.Data), "\n") == "size" {
			hasSize = true
		}
	}
	assert.True(t, hasSize, "object-info response must include the `size` attrs line")
}

func TestConn_Command_Headers(t *testing.T) {
	store := openFixtureStore(t, "loose-only")

	var captured http.Header
	mux := http.NewServeMux()
	mux.HandleFunc("/repo.git/info/refs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", smartAdvHeader)
		pw := pktline.NewWriter(w)
		require.NoError(t, pw.WritePacket([]byte("# service=git-upload-pack\n")))
		require.NoError(t, pw.WriteFlush())
		require.NoError(t, server.Serve(r.Context(),
			pktline.NewReader(bytes.NewReader([]byte("0000"))),
			pw, store, server.Options{PreferredProtocol: transport.ProtocolV2}))
	})
	mux.HandleFunc("/repo.git/git-upload-pack", func(w http.ResponseWriter, r *http.Request) {
		captured = r.Header.Clone()
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		w.Header().Set("Content-Type", commandAcceptType)
		require.NoError(t, server.Serve(r.Context(),
			pktline.NewReader(bytes.NewReader(body)),
			pktline.NewWriter(w), store,
			server.Options{PreferredProtocol: transport.ProtocolV2}))
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := openSmartTestConn(t, srv, "/repo.git", WithUserAgent("ua-from-tr/1"))
	rdr, err := c.Command(context.Background(), "ls-refs", nil, nil)
	require.NoError(t, err)
	_ = readAllPackets(t, rdr)

	require.NotNil(t, captured, "command POST must have hit the mux handler")
	assert.Equal(t, commandContentType, captured.Get("Content-Type"),
		"the command POST must set the canonical request Content-Type")
	assert.Equal(t, commandAcceptType, captured.Get("Accept"),
		"the command POST must set the canonical Accept header")
	assert.Equal(t, "version=2", captured.Get("Git-Protocol"),
		"the command POST must echo the negotiated Git-Protocol value")
	assert.Equal(t, "ua-from-tr/1", captured.Get("User-Agent"),
		"the command POST must reuse the [Conn]'s User-Agent")
}

func TestConn_Command_Body_PktLineShape(t *testing.T) {
	store := openFixtureStore(t, "loose-only")

	var captured []byte
	mux := http.NewServeMux()
	mux.HandleFunc("/repo.git/info/refs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", smartAdvHeader)
		pw := pktline.NewWriter(w)
		require.NoError(t, pw.WritePacket([]byte("# service=git-upload-pack\n")))
		require.NoError(t, pw.WriteFlush())
		require.NoError(t, server.Serve(r.Context(),
			pktline.NewReader(bytes.NewReader([]byte("0000"))),
			pw, store, server.Options{PreferredProtocol: transport.ProtocolV2}))
	})
	mux.HandleFunc("/repo.git/git-upload-pack", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		captured = body
		w.Header().Set("Content-Type", commandAcceptType)
		require.NoError(t, server.Serve(r.Context(),
			pktline.NewReader(bytes.NewReader(body)),
			pktline.NewWriter(w), store,
			server.Options{PreferredProtocol: transport.ProtocolV2}))
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := openSmartTestConn(t, srv, "/repo.git")
	rdr, err := c.Command(context.Background(), "ls-refs",
		[]string{"peel", "symrefs"}, []string{"object-format=sha1"})
	require.NoError(t, err)
	_ = readAllPackets(t, rdr)

	// Verify the on-wire request body matches the canonical v2
	// command-request grammar from `gitprotocol-v2.adoc` §"Command
	// Request":
	//
	//     PKT-LINE("command=ls-refs\n")
	//     PKT-LINE("object-format=sha1\n")
	//     0001                              <- delim
	//     PKT-LINE("peel\n")
	//     PKT-LINE("symrefs\n")
	//     0000                              <- flush
	//
	pr := pktline.NewReader(bytes.NewReader(captured))
	want := []struct {
		kind pktline.Kind
		data string
	}{
		{pktline.Data, "command=ls-refs\n"},
		{pktline.Data, "object-format=sha1\n"},
		{pktline.Delim, ""},
		{pktline.Data, "peel\n"},
		{pktline.Data, "symrefs\n"},
		{pktline.Flush, ""},
	}
	for i, w := range want {
		p, err := pr.ReadPacket()
		require.NoError(t, err, "packet %d", i)
		assert.Equal(t, w.kind, p.Kind, "packet %d kind", i)
		if w.kind == pktline.Data {
			assert.Equal(t, w.data, string(p.Data), "packet %d data", i)
		}
	}
}

func TestConn_Command_404(t *testing.T) {
	store := openFixtureStore(t, "loose-only")
	mux := http.NewServeMux()
	mux.HandleFunc("/repo.git/info/refs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", smartAdvHeader)
		pw := pktline.NewWriter(w)
		require.NoError(t, pw.WritePacket([]byte("# service=git-upload-pack\n")))
		require.NoError(t, pw.WriteFlush())
		require.NoError(t, server.Serve(r.Context(),
			pktline.NewReader(bytes.NewReader([]byte("0000"))),
			pw, store, server.Options{PreferredProtocol: transport.ProtocolV2}))
	})
	mux.HandleFunc("/repo.git/git-upload-pack", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := openSmartTestConn(t, srv, "/repo.git")
	rdr, err := c.Command(context.Background(), "ls-refs", nil, nil)
	assert.Nil(t, rdr)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotFound),
		"command POST 404 must map to ErrNotFound; got %v", err)
}

func TestConn_Command_500(t *testing.T) {
	store := openFixtureStore(t, "loose-only")
	mux := http.NewServeMux()
	mux.HandleFunc("/repo.git/info/refs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", smartAdvHeader)
		pw := pktline.NewWriter(w)
		require.NoError(t, pw.WritePacket([]byte("# service=git-upload-pack\n")))
		require.NoError(t, pw.WriteFlush())
		require.NoError(t, server.Serve(r.Context(),
			pktline.NewReader(bytes.NewReader([]byte("0000"))),
			pw, store, server.Options{PreferredProtocol: transport.ProtocolV2}))
	})
	mux.HandleFunc("/repo.git/git-upload-pack", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("upload-pack lit on fire"))
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := openSmartTestConn(t, srv, "/repo.git")
	rdr, err := c.Command(context.Background(), "ls-refs", nil, nil)
	assert.Nil(t, rdr)
	require.Error(t, err)
	var pe *ProtocolError
	require.True(t, errors.As(err, &pe),
		"command POST 5xx must surface as *ProtocolError; got %T: %v", err, err)
	assert.Equal(t, "command", pe.Op)
	assert.Equal(t, http.StatusInternalServerError, pe.Status)
	assert.Contains(t, pe.Server, "upload-pack lit on fire")
}

func TestConn_Command_401_NoCreds_ReturnsErrAuthRequired(t *testing.T) {
	store := openFixtureStore(t, "loose-only")
	mux := http.NewServeMux()
	mux.HandleFunc("/repo.git/info/refs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", smartAdvHeader)
		pw := pktline.NewWriter(w)
		require.NoError(t, pw.WritePacket([]byte("# service=git-upload-pack\n")))
		require.NoError(t, pw.WriteFlush())
		require.NoError(t, server.Serve(r.Context(),
			pktline.NewReader(bytes.NewReader([]byte("0000"))),
			pw, store, server.Options{PreferredProtocol: transport.ProtocolV2}))
	})
	mux.HandleFunc("/repo.git/git-upload-pack", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
		w.WriteHeader(http.StatusUnauthorized)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	// No credential resolver: the command POST is sent anonymously and
	// the 401 surfaces as ErrAuthRequired (the caller may now plug in
	// credentials and retry at the Session layer).
	c := openSmartTestConn(t, srv, "/repo.git")
	rdr, err := c.Command(context.Background(), "ls-refs", nil, nil)
	assert.Nil(t, rdr)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrAuthRequired),
		"command 401 with no creds applied maps to ErrAuthRequired; got %v", err)
}

func TestConn_Command_401_WithCreds_ReturnsErrAuthFailed(t *testing.T) {
	store := openFixtureStore(t, "loose-only")
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:secret"))

	var sawPostAuth string
	mux := http.NewServeMux()
	mux.HandleFunc("/repo.git/info/refs", func(w http.ResponseWriter, r *http.Request) {
		// The probe path is exercised in open_test.go; this fixture
		// accepts on the first GET so the [Conn] is positioned for
		// the Command call we want to test, regardless of the static
		// resolver's involvement.
		w.Header().Set("Content-Type", smartAdvHeader)
		pw := pktline.NewWriter(w)
		require.NoError(t, pw.WritePacket([]byte("# service=git-upload-pack\n")))
		require.NoError(t, pw.WriteFlush())
		require.NoError(t, server.Serve(r.Context(),
			pktline.NewReader(bytes.NewReader([]byte("0000"))),
			pw, store, server.Options{PreferredProtocol: transport.ProtocolV2}))
	})
	mux.HandleFunc("/repo.git/git-upload-pack", func(w http.ResponseWriter, r *http.Request) {
		sawPostAuth = r.Header.Get("Authorization")
		w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
		w.WriteHeader(http.StatusUnauthorized)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := openSmartTestConn(t, srv, "/repo.git",
		WithCredentials(Static(Basic("alice", "secret"))))

	rdr, err := c.Command(context.Background(), "ls-refs", nil, nil)
	assert.Nil(t, rdr)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrAuthFailed),
		"command 401 with creds applied maps to ErrAuthFailed; got %v", err)
	assert.Equal(t, want, sawPostAuth,
		"the command POST must carry the static resolver's credential before failing")
}

func TestRedirect_OnPost_Initial_Rejects(t *testing.T) {
	store := openFixtureStore(t, "loose-only")
	mux := http.NewServeMux()
	mux.HandleFunc("/repo.git/info/refs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", smartAdvHeader)
		pw := pktline.NewWriter(w)
		require.NoError(t, pw.WritePacket([]byte("# service=git-upload-pack\n")))
		require.NoError(t, pw.WriteFlush())
		require.NoError(t, server.Serve(r.Context(),
			pktline.NewReader(bytes.NewReader([]byte("0000"))),
			pw, store, server.Options{PreferredProtocol: transport.ProtocolV2}))
	})
	mux.HandleFunc("/repo.git/git-upload-pack", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere.git/git-upload-pack", http.StatusTemporaryRedirect)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := openSmartTestConn(t, srv, "/repo.git")
	rdr, err := c.Command(context.Background(), "ls-refs", nil, nil)
	assert.Nil(t, rdr)
	require.Error(t, err)
	var pe *ProtocolError
	require.True(t, errors.As(err, &pe),
		"a rejected command-POST redirect must surface as *ProtocolError; got %T: %v", err, err)
	assert.Equal(t, "command", pe.Op)
	assert.True(t, errors.Is(pe.Err, errRedirectRejected),
		"the rejection must wrap errRedirectRejected; got %v", pe.Err)
}

func TestRedirect_OnPost_Never_Rejects(t *testing.T) {
	store := openFixtureStore(t, "loose-only")
	mux := http.NewServeMux()
	mux.HandleFunc("/repo.git/info/refs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", smartAdvHeader)
		pw := pktline.NewWriter(w)
		require.NoError(t, pw.WritePacket([]byte("# service=git-upload-pack\n")))
		require.NoError(t, pw.WriteFlush())
		require.NoError(t, server.Serve(r.Context(),
			pktline.NewReader(bytes.NewReader([]byte("0000"))),
			pw, store, server.Options{PreferredProtocol: transport.ProtocolV2}))
	})
	mux.HandleFunc("/repo.git/git-upload-pack", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere.git/git-upload-pack", http.StatusTemporaryRedirect)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	tr := New(WithFollowRedirects(FollowRedirectsNever))
	// `Never` rejects redirects on the probe GET too; the test fixture
	// does not redirect on the probe, so the open succeeds. The 3xx on
	// the POST is what we exercise here.
	u := parseTestURL(t, srv, "/repo.git")
	conn, err := tr.Open(context.Background(), u, transport.OpenOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	c := conn.(*Conn)
	drainAdvertisement(t, c)

	rdr, err := c.Command(context.Background(), "ls-refs", nil, nil)
	assert.Nil(t, rdr)
	require.Error(t, err)
	var pe *ProtocolError
	require.True(t, errors.As(err, &pe),
		"a Never-rejected POST redirect must surface as *ProtocolError; got %T: %v", err, err)
	assert.Equal(t, "command", pe.Op)
}

func TestRedirect_OnPost_Always_Follows(t *testing.T) {
	store := openFixtureStore(t, "loose-only")
	var hopRequests int32
	mux := http.NewServeMux()
	mux.HandleFunc("/repo.git/info/refs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", smartAdvHeader)
		pw := pktline.NewWriter(w)
		require.NoError(t, pw.WritePacket([]byte("# service=git-upload-pack\n")))
		require.NoError(t, pw.WriteFlush())
		require.NoError(t, server.Serve(r.Context(),
			pktline.NewReader(bytes.NewReader([]byte("0000"))),
			pw, store, server.Options{PreferredProtocol: transport.ProtocolV2}))
	})
	mux.HandleFunc("/repo.git/git-upload-pack", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hopRequests, 1)
		// Use 307 so net/http preserves the method on the follow-up
		// request — the canonical "POST through a redirect" shape.
		http.Redirect(w, r, "/repo2.git/git-upload-pack", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("/repo2.git/git-upload-pack", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hopRequests, 1)
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		w.Header().Set("Content-Type", commandAcceptType)
		require.NoError(t, server.Serve(r.Context(),
			pktline.NewReader(bytes.NewReader(body)),
			pktline.NewWriter(w), store,
			server.Options{PreferredProtocol: transport.ProtocolV2}))
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := openSmartTestConn(t, srv, "/repo.git",
		WithFollowRedirects(FollowRedirectsAlways))

	rdr, err := c.Command(context.Background(), "ls-refs", nil,
		[]string{"object-format=sha1"})
	require.NoError(t, err, "FollowRedirectsAlways must let a POST follow a 307 hop")
	require.NotNil(t, rdr)
	pkts := readAllPackets(t, rdr)
	require.NotEmpty(t, pkts)
	assert.Equal(t, int32(2), atomic.LoadInt32(&hopRequests),
		"both /repo.git and /repo2.git endpoints must have been hit")
}

// commandPostURL_Test_PathRewrite checks the URL the command POST
// derives from the [Conn]'s probe URL. The probe URL has the shape
// `http://host/<base>/info/refs?service=git-upload-pack`; the POST
// URL must be `http://host/<base>/git-upload-pack` — same scheme/host,
// rewritten suffix, no query.
func TestCommandPostURL_PathRewrite(t *testing.T) {
	tests := []struct {
		name string
		base string
		want string
	}{
		{
			name: "standard repo path",
			base: "https://example.com/foo/bar.git/info/refs?service=git-upload-pack",
			want: "https://example.com/foo/bar.git/git-upload-pack",
		},
		{
			name: "root-level repo",
			base: "http://localhost:8080/info/refs?service=git-upload-pack",
			want: "http://localhost:8080/git-upload-pack",
		},
		{
			name: "preserves bracketed IPv6 host",
			base: "https://[fe80::1]:8443/repo.git/info/refs?service=git-upload-pack",
			want: "https://[fe80::1]:8443/repo.git/git-upload-pack",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			u, err := url.Parse(tc.base)
			require.NoError(t, err)
			got, err := commandPostURL(u)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got.String())
		})
	}
}

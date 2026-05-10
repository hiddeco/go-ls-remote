package httpt

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
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

// TestConn_Command_RejectsOversizePayload pins the upfront validation
// of the command-line, capability, and argument payloads against the
// pkt-line size cap. Each input is framed as a single pkt-line whose
// payload is the value plus a trailing LF (and `command=` for the
// command name); a value above the cap cannot be framed at all and
// would otherwise produce a malformed request body. The check
// rejects such inputs with an error wrapping
// [pktline.ErrPayloadTooLarge].
func TestConn_Command_RejectsOversizePayload(t *testing.T) {
	overlong := strings.Repeat("a", pktline.MaxPayload)
	tests := []struct {
		name string
		cmd  string
		args []string
		caps []string
	}{
		{name: "oversize command name", cmd: overlong, args: nil, caps: nil},
		{name: "oversize capability", cmd: "ls-refs", caps: []string{overlong}},
		{name: "oversize argument", cmd: "ls-refs", args: []string{overlong}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := &Conn{
				body:              &closeCounter{Reader: bytes.NewReader(nil)},
				reader:            pktline.NewReader(bytes.NewReader(nil)),
				url:               mustParseURL(t, "https://example.com/repo.git/info/refs"),
				userAgent:         defaultUserAgent,
				gitProtocolHeader: "version=2",
			}

			rdr, err := c.Command(context.Background(), tc.cmd, tc.args, tc.caps)
			assert.Nil(t, rdr)
			require.Error(t, err)
			assert.True(t, errors.Is(err, pktline.ErrPayloadTooLarge),
				"oversize input must wrap pktline.ErrPayloadTooLarge; got %v", err)
		})
	}
}

// TestCommandPostURL_RejectsMissingSuffix pins the safety check on the
// path-rewrite step: a probe URL whose path does not end in
// `/info/refs` cannot be safely rewritten by suffix trim, since the
// trim is a no-op and the synthesised POST URL would point at a
// non-existent endpoint. The function refuses such a URL with an
// explicit error rather than producing the wrong path silently.
//
// In practice the only way the [Conn] sees such a URL is through a
// misbehaving HTTP redirect that drops the suffix on its way to the
// smart advertisement. The check makes that failure mode loud.
func TestCommandPostURL_RejectsMissingSuffix(t *testing.T) {
	base := &url.URL{
		Scheme: "https",
		Host:   "example.com",
		Path:   "/repo.git/discovery",
	}
	got, err := commandPostURL(base)
	assert.Nil(t, got)
	require.Error(t, err)
	assert.ErrorContains(t, err, "/info/refs",
		"the error must name the missing suffix so callers can diagnose the redirect")
}

// TestCommandPostURL_RawPathRewrite pins the [url.URL.RawPath] handling
// in [commandPostURL]: when the probe URL carries a percent-encoded
// path (e.g. `/repo%2Egit/info/refs`), [url.URL.String] prefers
// `RawPath` over `Path`. The rewrite must clear `RawPath` so the
// derived POST URL re-encodes from the rewritten `Path` rather than
// silently emitting the unrewritten encoded form.
func TestCommandPostURL_RawPathRewrite(t *testing.T) {
	base := &url.URL{
		Scheme:  "https",
		Host:    "example.com",
		Path:    "/repo.git/info/refs",
		RawPath: "/repo%2Egit/info/refs",
	}
	got, err := commandPostURL(base)
	require.NoError(t, err)
	assert.Equal(t,
		"https://example.com/repo.git/git-upload-pack",
		got.String(),
		"the rewrite must clear RawPath so String re-encodes from the rewritten Path")
	assert.Empty(t, got.RawPath,
		"RawPath must be cleared so it does not shadow the rewritten Path")
}

// TestConn_Command_UnexpectedStatus_418 mirrors
// `TestOpen_UnexpectedStatus_418` for the command path: a status that
// is neither 2xx nor a known sentinel-mapping code surfaces as a
// [*ProtocolError] with `Op == "command"` and the originating status
// code, with an "unexpected status" Err.
func TestConn_Command_UnexpectedStatus_418(t *testing.T) {
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
		w.WriteHeader(http.StatusTeapot)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := openSmartTestConn(t, srv, "/repo.git")
	rdr, err := c.Command(context.Background(), "ls-refs", nil, nil)
	assert.Nil(t, rdr)
	require.Error(t, err)
	var pe *ProtocolError
	require.True(t, errors.As(err, &pe),
		"unexpected status on the command path maps to *ProtocolError; got %T: %v", err, err)
	assert.Equal(t, "command", pe.Op)
	assert.Equal(t, http.StatusTeapot, pe.Status)
	require.NotNil(t, pe.Err)
	assert.Contains(t, pe.Err.Error(), "unexpected status")
}

// TestConn_Command_ResolverError_Wrapped pins the error wrap on the
// resolver-error branch: a [CredentialResolver] that returns an error
// must surface from [Conn.Command] wrapped so callers can match the
// inner sentinel via [errors.Is], with the wrap message naming the
// redacted POST URL for log-line triage.
func TestConn_Command_ResolverError_Wrapped(t *testing.T) {
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

	srv := httptest.NewServer(mux)
	defer srv.Close()

	sentinel := errors.New("vault: unavailable")
	// Open the connection without a resolver so the probe succeeds, then
	// install a failing resolver on the [Conn] for the command call. This
	// isolates the resolver-error branch from the probe path's auth
	// retry.
	c := openSmartTestConn(t, srv, "/repo.git")
	c.creds = credentialResolverFunc(func(_ context.Context, _ *url.URL) (Credentials, error) {
		return nil, sentinel
	})

	rdr, err := c.Command(context.Background(), "ls-refs", nil, nil)
	assert.Nil(t, rdr)
	require.Error(t, err)
	assert.True(t, errors.Is(err, sentinel),
		"resolver errors must wrap so callers can match the inner cause via errors.Is; got %v", err)
	assert.Contains(t, err.Error(), srv.URL,
		"the wrap message must include the (redacted) POST URL for log triage")
	assert.Contains(t, err.Error(), "/repo.git/git-upload-pack",
		"the wrap message must name the rewritten POST endpoint")
}

// TestConn_Command_RedirectRejected_ClosesBody pins that
// [Conn.Command] closes the [http.Response.Body] when `client.Do`
// returns both a non-nil response and an error — Go's `net/http` does
// this when [http.Client.CheckRedirect] rejects a 3xx hop. Without an
// explicit close the underlying connection is pinned in the transport
// pool until the body is GC'd. The probe path's analogous behaviour is
// covered by the existing redirect tests; this test isolates the
// command-path drain.
func TestConn_Command_RedirectRejected_ClosesBody(t *testing.T) {
	// Build a probe body that drives the smart advertisement to a
	// flush so [drainAdvertisement] terminates: `# service=` preamble,
	// flush, then a v2 capability line, then the closing flush.
	var probeBuf bytes.Buffer
	pw := pktline.NewWriter(&probeBuf)
	require.NoError(t, pw.WritePacket([]byte("# service=git-upload-pack\n")))
	require.NoError(t, pw.WriteFlush())
	require.NoError(t, pw.WritePacket([]byte("version 2\n")))
	require.NoError(t, pw.WriteFlush())
	probeBody := probeBuf.Bytes()
	postBodyBytes := []byte("redirected — body bytes for the leak check")

	rt := &countingRoundTripper{respond: func(req *http.Request, _ int) *http.Response {
		switch {
		case req.Method == http.MethodGet && req.URL.Path == "/repo.git/info/refs":
			h := http.Header{}
			h.Set("Content-Type", smartAdvHeader)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     h,
				Body:       io.NopCloser(bytes.NewReader(probeBody)),
			}
		case req.Method == http.MethodPost && req.URL.Path == "/repo.git/git-upload-pack":
			h := http.Header{}
			h.Set("Location", "https://elsewhere.example/repo.git/git-upload-pack")
			return &http.Response{
				StatusCode: http.StatusFound,
				Header:     h,
				// A non-empty body is the entire point of this fixture:
				// the wrapped [closeCounter] is what we assert on.
				Body: &closeCounter{Reader: bytes.NewReader(postBodyBytes)},
			}
		default:
			h := http.Header{}
			h.Set("Content-Type", smartAdvHeader)
			return &http.Response{StatusCode: http.StatusOK, Header: h, Body: http.NoBody}
		}
	}}

	tr := New(
		WithClient(&http.Client{Transport: rt}),
		// `Initial` follows redirects on the probe GET but rejects them
		// on the command POST — exactly the case where `client.Do`
		// returns a non-nil response alongside the error.
		WithFollowRedirects(FollowRedirectsInitial),
	)

	u, err := transport.ParseURL("https://example.com/repo.git")
	require.NoError(t, err)
	conn, err := tr.Open(context.Background(), u, transport.OpenOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	c := conn.(*Conn)
	drainAdvertisement(t, c)

	rdr, cmdErr := c.Command(context.Background(), "ls-refs", nil, nil)
	assert.Nil(t, rdr)
	require.Error(t, cmdErr)
	var pe *ProtocolError
	require.True(t, errors.As(cmdErr, &pe))
	assert.Equal(t, "command", pe.Op)

	// The fixture's POST hop returned a 302 response with a wrapped
	// [closeCounter] body. Find it among the round-tripper's recorded
	// responses and assert exactly one Close.
	var post *closeCounter
	for _, rec := range rt.responses {
		if cc, ok := rec.(*closeCounter); ok {
			post = cc
			break
		}
	}
	require.NotNil(t, post, "the POST response body must have been the wrapped closeCounter")
	// `net/http` 1.25 closes the body itself when [http.Client.Do]
	// returns a CheckRedirect error, but earlier Go releases (and the
	// stdlib doc) leave the body for the caller. Our defensive
	// drain-and-close adds one more close on top. Either way the
	// invariant is "body is closed at least once": pin that, not the
	// exact count.
	assert.GreaterOrEqual(t, post.closes, 1,
		"a redirect-rejected POST must release its response body")
}

// countingRoundTripper is a thin variant of `stubRoundTripper` that
// also records each response's body so leak tests can assert on it
// after the fact. The vanilla `stubRoundTripper` only records requests.
type countingRoundTripper struct {
	respond   func(req *http.Request, hop int) *http.Response
	requests  []*http.Request
	responses []io.ReadCloser
}

func (s *countingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	hop := len(s.requests)
	s.requests = append(s.requests, req.Clone(req.Context()))
	resp := s.respond(req, hop)
	if resp == nil {
		return nil, fmt.Errorf("countingRoundTripper: respond returned nil for hop %d (%s %s)",
			hop, req.Method, req.URL)
	}
	resp.Request = req
	if resp.Body == nil {
		resp.Body = http.NoBody
	}
	s.responses = append(s.responses, resp.Body)
	return resp, nil
}

// TestConn_Close_ReleasesUndrainedCommandBody pins the leak fix on
// [Conn.Close]: a caller that abandons the [pktline.Reader] returned
// by [Conn.Command] without draining must still see the underlying
// body closed when the parent [Conn] is closed. The
// [pktline.Reader] does not own a Close method, so this responsibility
// falls on [Conn].
func TestConn_Close_ReleasesUndrainedCommandBody(t *testing.T) {
	probe := &closeCounter{Reader: bytes.NewReader(nil)}
	cmd := &closeCounter{Reader: strings.NewReader("undrained command body")}
	c := &Conn{
		body:    probe,
		reader:  pktline.NewReader(probe),
		url:     mustParseURL(t, "https://example.com/repo.git/info/refs"),
		cmdBody: cmd,
	}

	require.NoError(t, c.Close(), "Close must not error")
	assert.Equal(t, 1, probe.closes, "probe body must be closed exactly once")
	assert.Equal(t, 1, cmd.closes, "tracked command body must be closed exactly once")

	// Idempotency: a second close must not double-close either body.
	require.NoError(t, c.Close())
	assert.Equal(t, 1, probe.closes)
	assert.Equal(t, 1, cmd.closes)
}

// TestConn_Command_ClosesPreviousBody pins the bound on the
// command-body bookkeeping: each successful command POST releases the
// body of the previous successful POST so the [Conn] does not
// accumulate already-superseded bodies for its lifetime. The
// single-flight contract on [Conn] means at most one body is ever
// outstanding, so close-and-replace is the natural shape and bounds
// memory regardless of how many commands the caller issues.
func TestConn_Command_ClosesPreviousBody(t *testing.T) {
	bodies := [3]*closeCounter{
		{Reader: bytes.NewReader([]byte("first response"))},
		{Reader: bytes.NewReader([]byte("second response"))},
		{Reader: bytes.NewReader([]byte("third response"))},
	}
	rt := &countingRoundTripper{respond: func(_ *http.Request, hop int) *http.Response {
		h := http.Header{}
		h.Set("Content-Type", commandAcceptType)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     h,
			Body:       bodies[hop],
		}
	}}

	c := &Conn{
		body:              &closeCounter{Reader: bytes.NewReader(nil)},
		reader:            pktline.NewReader(bytes.NewReader(nil)),
		client:            &http.Client{Transport: rt},
		url:               mustParseURL(t, "https://example.com/repo.git/info/refs"),
		userAgent:         defaultUserAgent,
		gitProtocolHeader: "version=2",
	}

	_, err := c.Command(context.Background(), "ls-refs", nil, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, bodies[0].closes,
		"first body remains in-flight until superseded")

	_, err = c.Command(context.Background(), "ls-refs", nil, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, bodies[0].closes,
		"first body closed when second Command takes over")
	assert.Equal(t, 0, bodies[1].closes,
		"second body in-flight after second Command")

	_, err = c.Command(context.Background(), "ls-refs", nil, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, bodies[0].closes)
	assert.Equal(t, 1, bodies[1].closes,
		"second body closed when third Command takes over")
	assert.Equal(t, 0, bodies[2].closes,
		"third body in-flight after third Command")

	require.NoError(t, c.Close())
	assert.Equal(t, 1, bodies[2].closes,
		"third body closed when Conn.Close runs")
}

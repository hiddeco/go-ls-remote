package lsremote

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/transport"
	httpt "github.com/hiddeco/go-ls-remote/transport/http"
)

// closeRecordingTransport wraps the HTTP [transport.Transport] so a
// test can observe how many times the returned [transport.Conn] is
// closed. The wrapping is transparent: `Open` delegates to the inner
// transport and yields a [closeRecordingConn] over the resulting
// [transport.Conn] with a shared atomic counter. The counter is
// exposed via [closeRecordingTransport.closeCount].
type closeRecordingTransport struct {
	inner transport.Transport
	count atomic.Int64
}

// newCloseRecordingRegistry returns a `*transport.Registry` that
// resolves `https` and `http` to a `closeRecordingTransport` wrapping
// the package's default HTTP transport, plus the recording wrapper
// itself so the caller can read `closeCount()`.
func newCloseRecordingRegistry(t *testing.T) (*transport.Registry, *closeRecordingTransport) {
	t.Helper()
	rec := &closeRecordingTransport{inner: httpt.New()}
	return transport.NewRegistry(rec), rec
}

func (r *closeRecordingTransport) Schemes() []string { return r.inner.Schemes() }

func (r *closeRecordingTransport) Open(ctx context.Context, u *transport.URL,
	opts transport.OpenOptions) (transport.Conn, error) {
	conn, err := r.inner.Open(ctx, u, opts)
	if err != nil {
		return nil, err
	}
	return &closeRecordingConn{inner: conn, count: &r.count}, nil
}

func (r *closeRecordingTransport) closeCount() int { return int(r.count.Load()) }

// closeRecordingConn delegates every method to the inner
// [transport.Conn] and increments the shared counter on `Close`. The
// counter increment lives outside `inner.Close()` so a re-entrant
// `Close` (the idempotency contract permits one) still records the
// extra call — which is what the test wants to assert against.
type closeRecordingConn struct {
	inner transport.Conn
	count *atomic.Int64
}

func (c *closeRecordingConn) Advertisement() *pktline.Reader { return c.inner.Advertisement() }
func (c *closeRecordingConn) Command(ctx context.Context, name string,
	body transport.CommandBody) (*pktline.Reader, error) {
	return c.inner.Command(ctx, name, body)
}
func (c *closeRecordingConn) Close() error {
	c.count.Add(1)
	return c.inner.Close()
}

// TestRefs_topLevel pins the one-shot `Refs` helper's happy path: it
// dials the in-process v2 server, returns an iterator over the
// advertised refs, and yields at least HEAD and `refs/heads/main` with
// no error.
func TestRefs_topLevel(t *testing.T) {
	store, _ := openObjectInfoFixture(t)
	srv := httptest.NewServer(serveHandlerV2(t, store, "/repo.git"))
	defer srv.Close()

	seq, err := Refs(context.Background(), srv.URL+"/repo.git", RefsRequest{})
	require.NoError(t, err)

	var got []Ref
	for ref, err := range seq {
		require.NoError(t, err)
		got = append(got, ref)
	}
	require.NotEmpty(t, got)

	var sawHEAD, sawMain bool
	for _, r := range got {
		switch r.Name {
		case "HEAD":
			sawHEAD = true
		case "refs/heads/main":
			sawMain = true
		}
	}
	assert.True(t, sawHEAD, "the one-shot Refs must yield HEAD")
	assert.True(t, sawMain, "the one-shot Refs must yield refs/heads/main")
}

// TestRefs_topLevel_closesSessionOnDrain pins the lifecycle contract:
// after the caller drains the iterator returned by `Refs`, the
// underlying Session must close its `transport.Conn` exactly once. The
// test plugs a `closeRecordingTransport` into the registry so it can
// observe the `Close` count without reaching into Session internals.
func TestRefs_topLevel_closesSessionOnDrain(t *testing.T) {
	store, _ := openObjectInfoFixture(t)
	srv := httptest.NewServer(serveHandlerV2(t, store, "/repo.git"))
	defer srv.Close()

	reg, rec := newCloseRecordingRegistry(t)

	seq, err := Refs(context.Background(), srv.URL+"/repo.git",
		RefsRequest{}, WithTransports(reg))
	require.NoError(t, err)

	var n int
	for _, err := range seq {
		require.NoError(t, err)
		n++
	}
	assert.Greater(t, n, 0, "the iterator must yield at least one ref")
	assert.Equal(t, 1, rec.closeCount(),
		"the underlying Conn must be closed exactly once when the iter drains")
}

// TestRefs_topLevel_closesSessionOnEarlyStop pins that an early `break`
// from the caller's `for range` still closes the Session: the wrapper's
// `defer Close` must fire when `yield` returns false.
func TestRefs_topLevel_closesSessionOnEarlyStop(t *testing.T) {
	store, _ := openObjectInfoFixture(t)
	srv := httptest.NewServer(serveHandlerV2(t, store, "/repo.git"))
	defer srv.Close()

	reg, rec := newCloseRecordingRegistry(t)

	seq, err := Refs(context.Background(), srv.URL+"/repo.git",
		RefsRequest{}, WithTransports(reg))
	require.NoError(t, err)

	// Take exactly one ref and bail.
	var taken int
	for range seq {
		taken++
		break
	}
	assert.Equal(t, 1, taken, "the iter must yield at least one ref before the early stop")
	assert.Equal(t, 1, rec.closeCount(),
		"an early-stop iter must still close the underlying Conn")
}

// TestRefs_topLevel_dialError propagates a dial-time failure verbatim:
// the helper returns `(nil, err)` and does not open or close any
// Session.
func TestRefs_topLevel_dialError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repo.git/info/refs", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	seq, err := Refs(context.Background(), srv.URL+"/repo.git", RefsRequest{})
	assert.Nil(t, seq)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotFound),
		"a 404 on the discovery probe must reach ErrNotFound via errors.Is")
}

// TestListRefs_topLevel pins the slice-collecting one-shot: it returns
// every advertised ref in a single slice, and the in-process server's
// HEAD and main both appear.
func TestListRefs_topLevel(t *testing.T) {
	store, _ := openObjectInfoFixture(t)
	srv := httptest.NewServer(serveHandlerV2(t, store, "/repo.git"))
	defer srv.Close()

	refs, err := ListRefs(context.Background(), srv.URL+"/repo.git", RefsRequest{})
	require.NoError(t, err)
	require.NotEmpty(t, refs)

	var names []string
	for _, r := range refs {
		names = append(names, r.Name)
	}
	assert.Contains(t, names, "HEAD")
	assert.Contains(t, names, "refs/heads/main")
}

// TestObjectInfos_topLevel pins the one-shot `object-info` helper:
// against a v2 server, a query for a packed commit OID with `Size:
// true` returns one row whose Hash matches and whose Size is positive.
func TestObjectInfos_topLevel(t *testing.T) {
	store, commitOID := openObjectInfoFixture(t)
	srv := httptest.NewServer(serveHandlerV2(t, store, "/repo.git"))
	defer srv.Close()

	got, err := ObjectInfos(context.Background(), srv.URL+"/repo.git",
		[]string{commitOID}, ObjectInfoRequest{Size: true})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, commitOID, got[0].Hash)
	assert.Greater(t, got[0].Size, int64(0))
}

// TestExists_success pins the success branch: a reachable repository
// produces `(true, nil)`.
func TestExists_success(t *testing.T) {
	store, _ := openObjectInfoFixture(t)
	srv := httptest.NewServer(serveHandlerV2(t, store, "/repo.git"))
	defer srv.Close()

	ok, err := Exists(context.Background(), srv.URL+"/repo.git")
	require.NoError(t, err)
	assert.True(t, ok, "a reachable v2 repo must produce Exists=true")
}

// TestExists_notFound pins the `ErrNotFound` branch: a 404 on the
// discovery probe collapses to `(false, nil)` without surfacing the
// underlying error.
func TestExists_notFound(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repo.git/info/refs", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ok, err := Exists(context.Background(), srv.URL+"/repo.git")
	require.NoError(t, err, "ErrNotFound must collapse to (false, nil), not propagate")
	assert.False(t, ok)
}

// TestExists_otherError pins the catch-all branch: an error that does
// NOT match `ErrNotFound` propagates verbatim with `(false, err)`. A
// malformed URL — empty rawURL — triggers `transport.ParseURL` to
// return `transport.ErrEmptyURL`, which is not an `ErrNotFound`.
func TestExists_otherError(t *testing.T) {
	ok, err := Exists(context.Background(), "")
	require.Error(t, err)
	assert.False(t, ok)
	assert.False(t, errors.Is(err, ErrNotFound),
		"the empty-URL error must not collapse to ErrNotFound")
}

// TestDefaultBranch_v2 pins the v2 path: the helper issues `ls-refs`
// with `ref-prefix HEAD` and `symrefs`, finds HEAD's symref target, and
// returns `refs/heads/main`.
func TestDefaultBranch_v2(t *testing.T) {
	store, _ := openObjectInfoFixture(t)
	srv := httptest.NewServer(serveHandlerV2(t, store, "/repo.git"))
	defer srv.Close()

	got, err := DefaultBranch(context.Background(), srv.URL+"/repo.git")
	require.NoError(t, err)
	assert.Equal(t, "refs/heads/main", got)
}

// TestDefaultBranch_v0 pins the v0 fallback: a v0 server advertises
// `symref=HEAD:refs/heads/main` on its capability list, and the helper
// resolves the target without issuing any command.
func TestDefaultBranch_v0(t *testing.T) {
	store, _ := openObjectInfoFixture(t)
	srv := httptest.NewServer(serveHandlerV0(t, store, "/repo.git"))
	defer srv.Close()

	got, err := DefaultBranch(context.Background(), srv.URL+"/repo.git")
	require.NoError(t, err)
	assert.Equal(t, "refs/heads/main", got)
}

// TestDefaultBranch_v2_unborn pins that an unborn HEAD on a v2 server
// surfaces its symref target rather than collapsing to
// [ErrNoDefaultBranch]. Canonical Git's discovery client passes the
// `unborn` argument on its `ls-refs` request ([connect.c:591-592]),
// gated by the server's `ls-refs=unborn` capability advertisement;
// without it, the server's [ls-refs.c:135-136] skips HEAD entirely on
// an unborn repository. `DefaultBranch` must mirror canonical Git and
// set `Unborn: true` on its internal request so the symref target is
// reported on every repository state — born, detached, or unborn.
//
// [connect.c:591-592]: https://github.com/git/git/blob/v2.54.0/connect.c#L591-L592
// [ls-refs.c:135-136]: https://github.com/git/git/blob/v2.54.0/ls-refs.c#L135-L136
func TestDefaultBranch_v2_unborn(t *testing.T) {
	store := openFixtureStore(t, "unborn-head")
	srv := httptest.NewServer(serveHandlerV2(t, store, "/repo.git"))
	defer srv.Close()

	got, err := DefaultBranch(context.Background(), srv.URL+"/repo.git")
	require.NoError(t, err,
		"unborn HEAD must surface its symref target, not ErrNoDefaultBranch")
	assert.Equal(t, "refs/heads/main", got,
		"the unborn-head fixture's HEAD points at refs/heads/main")
}

// TestDefaultBranch_v2_noSymref pins the error path for a v2 server
// that does not attach a `symref-target:` attribute to HEAD and has no
// capability-level symref entry. The helper must return a
// [*ProtocolError] whose chain matches [ErrNoDefaultBranch] (the
// repository is reachable but HEAD has no symbolic target) and must NOT
// match [ErrNotFound] (which is reserved for "repository missing").
// The [*ProtocolError.Op] must be `"ls-refs"` because the failure is
// detected after the `ls-refs` command exchange.
func TestDefaultBranch_v2_noSymref(t *testing.T) {
	conn := &commandStubConn{
		adv:    pktline.NewReader(bytes.NewReader(buildV2Advertisement(t))),
		cmdRdr: pktline.NewReader(bytes.NewReader(buildV2LSRefsNoSymrefResponse(t))),
	}
	cap := &captureTransport{schemes: []string{"https"}, conn: conn}
	reg := transport.NewRegistry(cap)

	_, err := DefaultBranch(context.Background(), "https://example.com/repo.git",
		WithTransports(reg))
	require.Error(t, err)

	assert.True(t, errors.Is(err, ErrNoDefaultBranch),
		"v2 headless must match ErrNoDefaultBranch via errors.Is; got %v", err)
	assert.False(t, errors.Is(err, ErrNotFound),
		"v2 headless must NOT match ErrNotFound; got %v", err)

	var pe *ProtocolError
	require.True(t, errors.As(err, &pe),
		"v2 headless must surface as *ProtocolError; got %T", err)
	assert.Equal(t, "ls-refs", pe.Op,
		"v2 failure is detected after the ls-refs exchange; Op must be \"ls-refs\"")
}

// TestDefaultBranch_v0_noSymref pins the error path for a v0 server
// whose capability list omits the `symref=HEAD:...` entry. The helper
// must return [ErrNoDefaultBranch] and the [*ProtocolError.Op] must be
// `"advertisement"` because the failure is detected at capability-scan
// time, not via a command.
func TestDefaultBranch_v0_noSymref(t *testing.T) {
	conn := &stubConn{
		adv: pktline.NewReader(bytes.NewReader(buildV0NoSymrefAdvertisement(t))),
	}
	cap := &captureTransport{schemes: []string{"https"}, conn: conn}
	reg := transport.NewRegistry(cap)

	_, err := DefaultBranch(context.Background(), "https://example.com/repo.git",
		WithTransports(reg))
	require.Error(t, err)

	assert.True(t, errors.Is(err, ErrNoDefaultBranch),
		"v0 headless must match ErrNoDefaultBranch via errors.Is; got %v", err)
	assert.False(t, errors.Is(err, ErrNotFound),
		"v0 headless must NOT match ErrNotFound; got %v", err)

	var pe *ProtocolError
	require.True(t, errors.As(err, &pe),
		"v0 headless must surface as *ProtocolError; got %T", err)
	assert.Equal(t, "advertisement", pe.Op,
		"v0 failure is detected at advertisement time; Op must be \"advertisement\"")
}

// TestDefaultBranch_repoNotFound pins that a dial failure caused by a
// missing repository (404 on the discovery probe) still surfaces
// [ErrNotFound], NOT [ErrNoDefaultBranch]. The two sentinels are
// mutually exclusive: [ErrNotFound] means the repository itself is
// absent, while [ErrNoDefaultBranch] means the repository is present
// but HEAD has no symbolic target.
func TestDefaultBranch_repoNotFound(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repo.git/info/refs", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	_, err := DefaultBranch(context.Background(), srv.URL+"/repo.git")
	require.Error(t, err)

	assert.True(t, errors.Is(err, ErrNotFound),
		"a 404 on discovery must reach ErrNotFound via errors.Is; got %v", err)
	assert.False(t, errors.Is(err, ErrNoDefaultBranch),
		"a 404 on discovery must NOT match ErrNoDefaultBranch; got %v", err)
}

// TestTags_topLevel pins the `Tags` shorthand: it restricts the
// response to `refs/tags/` and asks for peeled values. The
// `packed-only` fixture has an annotated `refs/tags/v1` with a peel
// recorded in `packed-refs`, so the response carries `Peeled` populated.
func TestTags_topLevel(t *testing.T) {
	store := openFixtureStore(t, "packed-only")
	srv := httptest.NewServer(serveHandlerV2(t, store, "/repo.git"))
	defer srv.Close()

	seq, err := Tags(context.Background(), srv.URL+"/repo.git")
	require.NoError(t, err)

	var got []Ref
	for ref, err := range seq {
		require.NoError(t, err)
		got = append(got, ref)
	}
	require.NotEmpty(t, got, "the packed-only fixture has refs/tags/v1")

	var sawTag bool
	for _, r := range got {
		assert.True(t, len(r.Name) >= len("refs/tags/") && r.Name[:len("refs/tags/")] == "refs/tags/",
			"every Tags ref must live under refs/tags/; got %q", r.Name)
		if r.Name == "refs/tags/v1" {
			sawTag = true
			assert.NotEmpty(t, r.Peeled,
				"an annotated tag fetched via Tags must carry a non-empty Peeled value")
		}
	}
	assert.True(t, sawTag, "the packed-only fixture must surface refs/tags/v1")
}

// TestHeads_topLevel pins the `Heads` shorthand: it restricts the
// response to `refs/heads/` so neither HEAD nor any tag survives.
func TestHeads_topLevel(t *testing.T) {
	store, _ := openObjectInfoFixture(t)
	srv := httptest.NewServer(serveHandlerV2(t, store, "/repo.git"))
	defer srv.Close()

	seq, err := Heads(context.Background(), srv.URL+"/repo.git")
	require.NoError(t, err)

	var got []Ref
	for ref, err := range seq {
		require.NoError(t, err)
		got = append(got, ref)
	}
	require.NotEmpty(t, got)
	for _, r := range got {
		assert.True(t, len(r.Name) >= len("refs/heads/") && r.Name[:len("refs/heads/")] == "refs/heads/",
			"Heads must restrict to refs/heads/; got %q", r.Name)
	}
}

package lsremote

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
	"github.com/hiddeco/go-ls-remote/internal/objstore"
	"github.com/hiddeco/go-ls-remote/pktline"
)

// openObjectInfoFixture builds an on-disk store rooted at a fresh
// `t.TempDir()` that combines a real ref with a real packable object so
// both `ls-refs` (refs/heads/main) and `object-info` (a packed commit
// OID) return non-empty results against the in-process server.
//
// The shape mirrors `internal/server/concurrency_test.go::openConcurrentSessionsFixture`:
// HEAD is symbolic to `refs/heads/main`, `packed-refs` pins
// `refs/heads/main` at the canonical `three-objects` pack's commit, and
// the pack/idx pair from `testdata/objfmt/` lives under `objects/pack/`.
func openObjectInfoFixture(t *testing.T) (store *objstore.Store[objfmt.SHA1Hash], commitOID string) {
	t.Helper()

	// The canonical commit OID inside the `three-objects` pack; mirrors
	// `internal/server/object_info_test.go::packCommitOID`.
	const packCommitOID = "26dae744f51e61913f50bd402cbe63953c7d637b"

	root := t.TempDir()
	gitDir := filepath.Join(root, ".git")
	require.NoError(t, os.MkdirAll(filepath.Join(gitDir, "objects", "pack"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(gitDir, "refs"), 0o755))

	require.NoError(t, os.WriteFile(filepath.Join(gitDir, "HEAD"),
		[]byte("ref: refs/heads/main\n"), 0o644))

	packedRefs := "" +
		"# pack-refs with: peeled fully-peeled sorted\n" +
		packCommitOID + " refs/heads/main\n"
	require.NoError(t, os.WriteFile(
		filepath.Join(gitDir, "packed-refs"),
		[]byte(packedRefs), 0o644))

	// `testdata/objfmt/three-objects.{pack,idx}` lives at the module
	// root. The test binary's working directory is the package dir, so
	// climb two levels to reach the module root.
	wd, err := os.Getwd()
	require.NoError(t, err)
	objfmtSrc := filepath.Join(wd, "testdata", "objfmt")
	for _, name := range []string{
		"three-objects.pack", "three-objects.idx",
	} {
		src, err := os.Open(filepath.Join(objfmtSrc, name))
		require.NoError(t, err)
		dst, err := os.OpenFile(
			filepath.Join(gitDir, "objects", "pack", name),
			os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		require.NoError(t, err)
		_, err = io.Copy(dst, src)
		require.NoError(t, err)
		require.NoError(t, src.Close())
		require.NoError(t, dst.Close())
	}

	s, err := objstore.Open[objfmt.SHA1Hash](gitDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s, packCommitOID
}

// TestSession_Capabilities_returnsDeepCopy pins the deep-copy contract:
// the value returned by `*Session.Capabilities` must be independent of
// the session's internal snapshot, so a caller mutating any slice or
// map on the returned struct cannot corrupt later observations.
func TestSession_Capabilities_returnsDeepCopy(t *testing.T) {
	store := openFixtureStore(t, "loose-only")
	srv := httptest.NewServer(serveHandlerV2(t, store, "/repo.git"))
	defer srv.Close()

	s, err := Dial(context.Background(), srv.URL+"/repo.git")
	require.NoError(t, err)
	defer func() { _ = s.Close() }()

	c1 := s.Capabilities()
	require.NotEmpty(t, c1.Commands,
		"v2 advertisement must populate at least one command in Capabilities.Commands")
	require.NotEmpty(t, c1.Raw,
		"the verbatim capability map must be populated for a v2 advertisement")

	// Mutate every slice/map on the returned copy.
	c1.Commands[0] = "MUTATED"
	c1.Commands = append(c1.Commands, "ADDED-CMD")
	c1.LSRefsArgs = append(c1.LSRefsArgs, "ADDED-LSREFS-ARG")
	c1.ObjectInfoArgs = append(c1.ObjectInfoArgs, "ADDED-OI-ARG")
	c1.FetchArgs = append(c1.FetchArgs, "ADDED-FETCH-ARG")
	c1.Symrefs = append(c1.Symrefs, Symref{Name: "ADDED", Target: "ADDED-TGT"})
	c1.Raw["agent"] = []string{"MUTATED-AGENT"}
	c1.Raw["MUTATED-KEY"] = []string{"x"}

	c2 := s.Capabilities()
	assert.NotEqual(t, "MUTATED", c2.Commands[0],
		"the snapshot's Commands slice must not alias the returned copy")
	assert.NotContains(t, c2.Commands, "ADDED-CMD",
		"appending to the returned Commands must not extend the snapshot")
	assert.NotContains(t, c2.LSRefsArgs, "ADDED-LSREFS-ARG")
	assert.NotContains(t, c2.ObjectInfoArgs, "ADDED-OI-ARG")
	assert.NotContains(t, c2.FetchArgs, "ADDED-FETCH-ARG")
	for _, sr := range c2.Symrefs {
		assert.NotEqual(t, "ADDED", sr.Name)
	}
	if v, ok := c2.Raw["agent"]; ok {
		assert.NotEqual(t, []string{"MUTATED-AGENT"}, v,
			"mutating Raw[agent] on the returned copy must not affect the snapshot")
	}
	_, hasMutatedKey := c2.Raw["MUTATED-KEY"]
	assert.False(t, hasMutatedKey,
		"adding a key to the returned Raw map must not propagate to the snapshot")
}

// TestSession_Refs_v2 pins the v2 happy path: `*Session.Refs` issues an
// `ls-refs` command and the iterator yields at least HEAD and the
// fixture's single branch with no errors.
func TestSession_Refs_v2(t *testing.T) {
	store, _ := openObjectInfoFixture(t)
	srv := httptest.NewServer(serveHandlerV2(t, store, "/repo.git"))
	defer srv.Close()

	s, err := Dial(context.Background(), srv.URL+"/repo.git")
	require.NoError(t, err)
	defer func() { _ = s.Close() }()

	seq, err := s.Refs(context.Background(), RefsArgs{})
	require.NoError(t, err)

	var got []Ref
	for ref, err := range seq {
		require.NoError(t, err)
		got = append(got, ref)
	}
	require.NotEmpty(t, got, "ls-refs against a populated repo must yield at least one ref")

	var sawHead, sawMain bool
	for _, ref := range got {
		switch ref.Name {
		case "HEAD":
			sawHead = true
		case "refs/heads/main":
			sawMain = true
		}
	}
	assert.True(t, sawHead, "v2 ls-refs must yield HEAD by default")
	assert.True(t, sawMain, "v2 ls-refs must yield refs/heads/main")
}

// TestSession_Refs_v2_prefixesAndSymrefs pins that `RefsArgs.Prefixes`
// flows to the server as one `ref-prefix` arg per element and
// `RefsArgs.Symrefs` populates `Ref.Symref` on HEAD. The fixture
// advertises a single branch, so a prefix of `refs/heads/` admits
// refs/heads/main but not HEAD (which is not under that namespace).
func TestSession_Refs_v2_prefixesAndSymrefs(t *testing.T) {
	store, _ := openObjectInfoFixture(t)
	srv := httptest.NewServer(serveHandlerV2(t, store, "/repo.git"))
	defer srv.Close()

	s, err := Dial(context.Background(), srv.URL+"/repo.git")
	require.NoError(t, err)
	defer func() { _ = s.Close() }()

	// First: bound the response to `refs/heads/` only.
	seq, err := s.Refs(context.Background(), RefsArgs{
		Prefixes: []string{"refs/heads/"},
	})
	require.NoError(t, err)
	var got []Ref
	for ref, err := range seq {
		require.NoError(t, err)
		got = append(got, ref)
	}
	require.NotEmpty(t, got)
	for _, ref := range got {
		assert.Equal(t, "refs/heads/main", ref.Name,
			"ref-prefix refs/heads/ must restrict the response to refs/heads/ entries")
	}

	// Second: ask for symrefs and confirm HEAD carries Symref =
	// refs/heads/main.
	seq, err = s.Refs(context.Background(), RefsArgs{Symrefs: true})
	require.NoError(t, err)
	var sawHEADSymref bool
	for ref, err := range seq {
		require.NoError(t, err)
		if ref.Name == "HEAD" {
			assert.Equal(t, "refs/heads/main", ref.Symref,
				"RefsArgs.Symrefs must flow to the server and populate Ref.Symref on HEAD")
			sawHEADSymref = true
		}
	}
	assert.True(t, sawHEADSymref, "the symref-target attribute must reach HEAD on v2")
}

// TestSession_Refs_v0_clientSideFilter pins the v0/v1 path: the cached
// advertisement-time slice is filtered client-side by `RefsArgs.Prefixes`,
// no command is issued, and no error is returned.
func TestSession_Refs_v0_clientSideFilter(t *testing.T) {
	store, _ := openObjectInfoFixture(t)
	srv := httptest.NewServer(serveHandlerV0(t, store, "/repo.git"))
	defer srv.Close()

	s, err := Dial(context.Background(), srv.URL+"/repo.git")
	require.NoError(t, err)
	defer func() { _ = s.Close() }()
	require.Equal(t, ProtocolV0, s.Capabilities().Version)

	// No filter: every cached ref comes back, including HEAD.
	seq, err := s.Refs(context.Background(), RefsArgs{})
	require.NoError(t, err)
	var all []Ref
	for ref, err := range seq {
		require.NoError(t, err)
		all = append(all, ref)
	}
	require.NotEmpty(t, all)

	// Filter on `refs/heads/`: HEAD is dropped, refs/heads/main survives.
	seq, err = s.Refs(context.Background(), RefsArgs{
		Prefixes: []string{"refs/heads/"},
	})
	require.NoError(t, err)
	var filtered []Ref
	for ref, err := range seq {
		require.NoError(t, err)
		filtered = append(filtered, ref)
	}
	require.NotEmpty(t, filtered)
	for _, ref := range filtered {
		assert.True(t, ref.Name != "HEAD",
			"client-side prefix filter on v0 must drop HEAD; got %q", ref.Name)
		assert.Contains(t, ref.Name, "refs/heads/",
			"every retained ref must carry the requested prefix; got %q", ref.Name)
	}
}

// TestSession_ListRefs pins the slice-collection helper: it drains the
// iterator and returns the same refs.
func TestSession_ListRefs(t *testing.T) {
	store, _ := openObjectInfoFixture(t)
	srv := httptest.NewServer(serveHandlerV2(t, store, "/repo.git"))
	defer srv.Close()

	s, err := Dial(context.Background(), srv.URL+"/repo.git")
	require.NoError(t, err)
	defer func() { _ = s.Close() }()

	refs, err := s.ListRefs(context.Background(), RefsArgs{})
	require.NoError(t, err)
	require.NotEmpty(t, refs)

	var names []string
	for _, r := range refs {
		names = append(names, r.Name)
	}
	assert.Contains(t, names, "HEAD")
	assert.Contains(t, names, "refs/heads/main")
}

// TestSession_ObjectInfo_v2 pins the v2 object-info happy path: a query
// for a packed commit OID with `Size: true` returns one row whose Hash
// matches the request and whose Size is strictly positive.
func TestSession_ObjectInfo_v2(t *testing.T) {
	store, commitOID := openObjectInfoFixture(t)
	srv := httptest.NewServer(serveHandlerV2(t, store, "/repo.git"))
	defer srv.Close()

	s, err := Dial(context.Background(), srv.URL+"/repo.git")
	require.NoError(t, err)
	defer func() { _ = s.Close() }()

	got, err := s.ObjectInfo(context.Background(),
		[]string{commitOID}, ObjectInfoArgs{Size: true})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, commitOID, got[0].Hash)
	assert.Greater(t, got[0].Size, int64(0),
		"a real packed commit must report a strictly positive size")
}

// TestSession_ObjectInfo_unsupportedOnV0 pins the v0 gate: `object-info`
// is a v2-only command, so a v0-negotiated session must return a
// `*ProtocolError` whose chain matches `ErrUnsupportedProtocol`.
func TestSession_ObjectInfo_unsupportedOnV0(t *testing.T) {
	store, commitOID := openObjectInfoFixture(t)
	srv := httptest.NewServer(serveHandlerV0(t, store, "/repo.git"))
	defer srv.Close()

	s, err := Dial(context.Background(), srv.URL+"/repo.git")
	require.NoError(t, err)
	defer func() { _ = s.Close() }()
	require.Equal(t, ProtocolV0, s.Capabilities().Version)

	got, err := s.ObjectInfo(context.Background(),
		[]string{commitOID}, ObjectInfoArgs{Size: true})
	assert.Nil(t, got)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrUnsupportedProtocol),
		"object-info on v0 must match ErrUnsupportedProtocol; got %v", err)

	var pe *ProtocolError
	require.True(t, errors.As(err, &pe))
	assert.Equal(t, "object-info", pe.Op)
}

// TestSession_ObjectInfo_sizeFalseSetsNegativeOne pins the public
// "size not requested" translation: when `ObjectInfoArgs.Size` is
// false, every returned `ObjectInfo.Size` must be `-1` regardless of
// what the wire layer reported. The test drives the Session through a
// stub `transport.Conn` so the wire response is pinned bytes: an empty
// `attrs` line per `gitprotocol-v2.adoc` §"object-info" grammar
// (`info = PKT-LINE(attrs LF) ...`), one `<oid>\n` per OID, and a
// flush.
//
// A stub is used (rather than the in-process server) because the
// in-process server omits the attrs line entirely on the no-`size`
// branch — matching canonical Git's `protocol-caps.c::send_info` —
// while the wire decoder assumes an attrs line is always present.
// Reconciling that pre-existing mismatch is out of scope for this
// task; the unit-shaped test below verifies the public translation
// without depending on it.
func TestSession_ObjectInfo_sizeFalseSetsNegativeOne(t *testing.T) {
	commitOID := "26dae744f51e61913f50bd402cbe63953c7d637b"

	// Build a synthetic v2 object-info response with an empty attrs
	// line so the wire decoder treats subsequent rows as per-OID.
	var resp bytes.Buffer
	pw := pktline.NewWriter(&resp)
	require.NoError(t, pw.WritePacket([]byte("\n")))
	require.NoError(t, pw.WritePacket([]byte(commitOID+"\n")))
	require.NoError(t, pw.WriteFlush())

	conn := &fakeCommandConn{cmdResp: resp.Bytes()}
	s := &Session{
		conn: conn,
		caps: Capabilities{
			Version:      ProtocolV2,
			ObjectFormat: ObjectFormatSHA1,
			Raw:          map[string][]string{"object-info": {""}},
		},
	}

	got, err := s.ObjectInfo(context.Background(),
		[]string{commitOID}, ObjectInfoArgs{})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, commitOID, got[0].Hash)
	assert.Equal(t, int64(-1), got[0].Size,
		"Size: false must translate to ObjectInfo.Size == -1")

	// And confirm the wire request did NOT carry a `size` argument: the
	// public contract leaks no Size-related capability the caller did
	// not opt into.
	assert.Equal(t, "object-info", conn.lastCmdName)
	for _, a := range conn.lastCmdArgs {
		assert.NotEqual(t, "size", a,
			"Size: false must not send the `size` argument on the wire")
	}
}

// fakeCommandConn is a `transport.Conn` that records the last
// `Command` invocation and returns a pre-canned response. Used by
// Session unit tests that need to pin both sides of the wire exchange
// without standing up the in-process server.
type fakeCommandConn struct {
	cmdResp     []byte
	lastCmdName string
	lastCmdArgs []string
	lastCmdCaps []string
}

func (f *fakeCommandConn) Advertisement() *pktline.Reader {
	return pktline.NewReader(bytes.NewReader(nil))
}

func (f *fakeCommandConn) Command(_ context.Context, name string, args, caps []string) (*pktline.Reader, error) {
	f.lastCmdName = name
	f.lastCmdArgs = append([]string(nil), args...)
	f.lastCmdCaps = append([]string(nil), caps...)
	return pktline.NewReader(bytes.NewReader(f.cmdResp)), nil
}

func (f *fakeCommandConn) Close() error { return nil }

// TestSession_Close_idempotent pins the idempotent-Close contract: two
// successive calls must both return nil.
func TestSession_Close_idempotent(t *testing.T) {
	store := openFixtureStore(t, "loose-only")
	srv := httptest.NewServer(serveHandlerV2(t, store, "/repo.git"))
	defer srv.Close()

	s, err := Dial(context.Background(), srv.URL+"/repo.git")
	require.NoError(t, err)

	assert.NoError(t, s.Close(), "first Close must succeed")
	assert.NoError(t, s.Close(), "second Close must also return nil")
}

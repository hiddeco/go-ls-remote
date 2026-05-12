package lsremote

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
	"github.com/hiddeco/go-ls-remote/internal/objstore"
	"github.com/hiddeco/go-ls-remote/internal/wire"
	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/transport"
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

	seq, err := s.Refs(context.Background(), RefsRequest{})
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

// TestSession_Refs_v2_prefixesAndSymrefs pins that `RefsRequest.Prefixes`
// flows to the server as one `ref-prefix` arg per element and
// `RefsRequest.Symrefs` populates `Ref.Symref` on HEAD. The fixture
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
	seq, err := s.Refs(context.Background(), RefsRequest{
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
	seq, err = s.Refs(context.Background(), RefsRequest{Symrefs: true})
	require.NoError(t, err)
	var sawHEADSymref bool
	for ref, err := range seq {
		require.NoError(t, err)
		if ref.Name == "HEAD" {
			assert.Equal(t, "refs/heads/main", ref.Symref,
				"RefsRequest.Symrefs must flow to the server and populate Ref.Symref on HEAD")
			sawHEADSymref = true
		}
	}
	assert.True(t, sawHEADSymref, "the symref-target attribute must reach HEAD on v2")
}

// TestSession_Refs_v0_clientSideFilter pins the v0/v1 path: the cached
// advertisement-time slice is filtered client-side by `RefsRequest.Prefixes`,
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
	seq, err := s.Refs(context.Background(), RefsRequest{})
	require.NoError(t, err)
	var all []Ref
	for ref, err := range seq {
		require.NoError(t, err)
		all = append(all, ref)
	}
	require.NotEmpty(t, all)

	// Filter on `refs/heads/`: HEAD is dropped, refs/heads/main survives.
	seq, err = s.Refs(context.Background(), RefsRequest{
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

// TestSession_Refs_v0_SymrefsFalseLeavesEmpty pins that the default
// RefsRequest{} on a v0 session yields every ref with an empty
// Ref.Symref — callers that do not opt in to symref resolution must not
// observe the capability-level mapping.
func TestSession_Refs_v0_SymrefsFalseLeavesEmpty(t *testing.T) {
	store, _ := openObjectInfoFixture(t)
	srv := httptest.NewServer(serveHandlerV0(t, store, "/repo.git"))
	defer srv.Close()

	s, err := Dial(context.Background(), srv.URL+"/repo.git")
	require.NoError(t, err)
	defer func() { _ = s.Close() }()
	require.Equal(t, ProtocolV0, s.Capabilities().Version)

	// Default request does not set Symrefs; every yielded ref must have
	// an empty Symref, even HEAD whose capability-level mapping is
	// `symref=HEAD:refs/heads/main`.
	seq, err := s.Refs(context.Background(), RefsRequest{})
	require.NoError(t, err)
	var got []Ref
	for ref, err := range seq {
		require.NoError(t, err)
		got = append(got, ref)
	}
	require.NotEmpty(t, got)
	for _, ref := range got {
		assert.Empty(t, ref.Symref,
			"Symrefs: false must yield empty Ref.Symref; got %q on %q",
			ref.Symref, ref.Name)
	}
}

// TestSession_Refs_v0_SymrefsFlagFillsRefSymref pins that
// RefsRequest{Symrefs: true} on a v0 session post-fills Ref.Symref
// from Capabilities.Symrefs, unifying the call-site experience with
// v2. The fixture advertises `symref=HEAD:refs/heads/main`, so HEAD
// must carry Symref == "refs/heads/main" when the flag is set.
func TestSession_Refs_v0_SymrefsFlagFillsRefSymref(t *testing.T) {
	store, _ := openObjectInfoFixture(t)
	srv := httptest.NewServer(serveHandlerV0(t, store, "/repo.git"))
	defer srv.Close()

	s, err := Dial(context.Background(), srv.URL+"/repo.git")
	require.NoError(t, err)
	defer func() { _ = s.Close() }()
	require.Equal(t, ProtocolV0, s.Capabilities().Version)

	seq, err := s.Refs(context.Background(), RefsRequest{Symrefs: true})
	require.NoError(t, err)
	var sawHEAD bool
	for ref, err := range seq {
		require.NoError(t, err)
		if ref.Name == "HEAD" {
			assert.Equal(t, "refs/heads/main", ref.Symref,
				"Symrefs: true on v0 must post-fill Ref.Symref from "+
					"Capabilities.Symrefs; HEAD must carry refs/heads/main")
			sawHEAD = true
		}
	}
	assert.True(t, sawHEAD,
		"v0 advertisement must include HEAD so the symref assertion fires")
}

// TestSession_Refs_v0_NoSymrefsCapability pins the no-capability edge
// case: when the v0 advertisement carries no `symref=` capability,
// RefsRequest{Symrefs: true} must yield refs with an empty Ref.Symref
// rather than synthesising data. The stub uses
// buildV0NoSymrefAdvertisement, which emits HEAD with no symref cap.
func TestSession_Refs_v0_NoSymrefsCapability(t *testing.T) {
	advBytes := buildV0NoSymrefAdvertisement(t)
	conn := &stubConn{
		adv: pktline.NewReader(bytes.NewReader(advBytes)),
	}
	cap := &captureTransport{schemes: []string{"file"}, conn: conn}
	reg := transport.NewRegistry(cap)
	s, err := Dial(context.Background(), "file:///stub",
		WithTransports(reg))
	require.NoError(t, err)
	defer func() { _ = s.Close() }()
	require.Equal(t, ProtocolV0, s.Capabilities().Version)

	// No symref capability in the advertisement; Symrefs: true must
	// still yield refs with empty Symref rather than injecting data.
	seq, err := s.Refs(context.Background(), RefsRequest{Symrefs: true})
	require.NoError(t, err)
	for ref, err := range seq {
		require.NoError(t, err)
		assert.Empty(t, ref.Symref,
			"no symref= capability means Ref.Symref must be empty even "+
				"with Symrefs: true; got %q on %q", ref.Symref, ref.Name)
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

	refs, err := s.ListRefs(context.Background(), RefsRequest{})
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
		[]string{commitOID}, ObjectInfoRequest{Size: true})
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
		[]string{commitOID}, ObjectInfoRequest{Size: true})
	assert.Nil(t, got)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrUnsupportedProtocol),
		"object-info on v0 must match ErrUnsupportedProtocol; got %v", err)

	var pe *ProtocolError
	require.True(t, errors.As(err, &pe))
	assert.Equal(t, "object-info", pe.Op)
}

// TestSession_ObjectInfo_unsupportedWhenCapabilityAbsent pins the v2
// capability-set gate: even when the negotiated version is v2, a server
// that did not advertise `object-info` in its capability set must
// short-circuit on the client side with a `*ProtocolError` whose chain
// matches `ErrUnsupportedProtocol`. Mainstream Git hosts (GitHub,
// Codeberg, Bitbucket, Gitea, most of `gitlab.com`) advertise v2 with
// `commands=[ls-refs fetch]` only, so issuing `object-info` would
// otherwise elicit a raw transport-level failure rather than a
// public-typed error.
func TestSession_ObjectInfo_unsupportedWhenCapabilityAbsent(t *testing.T) {
	const commitOID = "26dae744f51e61913f50bd402cbe63953c7d637b"

	s := &Session{
		caps: Capabilities{
			Version:      ProtocolV2,
			ObjectFormat: ObjectFormatSHA1,
			Commands:     []string{"ls-refs", "fetch"},
		},
	}

	got, err := s.ObjectInfo(context.Background(),
		[]string{commitOID}, ObjectInfoRequest{Size: true})
	assert.Nil(t, got)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrUnsupportedProtocol),
		"object-info on a v2 session lacking the capability must match "+
			"ErrUnsupportedProtocol; got %v", err)

	var pe *ProtocolError
	require.True(t, errors.As(err, &pe))
	assert.Equal(t, "object-info", pe.Op)
}

// TestSession_ObjectInfo_sizeFalseSeam pins the wire-server seam on a
// no-`size` `object-info` request. Canonical Git's
// [protocol-caps.c::send_info lines 47-48] skip the attrs PKT-LINE
// entirely when the client did not request `size`, so the response is
// per-OID `<oid>\n` rows followed by a flush — no attrs. The wire
// decoder must recognise that shape and surface the OIDs, and the
// Session must translate each row's wire `Size=0` into the public
// `Size=-1` sentinel.
//
// This test exercises the full seam through the in-process server so a
// regression in either layer (server elides attrs, decoder consumes the
// first row as a degenerate attrs line, Session forgets to translate
// the sentinel) is caught here rather than at the next phase boundary.
//
// [protocol-caps.c::send_info lines 47-48]: https://github.com/git/git/blob/v2.54.0/protocol-caps.c#L47-L48
func TestSession_ObjectInfo_sizeFalseSeam(t *testing.T) {
	store, commitOID := openObjectInfoFixture(t)
	srv := httptest.NewServer(serveHandlerV2(t, store, "/repo.git"))
	defer srv.Close()

	s, err := Dial(context.Background(), srv.URL+"/repo.git")
	require.NoError(t, err)
	defer func() { _ = s.Close() }()

	got, err := s.ObjectInfo(context.Background(),
		[]string{commitOID}, ObjectInfoRequest{})
	require.NoError(t, err)
	require.Len(t, got, 1,
		"no-size response must surface one row per requested OID; "+
			"decoder previously consumed the first row as a degenerate attrs line")
	assert.Equal(t, commitOID, got[0].Hash)
	assert.Equal(t, int64(-1), got[0].Size,
		"Size: false must translate to ObjectInfo.Size == -1")
}

// TestSession_ObjectInfo_sizeFalseNoSizeArg confirms the request side
// of [TestSession_ObjectInfo_sizeFalseSeam]: when `ObjectInfoRequest.Size`
// is false the wire request must not carry the `size` argument. The
// public contract leaks no Size-related capability the caller did not
// opt into. A stub `transport.Conn` pins the bytes the Session writes
// without rebuilding the in-process server here.
func TestSession_ObjectInfo_sizeFalseNoSizeArg(t *testing.T) {
	commitOID := "26dae744f51e61913f50bd402cbe63953c7d637b"

	// Synthesise a canonical no-attrs response so the Session call
	// returns; this test cares only about the request bytes.
	var resp bytes.Buffer
	pw := pktline.NewWriter(&resp)
	require.NoError(t, pw.WritePacket([]byte(commitOID+"\n")))
	require.NoError(t, pw.WriteFlush())

	conn := &fakeCommandConn{cmdResp: resp.Bytes()}
	s := &Session{
		conn: conn,
		caps: Capabilities{
			Version:      ProtocolV2,
			ObjectFormat: ObjectFormatSHA1,
			Commands:     []string{"ls-refs", "fetch", "object-info"},
			Raw:          map[string][]string{"object-info": {""}},
		},
	}

	_, err := s.ObjectInfo(context.Background(),
		[]string{commitOID}, ObjectInfoRequest{})
	require.NoError(t, err)

	assert.Equal(t, "object-info", conn.lastCmdName)

	// Parse the captured request body as pkt-lines and assert no `size`
	// line appears in the argument section. The wire shape pinned by
	// [gitprotocol-v2.adoc §"Command Request"] places arguments after
	// the delim packet; pre-delim data lines carry the command name and
	// capabilities, neither of which can equal `size` in this fixture.
	//
	// [gitprotocol-v2.adoc §"Command Request"]: https://github.com/git/git/blob/v2.54.0/Documentation/gitprotocol-v2.adoc#command-request
	pr := pktline.NewReader(bytes.NewReader(conn.lastCmdBody.Bytes()))
	for {
		pkt, err := pr.ReadPacket()
		require.NoError(t, err)
		if pkt.Kind == pktline.Flush {
			break
		}
		if pkt.Kind != pktline.Data {
			continue
		}
		assert.NotEqual(t, "size\n", string(pkt.Data),
			"Size: false must not send the `size` argument on the wire")
	}
}

// fakeCommandConn is a `transport.Conn` that records the last
// `Command` invocation and returns a pre-canned response. Used by
// Session unit tests that need to pin both sides of the wire exchange
// without standing up the in-process server. `lastCmdBody` captures
// the bytes the callback wrote so a test can parse the on-wire request
// shape and assert against it.
type fakeCommandConn struct {
	cmdResp     []byte
	lastCmdName string
	lastCmdBody bytes.Buffer
}

func (f *fakeCommandConn) Advertisement() *pktline.Reader {
	return pktline.NewReader(bytes.NewReader(nil))
}

func (f *fakeCommandConn) Command(_ context.Context, name string,
	body transport.CommandBody) (*pktline.Reader, error) {
	f.lastCmdName = name
	f.lastCmdBody.Reset()
	if err := body(pktline.NewWriter(&f.lastCmdBody)); err != nil {
		return nil, err
	}
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

// TestSession_protocolError_bridgesServerRefused pins the public-bridge
// contract: when the wire layer surfaces an error whose chain reaches
// `wire.ErrServerRefused`, `protocolError` must join the public
// `ErrServerRefused` sentinel onto the stored chain so a caller's
// `errors.Is(err, ErrServerRefused)` matches without reaching into the
// wire package. The verbatim wire-level identity must remain reachable
// via `errors.Is` on the same chain — joining never severs it.
func TestSession_protocolError_bridgesServerRefused(t *testing.T) {
	s := &Session{
		url:  "http://example.test/repo.git",
		caps: Capabilities{Version: ProtocolV2},
	}

	t.Run("direct wire.ErrServerRefused is bridged", func(t *testing.T) {
		err := s.protocolError("ls-refs", wire.ErrServerRefused)
		require.Error(t, err)

		assert.True(t, errors.Is(err, ErrServerRefused),
			"public ErrServerRefused must match the joined chain")
		assert.True(t, errors.Is(err, wire.ErrServerRefused),
			"wire.ErrServerRefused must remain reachable on the joined chain")

		var pe *ProtocolError
		require.True(t, errors.As(err, &pe))
		assert.Equal(t, "ls-refs", pe.Op)
	})

	t.Run("wrapped wire.ErrServerRefused is bridged", func(t *testing.T) {
		wrapped := fmt.Errorf("ls-refs: %w", wire.ErrServerRefused)
		err := s.protocolError("ls-refs", wrapped)
		require.Error(t, err)

		assert.True(t, errors.Is(err, ErrServerRefused),
			"public ErrServerRefused must match even through a wrapping layer")
		assert.True(t, errors.Is(err, wire.ErrServerRefused),
			"wire.ErrServerRefused must remain reachable through the wrap")
	})

	t.Run("unrelated wire error is not bridged", func(t *testing.T) {
		other := errors.New("decode: short read")
		err := s.protocolError("object-info", other)
		require.Error(t, err)

		assert.False(t, errors.Is(err, ErrServerRefused),
			"ErrServerRefused must not match an unrelated wire error")
		assert.True(t, errors.Is(err, other),
			"the original wire error must remain reachable")
	})
}

package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
	"github.com/hiddeco/go-ls-remote/internal/objstore"
	"github.com/hiddeco/go-ls-remote/internal/testfixture"
	"github.com/hiddeco/go-ls-remote/internal/wire"
	"github.com/hiddeco/go-ls-remote/transport"
)

// openStoreFromFixture materializes the named fixture, creates any
// missing `objects/` and `objects/pack/` directories so the gitdir
// satisfies canonical Git's `is_git_directory` check (see
// [setup.c::is_git_directory]), and returns an opened [objstore.Store[objfmt.SHA1Hash]].
// A handful of ref-only fixtures (`unborn-head`, `detached-head`,
// `mixed`) ship without an `objects/` directory because they were
// authored for the ref-backend tests; the server tests need the full
// gitdir shape so the empty objects directory is conjured here.
//
// [setup.c::is_git_directory]: https://github.com/git/git/blob/v2.54.0/setup.c#L416
func openStoreFromFixture(t *testing.T, name string) *objstore.Store[objfmt.SHA1Hash] {
	t.Helper()
	gitdir := testfixture.MaterializeRepo(t, name)
	require.NoError(t, os.MkdirAll(filepath.Join(gitdir, "objects", "pack"), 0o755))
	store, err := objstore.Open[objfmt.SHA1Hash](gitdir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// openStoreFromFixture256 is the SHA-256 sibling of
// [openStoreFromFixture]. The two are split rather than parameterised
// so callsites that already hard-code the SHA-1 type stay readable;
// the SHA-256 path is exercised by exactly two tests.
func openStoreFromFixture256(t *testing.T, name string) *objstore.Store[objfmt.SHA256Hash] {
	t.Helper()
	gitdir := testfixture.MaterializeRepo(t, name)
	require.NoError(t, os.MkdirAll(filepath.Join(gitdir, "objects", "pack"), 0o755))
	store, err := objstore.Open[objfmt.SHA256Hash](gitdir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// TestServe_V0EmptyRepoPlaceholder pins the empty-repo branch of the
// canonical advertise loop ([upload-pack.c:1416-1438]): when no ref
// callback fires, `data.sent_capabilities` stays zero and
// `write_v0_ref` is invoked once with a synthetic
// `capabilities^{}` ref carrying the null oid. The pkt-line payload
// is `<zero-oid> capabilities^{}\0<caps>\n`. The `empty` fixture's
// HEAD is `ref: refs/heads/main`, so `format_symref_info`
// ([upload-pack.c:1216-1224]) yields `symref=HEAD:refs/heads/main`
// even though HEAD itself is not advertised.
//
// [upload-pack.c:1416-1438]: https://github.com/git/git/blob/v2.54.0/upload-pack.c#L1416-L1438
// [upload-pack.c:1216-1224]: https://github.com/git/git/blob/v2.54.0/upload-pack.c#L1216-L1224
func TestServe_V0EmptyRepoPlaceholder(t *testing.T) {
	t.Parallel()
	store := openEmptyStore(t)

	got := runAdvertise(t, store, Options{
		Agent:             "test-agent/0.0",
		PreferredProtocol: transport.ProtocolV0,
	})

	caps := "symref=HEAD:refs/heads/main object-format=sha1 agent=test-agent/0.0"
	want := pktLine("0000000000000000000000000000000000000000 capabilities^{}\x00"+caps+"\n") +
		"0000"
	assert.Equal(t, want, string(got))
}

// TestServe_V0UnbornHeadPlaceholder mirrors
// [TestServe_V0EmptyRepoPlaceholder] against the dedicated
// `unborn-head` fixture: the HEAD file resolves to a symbolic ref
// pointing at `refs/heads/main` with no underlying ref entry, so the
// advertise loop emits the `capabilities^{}` placeholder. The
// `symref=HEAD:refs/heads/main` cap is still emitted because
// `format_symref_info` formats whatever `data.symref` carries
// regardless of the placeholder ref's name.
func TestServe_V0UnbornHeadPlaceholder(t *testing.T) {
	t.Parallel()
	store := openStoreFromFixture(t, "unborn-head")

	got := runAdvertise(t, store, Options{
		Agent:             "test-agent/0.0",
		PreferredProtocol: transport.ProtocolV0,
	})

	caps := "symref=HEAD:refs/heads/main object-format=sha1 agent=test-agent/0.0"
	want := pktLine("0000000000000000000000000000000000000000 capabilities^{}\x00"+caps+"\n") +
		"0000"
	assert.Equal(t, want, string(got))
}

// TestServe_V0DefaultsAgent pins the agent fallback for the v0 path:
// when [Options.Agent] is empty, the cap list carries
// `agent=<wire.DefaultUserAgent>` (matching [upload-pack.c:1262]'s
// `git_user_agent_sanitized()`).
//
// [upload-pack.c:1262]: https://github.com/git/git/blob/v2.54.0/upload-pack.c#L1262
func TestServe_V0DefaultsAgent(t *testing.T) {
	t.Parallel()
	store := openEmptyStore(t)

	got := runAdvertise(t, store, Options{
		PreferredProtocol: transport.ProtocolV0,
	})

	wantAgent := "agent=" + wire.DefaultUserAgent
	assert.Contains(t, string(got), wantAgent,
		"want agent token %q in advertisement", wantAgent)
}

// TestServe_V0DetachedHead pins the detached-HEAD shape: HEAD has no
// symref target, so no `symref=HEAD:...` cap is emitted. Canonical
// reference: `format_symref_info` at [upload-pack.c:1216-1224] early-
// returns when the symref list is empty. The `detached-head` fixture
// has HEAD at `4444...` with no other refs, so HEAD is the only
// advertised ref.
//
// [upload-pack.c:1216-1224]: https://github.com/git/git/blob/v2.54.0/upload-pack.c#L1216-L1224
func TestServe_V0DetachedHead(t *testing.T) {
	t.Parallel()
	store := openStoreFromFixture(t, "detached-head")

	got := runAdvertise(t, store, Options{
		Agent:             "test-agent/0.0",
		PreferredProtocol: transport.ProtocolV0,
	})

	caps := "object-format=sha1 agent=test-agent/0.0"
	want := pktLine("4444444444444444444444444444444444444444 HEAD\x00"+caps+"\n") +
		"0000"
	assert.Equal(t, want, string(got))

	// Defence in depth: no symref token must appear anywhere in the
	// emitted bytes — neither in the cap list of HEAD nor as a stray
	// trailer.
	assert.NotContains(t, string(got), "symref=",
		"detached HEAD must not advertise a symref capability")
}

// TestServe_V0PackedRefsWithPeeledTag pins the full advertise shape
// for a non-empty repo whose `packed-refs` carries a branch and an
// annotated tag with a peel line. The fixture HEAD is symbolic
// (`refs/heads/main`), so the first emitted ref is HEAD itself with
// the cap list ([upload-pack.c:1416-1422] invokes
// `head_ref_namespaced(send_ref)` before
// `for_each_namespaced_ref_1(send_ref, &data)`). Subsequent refs are
// emitted in C-locale byte order; the annotated tag's peel line is
// written immediately after the tag ([upload-pack.c:1268-1270]).
//
// Layout of `packed-refs-fully-peeled/dotgit/packed-refs`:
//
//	# pack-refs with: peeled fully-peeled
//	aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa refs/heads/main
//	cccccccccccccccccccccccccccccccccccccccc refs/tags/v1
//	^dddddddddddddddddddddddddddddddddddddddd
//
// HEAD points at `refs/heads/main`, so HEAD's resolved oid is `aaa...`.
//
// [upload-pack.c:1416-1422]: https://github.com/git/git/blob/v2.54.0/upload-pack.c#L1416-L1422
// [upload-pack.c:1268-1270]: https://github.com/git/git/blob/v2.54.0/upload-pack.c#L1268-L1270
func TestServe_V0PackedRefsWithPeeledTag(t *testing.T) {
	t.Parallel()
	store := openStoreFromFixture(t, "packed-refs-fully-peeled")

	got := runAdvertise(t, store, Options{
		Agent:             "test-agent/0.0",
		PreferredProtocol: transport.ProtocolV0,
	})

	mainOID := strings.Repeat("a", 40)
	tagOID := strings.Repeat("c", 40)
	peeledOID := strings.Repeat("d", 40)

	caps := "symref=HEAD:refs/heads/main object-format=sha1 agent=test-agent/0.0"
	want := pktLine(mainOID+" HEAD\x00"+caps+"\n") +
		pktLine(mainOID+" refs/heads/main\n") +
		pktLine(tagOID+" refs/tags/v1\n") +
		pktLine(peeledOID+" refs/tags/v1^{}\n") +
		"0000"
	assert.Equal(t, want, string(got))
}

// TestServe_V0SortedRefs pins the C-locale byte ordering of the
// non-HEAD ref list against the `mixed` fixture, which combines a
// loose ref and a packed ref so the underlying `IterRefs` iterator
// has to merge two backends. Canonical reference:
// [gitprotocol-pack.adoc:201-203] ("MUST be sorted by name according
// to the C locale ordering"); the canonical advertise loop achieves
// this via `for_each_namespaced_ref_1` which iterates the merged
// ref view in sorted order.
//
// Layout of the `mixed` fixture:
//
//	dotgit/refs/heads/main         -> 3333...  (loose, overrides packed)
//	dotgit/packed-refs:
//	  1111... refs/heads/main             (shadowed by loose)
//	  2222... refs/heads/old
//	dotgit/HEAD                    -> ref: refs/heads/main
//
// [gitprotocol-pack.adoc:201-203]: https://github.com/git/git/blob/v2.54.0/Documentation/gitprotocol-pack.adoc?plain=1#L201-L203
func TestServe_V0SortedRefs(t *testing.T) {
	t.Parallel()
	store := openStoreFromFixture(t, "mixed")

	got := runAdvertise(t, store, Options{
		Agent:             "test-agent/0.0",
		PreferredProtocol: transport.ProtocolV0,
	})

	mainOID := strings.Repeat("3", 40)
	oldOID := strings.Repeat("2", 40)

	caps := "symref=HEAD:refs/heads/main object-format=sha1 agent=test-agent/0.0"
	want := pktLine(mainOID+" HEAD\x00"+caps+"\n") +
		pktLine(mainOID+" refs/heads/main\n") +
		pktLine(oldOID+" refs/heads/old\n") +
		"0000"
	assert.Equal(t, want, string(got))
}

// buildUnbornHeadStrayRefsFixture materializes a gitdir whose HEAD
// points symbolically at `refs/heads/main` while the underlying ref
// does not exist (so [objstore.Store[objfmt.SHA1Hash].Head] reports unborn) AND a
// stray packed ref does exist. No committed fixture under
// `testdata/repos/` carries this combination, so the directory is
// built inline in `t.TempDir()`.
func buildUnbornHeadStrayRefsFixture(t *testing.T, strayOID string) *objstore.Store[objfmt.SHA1Hash] {
	t.Helper()

	root := t.TempDir()
	gitDir := filepath.Join(root, ".git")
	require.NoError(t, os.MkdirAll(filepath.Join(gitDir, "objects", "pack"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(gitDir, "refs"), 0o755))

	require.NoError(t, os.WriteFile(filepath.Join(gitDir, "HEAD"),
		[]byte("ref: refs/heads/main\n"), 0o644))

	// `packed-refs` carries the stray ref but no entry for
	// `refs/heads/main`, so HEAD's symref target is unresolved and
	// `Head` returns the unborn shape.
	packedRefs := "" +
		"# pack-refs with: peeled fully-peeled sorted\n" +
		strayOID + " refs/tags/v1\n"
	require.NoError(t, os.WriteFile(filepath.Join(gitDir, "packed-refs"),
		[]byte(packedRefs), 0o644))

	s, err := objstore.Open[objfmt.SHA1Hash](gitDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestServe_V0UnbornHeadWithStrayRefs covers the v0 advertise path
// where `head.Unborn` is true but the ref list is non-empty: HEAD is
// not advertised (canonical Git's `mark_our_ref` filters the all-zero
// oid out), the canonical empty-repo placeholder also does not fire
// (the ref walk produces refs), and the cap list lands on the first
// sorted non-HEAD ref via the `capsEmitted=false` fall-through in
// `writeV0Advertisement`. The `symref=HEAD:<target>` cap is still
// emitted because `format_symref_info` ([upload-pack.c:1216-1224])
// formats whatever `data.symref` carries regardless of whether HEAD
// itself is advertised.
//
// [upload-pack.c:1216-1224]: https://github.com/git/git/blob/v2.54.0/upload-pack.c#L1216-L1224
func TestServe_V0UnbornHeadWithStrayRefs(t *testing.T) {
	t.Parallel()
	strayOID := strings.Repeat("e", 40)
	store := buildUnbornHeadStrayRefsFixture(t, strayOID)

	got := runAdvertise(t, store, Options{
		Agent:             "test-agent/0.0",
		PreferredProtocol: transport.ProtocolV0,
	})

	caps := "symref=HEAD:refs/heads/main object-format=sha1 agent=test-agent/0.0"
	want := pktLine(strayOID+" refs/tags/v1\x00"+caps+"\n") +
		"0000"
	assert.Equal(t, want, string(got))
}

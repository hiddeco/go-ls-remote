package objstore

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
)

// OIDs and offsets carried by the `midx-*` fixtures. Mirror the
// canonical midx + pack pairs under `testdata/objfmt/`; regenerate
// from the matching `*.offsets.txt` sidecars if the bytes ever change.
// Hard-coded here so the assertions stay obvious at the call site and
// the sidecar format is not promoted to a public contract.
const (
	// `testdata/objfmt/midx-pack-1.offsets.txt` — first commit, first byte
	// 0x42, lives in `midx-pack-1.idx`.
	midxPack1CommitOID    = "425e8a5fa5764bf56047743e3ef6d918129c3493"
	midxPack1CommitOffset = int64(12)

	// `testdata/objfmt/midx-pack-2.offsets.txt` — second commit, first byte
	// 0x07, lives in `midx-pack-2.idx`.
	midxPack2CommitOID    = "07bd9c9f7a2db2c0c3b6e100ab47e261b11549ec"
	midxPack2CommitOffset = int64(12)

	// `testdata/objfmt/three-objects.offsets.txt` — sibling-only OID.
	// Reuses [threeCommitOID] / [threeCommitOffset] from the idx-catalog
	// test set so the same bytes test both backends end to end.
)

// openMidxBackendFromFixture materializes the named fixture and opens
// the midx backend rooted at its `.git/`. Mirrors the helper the
// idx-catalog tests use so each test stays focused on the assertion it
// is making.
func openMidxBackendFromFixture(t *testing.T, name string) *midxBackend[objfmt.SHA1Hash] {
	t.Helper()
	root := materializeFixture(t, name)
	gitDir := filepath.Join(root, ".git")
	b, err := openMidxBackend[objfmt.SHA1Hash](gitDir, objfmt.SHA1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })
	return b
}

func TestMidxBackend_OpensWithSiblingPacks(t *testing.T) {
	t.Parallel()
	// `midx-with-siblings/` carries the midx + the two midx-covered
	// packs + one sibling pack added after midx generation. The
	// constructor must wire all of them up: the midx-listed packs into
	// `coveredByMidxIndex`, the sibling into `siblings`.
	b := openMidxBackendFromFixture(t, "midx-with-siblings")

	require.NotNil(t, b.midx)
	assert.Equal(t, []string{"midx-pack-1.idx", "midx-pack-2.idx"},
		b.packNames, "cached PackNames must list both covered idxs")
	assert.Len(t, b.coveredByMidxIndex, 2,
		"both midx-covered packs must be slotted by PackNames index")
	for i, p := range b.coveredByMidxIndex {
		assert.NotNilf(t, p, "covered slot %d must hold a *Pack", i)
	}
	assert.Len(t, b.siblings, 1,
		"the one not-covered pack must surface as a sibling")
	assert.Len(t, b.ordered, 3,
		"`ordered` must enumerate every opened pack exactly once")
}

func TestMidxBackend_LookupHitViaMidx(t *testing.T) {
	t.Parallel()
	// An OID present in a midx-covered pack must be answered through
	// `Midx.Find`, and the returned `*Pack` must be the one keyed by
	// the corresponding `PackNames` entry — not the sibling and not a
	// freshly-opened handle.
	b := openMidxBackendFromFixture(t, "midx-with-siblings")

	pack, off, ok, err := b.Lookup(hashFromHex(t, midxPack1CommitOID, objfmt.SHA1))
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, midxPack1CommitOffset, off)
	assert.Same(t, b.coveredByMidxIndex[0], pack,
		"midx hit must yield the pack at the matching PackNames slot")

	pack, off, ok, err = b.Lookup(hashFromHex(t, midxPack2CommitOID, objfmt.SHA1))
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, midxPack2CommitOffset, off)
	assert.Same(t, b.coveredByMidxIndex[1], pack)
}

func TestMidxBackend_LookupHitViaSiblingFallback(t *testing.T) {
	t.Parallel()
	// `three-objects` was added after midx generation, so its OIDs are
	// absent from the midx and must be picked up by the sibling
	// fallback scan. The returned pack is the sibling, not a midx
	// member.
	b := openMidxBackendFromFixture(t, "midx-with-siblings")

	pack, off, ok, err := b.Lookup(hashFromHex(t, threeCommitOID, objfmt.SHA1))
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, threeCommitOffset, off)
	require.Len(t, b.siblings, 1)
	assert.Same(t, b.siblings[0].pack, pack,
		"sibling fallback must yield the sibling-only `*Pack`")
}

func TestMidxBackend_LookupMissReturnsNilError(t *testing.T) {
	t.Parallel()
	// A clean miss surfaces as (nil, 0, false, nil) so the upper layer
	// can fall through to loose objects, alternates, or
	// [ErrCorruptObject] without reinterpreting the error slot.
	b := openMidxBackendFromFixture(t, "midx-with-siblings")

	pack, off, ok, err := b.Lookup(hashFromHex(t,
		"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", objfmt.SHA1))
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Nil(t, pack)
	assert.Zero(t, off)
}

func TestMidxBackend_AllPacksDeterministicOrder(t *testing.T) {
	t.Parallel()
	// `AllPacks` must yield every pack — midx-covered AND sibling — in
	// a single mtime-desc order with basename as the stable
	// tiebreaker, matching canonical Git's [packfile.c::sort_pack].
	// Stamping every pack to the same mtime exercises the tiebreaker:
	// the order collapses to lexical basename order regardless of
	// whether a pack is midx-covered.
	//
	// [packfile.c::sort_pack]: https://github.com/git/git/blob/v2.54.0/packfile.c#L1042
	wantNames := []string{"midx-pack-1.idx", "midx-pack-2.idx", "three-objects.idx"}
	for i := range 3 {
		root := materializeFixture(t, "midx-with-siblings")
		gitDir := filepath.Join(root, ".git")
		stampPackMtimes(t, gitDir, map[string]time.Time{
			"midx-pack-1.pack":   packMtimeAnchor,
			"midx-pack-2.pack":   packMtimeAnchor,
			"three-objects.pack": packMtimeAnchor,
		})

		b, err := openMidxBackend[objfmt.SHA1Hash](gitDir, objfmt.SHA1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = b.Close() })

		var names []string
		for p := range b.AllPacks() {
			require.NotNil(t, p)
			idx := idxForPackInMidx(t, b, p)
			names = append(names, filepath.Base(idx.Path()))
		}
		assert.Equal(t, wantNames, names,
			"iteration %d: equal-mtime backend must fall back to basename order", i)
	}
}

func TestMidxBackend_AllPacksOrderedByMtimeDesc(t *testing.T) {
	t.Parallel()
	// `AllPacks` is a single mtime-desc list across midx-covered and
	// sibling packs — the midx-listed-first quirk does not survive in
	// the enumeration order. With `three-objects.pack` (sibling)
	// stamped youngest, it must lead the iteration even though it is
	// not midx-covered.
	root := materializeFixture(t, "midx-with-siblings")
	gitDir := filepath.Join(root, ".git")
	stampPackMtimes(t, gitDir, map[string]time.Time{
		"midx-pack-1.pack":   packMtimeAnchor,
		"midx-pack-2.pack":   packMtimeAnchor.Add(time.Hour),
		"three-objects.pack": packMtimeAnchor.Add(2 * time.Hour),
	})

	b, err := openMidxBackend[objfmt.SHA1Hash](gitDir, objfmt.SHA1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })

	var names []string
	for p := range b.AllPacks() {
		require.NotNil(t, p)
		idx := idxForPackInMidx(t, b, p)
		names = append(names, filepath.Base(idx.Path()))
	}
	assert.Equal(t,
		[]string{"three-objects.idx", "midx-pack-2.idx", "midx-pack-1.idx"},
		names, "AllPacks must yield younger packs first across both buckets")
}

func TestMidxBackend_SiblingFallbackHitsYoungerSiblingFirst(t *testing.T) {
	t.Parallel()
	// Two sibling packs not covered by the midx, both holding the
	// lookup target, must resolve through the younger one. The
	// midx-with-siblings fixture's existing sibling is the
	// `three-objects` pack (basename `t...`); cloning a second copy
	// under a basename that sorts BEFORE it (`aaa-...`) and stamping
	// the EXISTING `three-objects` pack younger pins the divergence:
	// basename-sort would visit `aaa-...` first, mtime-sort visits
	// `three-objects` first.
	root := materializeFixture(t, "midx-with-siblings")
	gitDir := filepath.Join(root, ".git")
	packDir := filepath.Join(gitDir, "objects", "pack")
	clonePackPair(t, packDir, "three-objects", packDir, "aaa-older")
	stampPackMtimes(t, gitDir, map[string]time.Time{
		"midx-pack-1.pack":   packMtimeAnchor,
		"midx-pack-2.pack":   packMtimeAnchor,
		"aaa-older.pack":     packMtimeAnchor,
		"three-objects.pack": packMtimeAnchor.Add(time.Hour),
	})

	b, err := openMidxBackend[objfmt.SHA1Hash](gitDir, objfmt.SHA1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })
	require.Len(t, b.siblings, 2,
		"both clones must surface as siblings (neither is midx-covered)")

	pack, off, ok, err := b.Lookup(hashFromHex(t, threeCommitOID, objfmt.SHA1))
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, threeCommitOffset, off)
	idx := idxForPackInMidx(t, b, pack)
	assert.Equal(t, "three-objects.idx", filepath.Base(idx.Path()),
		"sibling fallback must hit the younger pack before the older sibling")
}

func TestMidxBackend_BasenameTiebreaker(t *testing.T) {
	t.Parallel()
	// When two packs carry identical mtimes, the basename comparator
	// breaks the tie. Stamp every pack to the same instant and the
	// sibling slot at the end of `AllPacks` must order lexically — a
	// silently-undefined order would surface as a flaky pass on this
	// test across CI hosts.
	root := materializeFixture(t, "midx-with-siblings")
	gitDir := filepath.Join(root, ".git")
	packDir := filepath.Join(gitDir, "objects", "pack")
	clonePackPair(t, packDir, "three-objects", packDir, "aaa-tied")
	stampPackMtimes(t, gitDir, map[string]time.Time{
		"midx-pack-1.pack":   packMtimeAnchor,
		"midx-pack-2.pack":   packMtimeAnchor,
		"three-objects.pack": packMtimeAnchor,
		"aaa-tied.pack":      packMtimeAnchor,
	})

	b, err := openMidxBackend[objfmt.SHA1Hash](gitDir, objfmt.SHA1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })

	var names []string
	for p := range b.AllPacks() {
		require.NotNil(t, p)
		idx := idxForPackInMidx(t, b, p)
		names = append(names, filepath.Base(idx.Path()))
	}
	assert.Equal(t,
		[]string{
			"aaa-tied.idx", "midx-pack-1.idx",
			"midx-pack-2.idx", "three-objects.idx",
		},
		names, "equal-mtime packs must order by basename")
}

func TestMidxBackend_NoSiblingPacks(t *testing.T) {
	t.Parallel()
	// `midx-no-siblings/` carries midx + the two covered packs only.
	// The fallback list is empty; every Lookup must resolve through
	// the midx itself.
	b := openMidxBackendFromFixture(t, "midx-no-siblings")

	assert.Empty(t, b.siblings, "no extra packs => empty fallback list")
	assert.Len(t, b.coveredByMidxIndex, 2)
	assert.Len(t, b.ordered, 2)

	pack, off, ok, err := b.Lookup(hashFromHex(t, midxPack1CommitOID, objfmt.SHA1))
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, midxPack1CommitOffset, off)
	assert.Same(t, b.coveredByMidxIndex[0], pack)

	// Sibling-only OID misses cleanly: there is no sibling, so the
	// fallback scan finds nothing.
	pack, _, ok, err = b.Lookup(hashFromHex(t, threeCommitOID, objfmt.SHA1))
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Nil(t, pack)
}

func TestMidxBackend_CorruptMidxReturnsErrorWithoutLeak(t *testing.T) {
	t.Parallel()
	// 16 garbage bytes are enough to fail `OpenMidx`'s magic check.
	// The constructor must surface the wrapped error referencing the
	// midx path; the no-leak claim is enforced by the constructor
	// closing every already-opened resource on failure (impossible to
	// observe without a leak counter, but covered by the failure-cleanup
	// discipline shared with `idxCatalog[objfmt.SHA1Hash]`).
	root := materializeFixture(t, "midx-corrupt")
	gitDir := filepath.Join(root, ".git")

	_, err := openMidxBackend[objfmt.SHA1Hash](gitDir, objfmt.SHA1)
	require.Error(t, err)
	midxPath := filepath.Join(gitDir, "objects", "pack", "multi-pack-index")
	assert.Contains(t, err.Error(), midxPath,
		"error must reference the offending midx path")
}

func TestMidxBackend_MissingPackReferencedByMidxIsCorrupt(t *testing.T) {
	t.Parallel()
	// `midx-missing-pack/` has a midx whose `PNAM` lists pack-1 AND
	// pack-2, but only pack-1 is on disk. The constructor must reject
	// the catalog with [ErrCorruptObject] and name the missing pack so
	// operators can find the offending file. Already-opened resources
	// are closed before the error returns.
	root := materializeFixture(t, "midx-missing-pack")
	gitDir := filepath.Join(root, ".git")

	_, err := openMidxBackend[objfmt.SHA1Hash](gitDir, objfmt.SHA1)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrCorruptObject,
		"missing-pack errors must wrap ErrCorruptObject")
	assert.Contains(t, err.Error(), "midx-pack-2.idx",
		"error must name the pack the midx references but the dir lacks")
}

func TestMidxBackend_CloseIsIdempotent(t *testing.T) {
	t.Parallel()
	// `Close` cascades to the midx + every wrapped idx + every wrapped
	// pack. Subsequent calls must return the same joined error (here,
	// nil) without re-running the cascade — the Store[objfmt.SHA1Hash]-level idempotency
	// guarantee assumes this.
	root := materializeFixture(t, "midx-with-siblings")
	gitDir := filepath.Join(root, ".git")

	b, err := openMidxBackend[objfmt.SHA1Hash](gitDir, objfmt.SHA1)
	require.NoError(t, err)

	first := b.Close()
	second := b.Close()
	require.NoError(t, first)
	assert.Equal(t, first, second,
		"second Close must return the same value the first did")
}

func TestMidxBackend_OpenStoreSelectsMidxWithExpectedPacks(t *testing.T) {
	t.Parallel()
	// End-to-end: `Open` on a midx-bearing fixture must select the
	// midx backend and surface the expected pack inventory through
	// `AllPacks`.
	root := materializeFixture(t, "midx-with-siblings")

	s, err := Open[objfmt.SHA1Hash](root)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	mb, ok := s.packs.(*midxBackend[objfmt.SHA1Hash])
	require.True(t, ok, "want *midxBackend[objfmt.SHA1Hash], got %T", s.packs)
	assert.Len(t, mb.ordered, 3)

	// Spot-check both lookup paths through the Store[objfmt.SHA1Hash]-owned backend.
	pack, off, hit, err := mb.Lookup(hashFromHex(t, midxPack1CommitOID, objfmt.SHA1))
	require.NoError(t, err)
	require.True(t, hit)
	assert.NotNil(t, pack)
	assert.Equal(t, midxPack1CommitOffset, off)

	pack, off, hit, err = mb.Lookup(hashFromHex(t, threeCommitOID, objfmt.SHA1))
	require.NoError(t, err)
	require.True(t, hit, "sibling fallback must work end to end")
	assert.NotNil(t, pack)
	assert.Equal(t, threeCommitOffset, off)
}

func TestMidxBackend_IgnoresNonIdxEntries(t *testing.T) {
	t.Parallel()
	// Defence in depth: stray files alongside the midx + `.idx` files
	// must not derail the constructor. `objects/pack/` legitimately
	// holds `.keep`, `.bitmap`, and `.rev` siblings on real-world
	// repos; the backend only consumes `.idx` and the `multi-pack-index`.
	root := materializeFixture(t, "midx-with-siblings")
	gitDir := filepath.Join(root, ".git")
	require.NoError(t, os.WriteFile(filepath.Join(gitDir,
		"objects", "pack", "midx-pack-1.keep"), nil, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(gitDir,
		"objects", "pack", "noise.txt"), []byte("ignore me"), 0o644))

	b, err := openMidxBackend[objfmt.SHA1Hash](gitDir, objfmt.SHA1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })
	assert.Len(t, b.ordered, 3, "noise files must not change the inventory")
}

// Compile-time guard: the backend satisfies the [packBackend[objfmt.SHA1Hash]] contract
// the Store[objfmt.SHA1Hash] opener selects through.
var _ packBackend[objfmt.SHA1Hash] = (*midxBackend[objfmt.SHA1Hash])(nil)

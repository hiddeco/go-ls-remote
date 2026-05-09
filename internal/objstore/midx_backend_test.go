package objstore

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
func openMidxBackendFromFixture(t *testing.T, name string) *midxBackend {
	t.Helper()
	root := materializeFixture(t, name)
	gitDir := filepath.Join(root, ".git")
	b, err := openMidxBackend(gitDir, objfmt.SHA1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })
	return b
}

func TestMidxBackend_OpensWithSiblingPacks(t *testing.T) {
	// `midx-with-siblings/` carries the midx + the two midx-covered
	// packs + one sibling pack added after midx generation. The
	// constructor must wire all of them up: the midx-listed packs into
	// `coveredByMidxIndex` (and `packsByChecksum`), the sibling into
	// `siblings` (and `packsByChecksum`).
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
	assert.Len(t, b.packsByChecksum, 3,
		"every opened pack must be reachable by its trailer checksum")
	assert.Len(t, b.ordered, 3,
		"`ordered` must enumerate every opened pack exactly once")
}

func TestMidxBackend_LookupHitViaMidx(t *testing.T) {
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
	// `AllPacks` must yield midx-listed packs first in `PackNames`
	// order, then sibling packs in basename-sorted order. Stable across
	// independent opener invocations: the cross-pack REF_DELTA scan and
	// any external integrity walk depends on it.
	for i := range 3 {
		b := openMidxBackendFromFixture(t, "midx-with-siblings")

		var got []*objfmt.Pack
		for p := range b.AllPacks() {
			require.NotNil(t, p)
			got = append(got, p)
		}

		want := []*objfmt.Pack{
			b.coveredByMidxIndex[0],
			b.coveredByMidxIndex[1],
			b.siblings[0].pack,
		}
		assert.Equal(t, want, got,
			"iteration %d: AllPacks must walk midx-covered then siblings", i)
	}
}

func TestMidxBackend_PackByChecksumLookup(t *testing.T) {
	// Every pack — midx-covered AND sibling — must round-trip through
	// the checksum index. The cross-pack REF_DELTA resolver depends on
	// this for both backends through a uniform contract.
	b := openMidxBackendFromFixture(t, "midx-with-siblings")

	require.Len(t, b.packsByChecksum, 3,
		"checksum index must cover every opened pack")
	for h, pack := range b.packsByChecksum {
		got, ok := b.packByChecksum(h)
		require.True(t, ok, "checksum %x must be reachable", h)
		assert.Same(t, pack, got)
	}

	// Unknown checksum surfaces as a clean miss: the (nil, false) shape
	// is part of the accessor's contract for the future REF_DELTA
	// resolver.
	pack, ok := b.packByChecksum(objfmt.Hash{})
	assert.False(t, ok)
	assert.Nil(t, pack)
}

func TestMidxBackend_NoSiblingPacks(t *testing.T) {
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
	// 16 garbage bytes are enough to fail `OpenMidx`'s magic check.
	// The constructor must surface the wrapped error referencing the
	// midx path; the no-leak claim is enforced by the constructor
	// closing every already-opened resource on failure (impossible to
	// observe without a leak counter, but covered by the failure-cleanup
	// discipline shared with `idxCatalog`).
	root := materializeFixture(t, "midx-corrupt")
	gitDir := filepath.Join(root, ".git")

	_, err := openMidxBackend(gitDir, objfmt.SHA1)
	require.Error(t, err)
	midxPath := filepath.Join(gitDir, "objects", "pack", "multi-pack-index")
	assert.Contains(t, err.Error(), midxPath,
		"error must reference the offending midx path")
}

func TestMidxBackend_MissingPackReferencedByMidxIsCorrupt(t *testing.T) {
	// `midx-missing-pack/` has a midx whose `PNAM` lists pack-1 AND
	// pack-2, but only pack-1 is on disk. The constructor must reject
	// the catalog with [ErrCorruptObject] and name the missing pack so
	// operators can find the offending file. Already-opened resources
	// are closed before the error returns.
	root := materializeFixture(t, "midx-missing-pack")
	gitDir := filepath.Join(root, ".git")

	_, err := openMidxBackend(gitDir, objfmt.SHA1)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrCorruptObject,
		"missing-pack errors must wrap ErrCorruptObject")
	assert.Contains(t, err.Error(), "midx-pack-2.idx",
		"error must name the pack the midx references but the dir lacks")
}

func TestMidxBackend_CloseIsIdempotent(t *testing.T) {
	// `Close` cascades to the midx + every wrapped idx + every wrapped
	// pack. Subsequent calls must return the same joined error (here,
	// nil) without re-running the cascade — the Store-level idempotency
	// guarantee assumes this.
	root := materializeFixture(t, "midx-with-siblings")
	gitDir := filepath.Join(root, ".git")

	b, err := openMidxBackend(gitDir, objfmt.SHA1)
	require.NoError(t, err)

	first := b.Close()
	second := b.Close()
	assert.NoError(t, first)
	assert.Equal(t, first, second,
		"second Close must return the same value the first did")
}

func TestMidxBackend_OpenStoreSelectsMidxWithExpectedPacks(t *testing.T) {
	// End-to-end: `Open` on a midx-bearing fixture must select the
	// midx backend and surface the expected pack inventory through
	// `AllPacks`.
	root := materializeFixture(t, "midx-with-siblings")

	s, err := Open(root)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	mb, ok := s.packs.(*midxBackend)
	require.True(t, ok, "want *midxBackend, got %T", s.packs)
	assert.Len(t, mb.ordered, 3)

	// Spot-check both lookup paths through the Store-owned backend.
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

	b, err := openMidxBackend(gitDir, objfmt.SHA1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })
	assert.Len(t, b.ordered, 3, "noise files must not change the inventory")
}

// Compile-time guard: the backend satisfies the [packBackend] contract
// the Store opener selects through.
var _ packBackend = (*midxBackend)(nil)

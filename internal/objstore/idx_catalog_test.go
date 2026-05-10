package objstore

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// OIDs and offsets carried by the `idx-single` and `idx-multi` fixtures.
// These mirror the canonical pack/idx pairs under `testdata/objfmt/`;
// regenerate from the matching `*.offsets.txt` sidecar if the fixture
// bytes ever change. Hard-coded here rather than read at test time so
// the assertions stay obvious at the call site and the sidecar format
// is not promoted to a public contract.
const (
	// `testdata/objfmt/three-objects.offsets.txt`.
	threeCommitOID    = "26dae744f51e61913f50bd402cbe63953c7d637b"
	threeCommitOffset = int64(12)
	threeBlobOID      = "97d881a6f710fc8fc34524d80bfc782359137a5c"
	threeBlobOffset   = int64(179)

	// `testdata/objfmt/ofs-delta.offsets.txt`.
	ofsBlobOID    = "87bab3f4f5c79ca006911993eaec265a51c49a8b"
	ofsBlobOffset = int64(12)
)

// openIdxCatalogFromFixture materializes the named fixture and opens
// the idx-catalog backend rooted at its `.git/`. Mirrors the helpers
// the loose-objects and reftable backend tests use so each test stays
// focused on the assertion it is making.
func openIdxCatalogFromFixture(t *testing.T, name string) *idxCatalog {
	t.Helper()
	root := materializeFixture(t, name)
	gitDir := filepath.Join(root, ".git")
	c, err := openIdxCatalog(gitDir, objfmt.SHA1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestIdxCatalog_EmptyRepoOpensWithZeroPacks(t *testing.T) {
	// `empty/` carries an `objects/pack/` directory holding only a
	// `.gitkeep` placeholder. The constructor must succeed, surface no
	// packs, and report a clean miss for any hash.
	c := openIdxCatalogFromFixture(t, "empty")

	require.Empty(t, c.packs)

	pack, off, ok, err := c.Lookup(hashFromHex(t,
		"abababababababababababababababababababab", objfmt.SHA1))
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Nil(t, pack)
	assert.Zero(t, off)

	count := 0
	for range c.AllPacks() {
		count++
	}
	assert.Zero(t, count, "AllPacks must yield nothing on an empty catalog")
}

func TestIdxCatalog_MissingPackDirectoryOpensCleanly(t *testing.T) {
	// A brand-new repo may not have an `objects/pack/` directory yet.
	// `openIdxCatalog` must collapse the ENOENT into "no packs" rather
	// than refusing to construct.
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "objects"), 0o755))

	c, err := openIdxCatalog(dir, objfmt.SHA1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	assert.Empty(t, c.packs)
}

func TestIdxCatalog_LookupHitInSinglePack(t *testing.T) {
	// Single pack/idx pair: a known OID resolves to its (Pack, offset)
	// tuple. The pack returned must be the open one this catalog owns,
	// not a freshly-opened sibling.
	c := openIdxCatalogFromFixture(t, "idx-single")
	require.Len(t, c.packs, 1)

	pack, off, ok, err := c.Lookup(hashFromHex(t, threeCommitOID, objfmt.SHA1))
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, threeCommitOffset, off)
	assert.Same(t, c.packs[0].pack, pack,
		"Lookup must return the catalog's own *Pack, not a fresh open")
}

func TestIdxCatalog_LookupMissReturnsNilError(t *testing.T) {
	// Miss path on a populated catalog: clean false / nil error so the
	// upper layers can fall through to loose objects, alternates, or
	// [ErrCorruptObject] as appropriate.
	c := openIdxCatalogFromFixture(t, "idx-single")

	pack, off, ok, err := c.Lookup(hashFromHex(t,
		"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", objfmt.SHA1))
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Nil(t, pack)
	assert.Zero(t, off)
}

func TestIdxCatalog_LookupHitInSecondPack(t *testing.T) {
	// `idx-multi` carries `ofs-delta` and `three-objects`. With both
	// packs stamped to the same mtime the catalog falls back to
	// basename order, putting `ofs-delta` first and `three-objects`
	// second. An OID present only in `three-objects` must be found
	// after iterating past the `ofs-delta` miss; the returned pack is
	// the second slot.
	root := materializeFixture(t, "idx-multi")
	gitDir := filepath.Join(root, ".git")
	stampPackMtimes(t, gitDir, map[string]time.Time{
		"ofs-delta.pack":     packMtimeAnchor,
		"three-objects.pack": packMtimeAnchor,
	})
	c, err := openIdxCatalog(gitDir, objfmt.SHA1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	require.Len(t, c.packs, 2)

	pack, off, ok, err := c.Lookup(hashFromHex(t, threeBlobOID, objfmt.SHA1))
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, threeBlobOffset, off)
	assert.Same(t, c.packs[1].pack, pack,
		"second-pack hit must yield the second *Pack in deterministic order")
}

func TestIdxCatalog_AllPacksDeterministicOrder(t *testing.T) {
	// `AllPacks` must walk the catalog's internal slice verbatim and
	// must do so deterministically across independent opener
	// invocations: the cross-pack REF_DELTA scan and any external
	// integrity walk depends on it. With both packs stamped to the
	// same mtime, the basename tiebreaker pins the order to
	// `ofs-delta`, `three-objects`.
	wantNames := []string{"ofs-delta.idx", "three-objects.idx"}
	for i := range 3 {
		root := materializeFixture(t, "idx-multi")
		gitDir := filepath.Join(root, ".git")
		stampPackMtimes(t, gitDir, map[string]time.Time{
			"ofs-delta.pack":     packMtimeAnchor,
			"three-objects.pack": packMtimeAnchor,
		})

		c, err := openIdxCatalog(gitDir, objfmt.SHA1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = c.Close() })

		// AllPacks order matches the internal slice order; capture both
		// and assert they are byte-identical.
		var fromIter []*objfmt.Pack
		for p := range c.AllPacks() {
			require.NotNil(t, p)
			fromIter = append(fromIter, p)
		}
		var fromSlice []*objfmt.Pack
		var names []string
		for _, e := range c.packs {
			fromSlice = append(fromSlice, e.pack)
			names = append(names, filepath.Base(e.idx.Path()))
		}

		assert.Equal(t, fromSlice, fromIter,
			"iteration %d: AllPacks must walk the catalog slice verbatim", i)
		assert.Equal(t, wantNames, names,
			"iteration %d: equal-mtime catalog must fall back to basename order", i)
	}
}

func TestIdxCatalog_AllPacksOrderedByMtimeDesc(t *testing.T) {
	// Mirror canonical Git's `packfile.c::sort_pack`: younger packs
	// first, with basename as a stable tiebreaker. Stamping
	// `three-objects.pack` younger than `ofs-delta.pack` flips the
	// basename-sorted order, so a backend that still keyed on basename
	// would yield the packs the wrong way round.
	root := materializeFixture(t, "idx-multi")
	gitDir := filepath.Join(root, ".git")
	stampPackMtimes(t, gitDir, map[string]time.Time{
		"ofs-delta.pack":     packMtimeAnchor,
		"three-objects.pack": packMtimeAnchor.Add(time.Hour),
	})

	c, err := openIdxCatalog(gitDir, objfmt.SHA1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	var names []string
	for p := range c.AllPacks() {
		require.NotNil(t, p)
		// Idxs and packs share a basename; the idx path is the stable
		// handle the rest of the suite uses for assertions.
		idx := idxForPackInCatalog(t, c, p)
		names = append(names, filepath.Base(idx.Path()))
	}
	assert.Equal(t, []string{"three-objects.idx", "ofs-delta.idx"}, names,
		"AllPacks must yield younger packs first, basename as tiebreaker")
}

func TestIdxCatalog_LookupHitsYoungerPackFirst(t *testing.T) {
	// Two packs that hold the same OID — built by copying
	// `three-objects.{pack,idx}` to a second basename — must resolve
	// through the younger one. The linear `Lookup` scan walks the
	// catalog in mtime-desc order, so the first hit is the younger
	// pack's `*Pack`.
	root := t.TempDir()
	gitDir := filepath.Join(root, ".git")
	require.NoError(t, os.MkdirAll(filepath.Join(gitDir, "objects", "pack"), 0o755))

	srcDir := filepath.Join(packFixtureRoot(t, "idx-single"), "objects", "pack")
	clonePackPair(t, srcDir, "three-objects",
		filepath.Join(gitDir, "objects", "pack"), "older")
	clonePackPair(t, srcDir, "three-objects",
		filepath.Join(gitDir, "objects", "pack"), "younger")
	stampPackMtimes(t, gitDir, map[string]time.Time{
		"older.pack":   packMtimeAnchor,
		"younger.pack": packMtimeAnchor.Add(time.Hour),
	})

	c, err := openIdxCatalog(gitDir, objfmt.SHA1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	require.Len(t, c.packs, 2)

	pack, off, ok, err := c.Lookup(hashFromHex(t, threeCommitOID, objfmt.SHA1))
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, threeCommitOffset, off)
	assert.Same(t, c.packs[0].pack, pack,
		"Lookup must return the first (youngest) pack's *Pack")
	assert.Equal(t, "younger.idx", filepath.Base(c.packs[0].idx.Path()),
		"the catalog's first slot must be the youngest pack")
}

func TestIdxCatalog_PackByChecksumLookup(t *testing.T) {
	// The cross-pack REF_DELTA scaffolding indexes packs by their
	// pack-trailer hash (as recorded in the paired idx). Each idx's
	// recorded checksum must round-trip to its `*Pack`.
	c := openIdxCatalogFromFixture(t, "idx-multi")

	for _, e := range c.packs {
		got, ok := c.packByChecksum(e.idx.PackChecksum())
		require.True(t, ok, "pack %s must be reachable by checksum",
			filepath.Base(e.idx.Path()))
		assert.Same(t, e.pack, got)
	}
}

func TestIdxCatalog_PackByChecksumMissReturnsNilFalse(t *testing.T) {
	// An unknown checksum must surface as a clean miss; the accessor is
	// the only way the future REF_DELTA resolver can probe the index,
	// so a (nil, false) shape is part of its contract.
	c := openIdxCatalogFromFixture(t, "idx-multi")

	pack, ok := c.packByChecksum(objfmt.Hash{})
	assert.False(t, ok)
	assert.Nil(t, pack)
}

func TestIdxCatalog_CorruptIdxReturnsErrorWithoutLeak(t *testing.T) {
	// `idx-corrupt/` ships a 16-byte-zero `bogus.idx` that fails the v1
	// fan-out length check. The opener must surface the wrapped error
	// (path included) and leak no file handles. The Cleanup that the
	// successful path would install never runs, so this test verifies
	// only the error shape; the no-leak claim is enforced by the
	// constructor closing every already-opened pair on failure.
	root := materializeFixture(t, "idx-corrupt")
	gitDir := filepath.Join(root, ".git")

	_, err := openIdxCatalog(gitDir, objfmt.SHA1)
	require.Error(t, err)
	bogus := filepath.Join(gitDir, "objects", "pack", "bogus.idx")
	assert.Contains(t, err.Error(), bogus,
		"error must reference the offending idx path")
}

func TestIdxCatalog_MissingPackSiblingReturnsError(t *testing.T) {
	// `idx-missing-pack/` has `three-objects.idx` with no `.pack`
	// sibling. The opener must surface an error mentioning both paths
	// and close the already-opened idx behind the scenes (impossible to
	// directly observe without a leak counter, but covered by the same
	// failure-cleanup discipline as the corrupt-idx case).
	root := materializeFixture(t, "idx-missing-pack")
	gitDir := filepath.Join(root, ".git")

	_, err := openIdxCatalog(gitDir, objfmt.SHA1)
	require.Error(t, err)
	idxPath := filepath.Join(gitDir, "objects", "pack", "three-objects.idx")
	packPath := filepath.Join(gitDir, "objects", "pack", "three-objects.pack")
	msg := err.Error()
	assert.Contains(t, msg, idxPath, "error must reference the orphan idx")
	assert.Contains(t, msg, packPath, "error must reference the missing pack")
}

func TestIdxCatalog_CloseIsIdempotent(t *testing.T) {
	// `Close` cascades to every wrapped idx and pack. Subsequent calls
	// must return the same joined error (here, nil) without re-running
	// the cascade — the Store-level idempotency guarantee assumes this.
	root := materializeFixture(t, "idx-multi")
	gitDir := filepath.Join(root, ".git")

	c, err := openIdxCatalog(gitDir, objfmt.SHA1)
	require.NoError(t, err)

	first := c.Close()
	second := c.Close()
	assert.NoError(t, first)
	assert.Equal(t, first, second,
		"second Close must return the same value the first did")
}

func TestIdxCatalog_OpenStoreSelectsCatalogWithExpectedPacks(t *testing.T) {
	// End-to-end: `Open` on a fixture with packs but no `multi-pack-index`
	// must select the catalogue backend and surface the expected number
	// of opened packs.
	root := materializeFixture(t, "idx-multi")

	s, err := Open(root)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	cat, ok := s.packs.(*idxCatalog)
	require.True(t, ok, "want *idxCatalog, got %T", s.packs)
	assert.Len(t, cat.packs, 2)

	// Spot-check: a known OID resolves end-to-end through the Store's
	// pack backend.
	pack, off, hit, err := cat.Lookup(hashFromHex(t, ofsBlobOID, objfmt.SHA1))
	require.NoError(t, err)
	require.True(t, hit)
	assert.NotNil(t, pack)
	assert.Equal(t, ofsBlobOffset, off)
}

func TestIdxCatalog_IgnoresNonIdxEntries(t *testing.T) {
	// Defence in depth: stray files alongside `.idx` files must not
	// derail the opener. `objects/pack/` legitimately holds `.keep`,
	// `.bitmap`, and `.rev` siblings on real-world repos; the catalog
	// only consumes `.idx` and pairs each with its `.pack`.
	root := materializeFixture(t, "idx-single")
	gitDir := filepath.Join(root, ".git")
	require.NoError(t, os.WriteFile(filepath.Join(gitDir,
		"objects", "pack", "three-objects.keep"), nil, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(gitDir,
		"objects", "pack", "noise.txt"), []byte("ignore me"), 0o644))

	c, err := openIdxCatalog(gitDir, objfmt.SHA1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	assert.Len(t, c.packs, 1)
}

// Compile-time guard: the catalog satisfies the [packBackend] contract
// the Store opener selects through.
var _ packBackend = (*idxCatalog)(nil)

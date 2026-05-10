package objstore

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Stable OIDs and resolved sizes for the canonical pack/idx fixtures
// used here. The values match the `*.offsets.txt` sidecars under
// `testdata/objfmt/`; pinning them inline keeps the assertions
// readable at the call site without promoting the sidecar format
// into a public contract.
const (
	// `testdata/objfmt/three-objects.offsets.txt`.
	threeCommitInflatedSize = int64(178)

	// `testdata/objfmt/ofs-delta.offsets.txt`. The `git verify-pack -v`
	// "size" field reports the inflated delta-payload bytes for delta
	// entries (9 bytes of opcodes for the trailing-`tail\n` append),
	// *not* the resolved object size. The resolved sizes here are the
	// blob lengths the fixture script wrote: 17605 bytes for b.txt
	// (the base, recorded as a non-delta), and 17600 bytes for a.txt
	// (the delta target, which is b.txt minus the trailing `tail\n`).
	ofsBaseBlobOID         = "87bab3f4f5c79ca006911993eaec265a51c49a8b"
	ofsBaseBlobSize        = int64(17605)
	ofsDeltaTargetBlobOID  = "3dc05f9f416b61fc12689af37a74721129e8064d"
	ofsDeltaTargetBlobSize = int64(17600)

	// `testdata/objfmt/ref-delta.offsets.txt`. Same OIDs as the
	// `ofs-delta.pack` fixture (same input bytes); the difference is
	// the encoding (REF_DELTA vs OFS_DELTA), not the resolved object.
	refBaseBlobOID        = ofsBaseBlobOID
	refDeltaTargetBlobOID = ofsDeltaTargetBlobOID
)

func TestObjectInfo_LooseBlobReturnsTypeAndSize(t *testing.T) {
	// Loose-first: the resolver consults `s.loose.Find` ahead of the
	// pack backend, so a loose blob should resolve without ever
	// touching `s.packs`. Type and size match the canonical fixture
	// payload.
	s := openStoreFromFixture(t, "loose-objects")

	got, err := s.ObjectInfo(hashFromHex(t, looseFixtureBlobOID, objfmt.SHA1))
	require.NoError(t, err)
	assert.Equal(t, objfmt.TypeBlob, got.Type)
	assert.Equal(t, looseFixtureBlobSize, got.Size)
}

func TestObjectInfo_PackedCommitReturnsTypeAndSize(t *testing.T) {
	// Pack-only repo: the resolver misses on loose, hits on packs, and
	// returns the non-delta header's type / inflated size verbatim. The
	// commit OID lives at offset 12 in `three-objects.pack` and
	// inflates to 178 bytes per the offsets sidecar.
	s := openStoreFromFixture(t, "pack-only")

	got, err := s.ObjectInfo(hashFromHex(t, threeCommitOID, objfmt.SHA1))
	require.NoError(t, err)
	assert.Equal(t, objfmt.TypeCommit, got.Type)
	assert.Equal(t, threeCommitInflatedSize, got.Size)
}

func TestObjectInfo_OfsDeltaResolvesToBaseTypeWithDeltaTargetSize(t *testing.T) {
	// 1-deep OFS_DELTA: the OID points at a delta entry whose base is
	// the other blob in the same pack. The resolver walks one OFS_DELTA
	// hop, lands on the base, and returns the base's type ([objfmt.TypeBlob])
	// alongside the target size carried by the delta payload's leading
	// varint — *not* the base blob's inflated size and *not* the
	// delta-payload size.
	s := openStoreFromFixture(t, "ofs-delta-pack")

	got, err := s.ObjectInfo(hashFromHex(t, ofsDeltaTargetBlobOID, objfmt.SHA1))
	require.NoError(t, err)
	assert.Equal(t, objfmt.TypeBlob, got.Type)
	assert.Equal(t, ofsDeltaTargetBlobSize, got.Size,
		"size must come from the delta target, not the base blob (%d)",
		ofsBaseBlobSize)
}

func TestObjectInfo_OfsDeltaBaseStillResolves(t *testing.T) {
	// Sanity: the same `ofs-delta-pack` fixture's base blob must
	// resolve as a normal non-delta blob. Catches a regression where
	// the walker would treat every entry in a delta-bearing pack as a
	// delta.
	s := openStoreFromFixture(t, "ofs-delta-pack")

	got, err := s.ObjectInfo(hashFromHex(t, ofsBaseBlobOID, objfmt.SHA1))
	require.NoError(t, err)
	assert.Equal(t, objfmt.TypeBlob, got.Type)
	assert.Equal(t, ofsBaseBlobSize, got.Size)
}

func TestObjectInfo_CrossPackRefDelta(t *testing.T) {
	// Synthesise a two-pack repo whose REF_DELTA carrier names a base
	// that lives in a sibling pack. The resolver must consult the
	// cross-pack lookup (via `s.packs.Lookup`) to find the base before
	// returning the delta-target size.
	dir := t.TempDir()
	makeCrossPackRefDeltaRepo(t, dir)

	s, err := Open(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	got, err := s.ObjectInfo(hashFromHex(t, refDeltaTargetBlobOID, objfmt.SHA1))
	require.NoError(t, err)
	assert.Equal(t, objfmt.TypeBlob, got.Type)
	assert.Equal(t, ofsDeltaTargetBlobSize, got.Size,
		"size must come from the delta target, not the base blob (%d)",
		ofsBaseBlobSize)
}

func TestObjectInfo_RefDeltaPositiveCacheSurvivesPackRemoval(t *testing.T) {
	// Cross-pack REF_DELTA is cached: the second call must not re-scan
	// the pack set. Force the issue by deleting the carrier pack between
	// calls — the first call seeds the cache, the second resolves
	// straight from it without touching the now-missing files.
	dir := t.TempDir()
	makeCrossPackRefDeltaRepo(t, dir)

	s, err := Open(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	first, err := s.ObjectInfo(hashFromHex(t, refDeltaTargetBlobOID, objfmt.SHA1))
	require.NoError(t, err)

	// Second call must hit the cache; mutate the on-disk state to
	// prove the resolver did not re-walk the backend.
	require.NoError(t, os.Remove(filepath.Join(dir,
		"objects", "pack", "ref-delta-only.pack")))
	require.NoError(t, os.Remove(filepath.Join(dir,
		"objects", "pack", "ref-delta-only.idx")))

	second, err := s.ObjectInfo(hashFromHex(t, refDeltaTargetBlobOID, objfmt.SHA1))
	require.NoError(t, err, "second call must resolve from cache")
	assert.Equal(t, first, second)
}

func TestObjectInfo_RefDeltaNegativeCacheReusesError(t *testing.T) {
	// REF_DELTA whose base does not live in any open pack: both calls
	// must surface the same `ErrCorruptObject` shape, with the second
	// served from the negative cache slot rather than re-scanning the
	// backend.
	dir := t.TempDir()
	deltaOID := makeOrphanRefDeltaRepo(t, dir)

	s, err := Open(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	_, firstErr := s.ObjectInfo(deltaOID)
	require.Error(t, firstErr)
	require.True(t, errors.Is(firstErr, ErrCorruptObject),
		"first error must wrap ErrCorruptObject, got %v", firstErr)

	// Negative cache hit on the second call. Verify the cache map
	// directly (white-box) so the assertion does not depend on a
	// side-channel like a removed pack file.
	s.refDeltaMu.Lock()
	cacheLen := len(s.refDeltaCache)
	s.refDeltaMu.Unlock()
	require.Equal(t, 1, cacheLen,
		"first call should have seeded one negative cache entry")

	_, secondErr := s.ObjectInfo(deltaOID)
	require.Error(t, secondErr)
	assert.True(t, errors.Is(secondErr, ErrCorruptObject),
		"second error must wrap ErrCorruptObject, got %v", secondErr)
}

func TestObjectInfo_MissingOIDReturnsErrNotExist(t *testing.T) {
	// Loose miss + pack miss + no alternates → `os.ErrNotExist`. The
	// `errors.Is` match is the public contract callers depend on to
	// distinguish a cold miss from a corruption report.
	s := openStoreFromFixture(t, "pack-only")

	_, err := s.ObjectInfo(hashFromHex(t,
		"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", objfmt.SHA1))
	require.Error(t, err)
	assert.True(t, errors.Is(err, os.ErrNotExist),
		"expected os.ErrNotExist, got %v", err)
}

func TestObjectInfo_CorruptDeltaPayloadWrapsErrCorruptObject(t *testing.T) {
	// Flip a byte at the start of the delta payload's compressed body
	// so the zlib stream the delta-header reader inflates fails to
	// decompress. The walker must surface the failure as
	// ErrCorruptObject — never a panic, never a clean miss.
	dir := t.TempDir()
	require.NoError(t, materializeFixtureInto(t, "ofs-delta-pack", dir))
	packPath := filepath.Join(dir, ".git", "objects", "pack", "ofs-delta.pack")

	// `ofs-delta.offsets.txt` puts the delta entry at offset 207. The
	// 1-byte type/size header is followed by a 1-byte OFS varint, so
	// the delta payload starts around offset 209. Flipping a byte
	// there guarantees a zlib-decompress failure on the delta header.
	corruptByte(t, packPath, 210)

	// Open with CRC verification disabled so the walk reaches the
	// delta-header read path; a CRC mismatch would short-circuit before
	// the zlib decoder is invoked.
	s, err := Open(filepath.Join(dir, ".git"), WithoutCRCCheck())
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	_, err = s.ObjectInfo(hashFromHex(t, ofsDeltaTargetBlobOID, objfmt.SHA1))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrCorruptObject),
		"expected ErrCorruptObject in chain, got %v", err)
}

func TestObjectInfo_CRC32MismatchWrapsErrCorruptObject(t *testing.T) {
	// Default open (CRC verification on): flipping a byte inside the
	// commit's compressed body must trip the CRC check before the
	// header is inflated. `three-objects.pack` lays the commit at
	// offset 12 and the next object (tree) starts at 131, so the
	// commit's body runs through [12, 131) — pick a byte safely in
	// the middle.
	dir := t.TempDir()
	require.NoError(t, materializeFixtureInto(t, "pack-only", dir))
	packPath := filepath.Join(dir, ".git", "objects", "pack", "three-objects.pack")
	corruptByte(t, packPath, 64)

	s, err := Open(filepath.Join(dir, ".git"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	_, err = s.ObjectInfo(hashFromHex(t, threeCommitOID, objfmt.SHA1))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrCorruptObject),
		"expected ErrCorruptObject in chain, got %v", err)
	assert.Contains(t, err.Error(), "crc32",
		"error must mention the CRC failure for diagnostics, got %v", err)
}

func TestPackBackend_IdxFor_MultiPackReturnsPairedIdx(t *testing.T) {
	// Construct a three-pack catalog by cloning the canonical
	// `three-objects.{pack,idx}` to three distinct basenames. Every pack
	// in the resulting store must round-trip through
	// `packBackend.IdxFor` to its own paired idx — never a miss, never
	// another pack's idx. Pins the (Pack -> Idx) lookup's correctness
	// independent of the underlying map vs. linear-scan implementation,
	// so the same assertion guards future re-shuffles of the backend's
	// internal storage.
	dir := t.TempDir()
	gitDir := filepath.Join(dir, ".git")
	require.NoError(t, os.MkdirAll(filepath.Join(gitDir, "objects", "pack"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(gitDir, "HEAD"),
		[]byte("ref: refs/heads/main\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(gitDir, "refs"), 0o755))

	srcDir := filepath.Join(packFixtureRoot(t, "idx-single"), "objects", "pack")
	dstDir := filepath.Join(gitDir, "objects", "pack")
	for _, base := range []string{"alpha", "beta", "gamma"} {
		clonePackPair(t, srcDir, "three-objects", dstDir, base)
	}

	s, err := Open(gitDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	cat, ok := s.packs.(*idxCatalog)
	require.True(t, ok, "fixture must select the idx-catalog backend")
	require.Len(t, cat.packs, 3,
		"three cloned pairs must surface as three catalog entries")

	for i, e := range cat.packs {
		got, ok := s.packs.IdxFor(e.pack)
		assert.Truef(t, ok,
			"slot %d: IdxFor must report the catalog's pack as known", i)
		assert.Samef(t, e.idx, got,
			"slot %d: IdxFor must return the entry's paired idx", i)
	}
}

func TestObjectInfo_MultiPackCRC32MismatchTripsRightPack(t *testing.T) {
	// Three-pack catalog where one pack's commit body has been flipped:
	// `Store.ObjectInfo` for the OID present in every pack must walk to
	// the youngest pack first (the corrupted one) and trip its CRC. A
	// regression where `IdxFor` returned the wrong pair would either
	// surface a clean answer (verifying against the wrong idx) or skip
	// verification entirely, both of which this assertion catches.
	dir := t.TempDir()
	gitDir := filepath.Join(dir, ".git")
	require.NoError(t, os.MkdirAll(filepath.Join(gitDir, "objects", "pack"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(gitDir, "HEAD"),
		[]byte("ref: refs/heads/main\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(gitDir, "refs"), 0o755))

	srcDir := filepath.Join(packFixtureRoot(t, "idx-single"), "objects", "pack")
	dstDir := filepath.Join(gitDir, "objects", "pack")
	for _, base := range []string{"oldest", "middle", "youngest"} {
		clonePackPair(t, srcDir, "three-objects", dstDir, base)
	}
	stampPackMtimes(t, gitDir, map[string]time.Time{
		"oldest.pack":   packMtimeAnchor,
		"middle.pack":   packMtimeAnchor.Add(time.Hour),
		"youngest.pack": packMtimeAnchor.Add(2 * time.Hour),
	})

	// Corrupt the youngest pack — that is the one `Lookup` walks to
	// first under canonical pack ordering, so its CRC is what
	// verification consults.
	corruptByte(t, filepath.Join(dstDir, "youngest.pack"), 64)

	s, err := Open(gitDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	_, err = s.ObjectInfo(hashFromHex(t, threeCommitOID, objfmt.SHA1))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrCorruptObject),
		"corrupted youngest pack must trip CRC, got %v", err)
	assert.Contains(t, err.Error(), "crc32",
		"error must name the CRC failure for diagnostics, got %v", err)
	assert.Contains(t, err.Error(), "youngest",
		"error must name the youngest pack as the offender, got %v", err)
}

func TestObjectInfo_WithoutCRCCheckBypassesVerification(t *testing.T) {
	// Same flipped-byte fixture as the CRC test: with CRC verification
	// disabled the resolver must succeed (the on-disk header is still
	// readable; the corrupted byte sits past the type/size varint and
	// the body is not inflated for `ObjectInfo`).
	dir := t.TempDir()
	require.NoError(t, materializeFixtureInto(t, "pack-only", dir))
	packPath := filepath.Join(dir, ".git", "objects", "pack", "three-objects.pack")
	corruptByte(t, packPath, 64)

	s, err := Open(filepath.Join(dir, ".git"), WithoutCRCCheck())
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	got, err := s.ObjectInfo(hashFromHex(t, threeCommitOID, objfmt.SHA1))
	require.NoError(t, err,
		"WithoutCRCCheck must skip the verification that would otherwise trip")
	assert.Equal(t, objfmt.TypeCommit, got.Type)
	assert.Equal(t, threeCommitInflatedSize, got.Size)
}

func TestObjectInfo_DeepOfsDeltaChainResolvesBelowBound(t *testing.T) {
	// Synthetic 8-deep OFS_DELTA chain: every entry but the terminal
	// blob is a delta. Asking `ObjectInfo` for the head must walk all
	// eight hops, land on the blob, and report the delta target size
	// (2 bytes, per the synthetic delta payload). The canonical
	// `ofs-delta.pack` fixture only carries a 1-deep chain; this test
	// covers the iterative-walk loop at a depth deltas at scale rather
	// than its single-hop fast path.
	const chainDepth = 8
	dir := t.TempDir()
	headOID := makeDeepOfsDeltaChain(t, dir, chainDepth)

	s, err := Open(dir, WithoutCRCCheck())
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	got, err := s.ObjectInfo(headOID)
	require.NoError(t, err)
	assert.Equal(t, objfmt.TypeBlob, got.Type)
	assert.Equal(t, int64(2), got.Size,
		"target size must come from the synthetic delta header (2 bytes)")
}

func TestObjectInfo_ChainDepthExceededWrapsErrCorruptObject(t *testing.T) {
	// Synthesise a pack carrying a deliberately-long OFS_DELTA chain:
	// one terminal blob followed by [maxChainDepth + 1] OFS_DELTA
	// entries, each pointing back at its immediate predecessor. Asking
	// `ObjectInfo` for the chain head must trip the depth bound and
	// surface `ErrCorruptObject` rather than recurse indefinitely.
	dir := t.TempDir()
	headOID := makeDeepOfsDeltaChain(t, dir, maxChainDepth+1)

	s, err := Open(dir, WithoutCRCCheck())
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	_, err = s.ObjectInfo(headOID)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrCorruptObject),
		"expected ErrCorruptObject in chain, got %v", err)
	assert.Contains(t, err.Error(), "depth",
		"error must mention chain depth, got %v", err)
}

func TestObjectInfo_AlternatesFallThrough(t *testing.T) {
	// `pack-with-alternates` has an empty local objects directory and
	// an alternates pointer at a sibling repo carrying the
	// `three-objects` pack. The resolver must miss locally on every
	// layer (loose + packs) and recurse into the alternate to find
	// the OID.
	root := materializeFixture(t, "pack-with-alternates")
	main := filepath.Join(root, "main")

	s, err := Open(main)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	require.NotEmpty(t, s.alternates,
		"alternates fixture must surface at least one alternate Store")

	got, err := s.ObjectInfo(hashFromHex(t, threeCommitOID, objfmt.SHA1))
	require.NoError(t, err)
	assert.Equal(t, objfmt.TypeCommit, got.Type)
	assert.Equal(t, threeCommitInflatedSize, got.Size)
}

func TestObjectInfo_ConcurrentSameOIDConverges(t *testing.T) {
	// Twenty goroutines hammer the same OID through `ObjectInfo`.
	// Under `-race` the run must stay clean and every result must
	// match. Catches both a data race in the cross-pack REF_DELTA cache
	// and any subtler accidental sharing in the walker.
	dir := t.TempDir()
	makeCrossPackRefDeltaRepo(t, dir)

	s, err := Open(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	target := hashFromHex(t, refDeltaTargetBlobOID, objfmt.SHA1)
	want, err := s.ObjectInfo(target)
	require.NoError(t, err)

	const goroutines = 20
	results := make([]Info, goroutines)
	errs := make([]error, goroutines)
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := range goroutines {
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = s.ObjectInfo(target)
		}(i)
	}
	wg.Wait()

	for i := range goroutines {
		require.NoErrorf(t, errs[i], "goroutine %d", i)
		assert.Equalf(t, want, results[i], "goroutine %d disagrees", i)
	}
}

// materializeFixtureInto copies the named fixture into dir, renaming
// every `dotgit` component to `.git` on the way through. Mirrors
// [materializeFixture] but writes into a caller-supplied directory so
// the tests that mutate fixture bytes can stay in control of the
// destination's lifetime.
func materializeFixtureInto(t *testing.T, name, dir string) error {
	t.Helper()
	wd, err := os.Getwd()
	require.NoError(t, err)
	src := filepath.Join(wd, "..", "..", "testdata", "repos", name)
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		parts := splitAll(rel)
		for i, p := range parts {
			if p == "dotgit" {
				parts[i] = ".git"
			}
		}
		target := filepath.Join(append([]string{dir}, parts...)...)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, raw, 0o644)
	})
}

// corruptByte flips every bit of one byte at off in path. Used by the
// CRC-mismatch and corrupt-zlib tests to introduce deterministic
// damage at a known location.
func corruptByte(t *testing.T, path string, off int64) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(t, err)
	defer f.Close()
	var b [1]byte
	_, err = f.ReadAt(b[:], off)
	require.NoError(t, err)
	b[0] ^= 0xff
	_, err = f.WriteAt(b[:], off)
	require.NoError(t, err)
}

// makeCrossPackRefDeltaRepo synthesises a repo at root containing two
// packs split out of the canonical `ref-delta.pack`:
//
//   - `ref-delta-base.pack` carries only the base blob (87bab3f4...)
//     at the same compressed bytes the canonical pack uses.
//   - `ref-delta-only.pack` carries only the REF_DELTA entry
//     (3dc05f9f...) whose base hash references the blob in the
//     sibling pack.
//
// Both packs are paired with hand-rolled v2 idx files so the
// `idxCatalog` backend can resolve OIDs across them. The resulting
// layout exercises the cross-pack REF_DELTA fall-through that a
// single-pack fixture cannot reach.
func makeCrossPackRefDeltaRepo(t *testing.T, root string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "objects", "pack"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "HEAD"),
		[]byte("ref: refs/heads/main\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "refs"), 0o755))

	wd, err := os.Getwd()
	require.NoError(t, err)
	srcPack := filepath.Join(wd, "..", "..", "testdata", "objfmt", "ref-delta.pack")
	src, err := os.ReadFile(srcPack)
	require.NoError(t, err)

	// `ref-delta.offsets.txt` pins the layout:
	//   blob    87bab3f4... at offset 12 (compressed body 195 bytes)
	//   delta   3dc05f9f... at offset 207 (REF_DELTA, 38 bytes)
	const (
		baseOffset    = int64(12)
		baseEnd       = int64(207)
		deltaOffset   = int64(207)
		trailerLength = int64(20) // SHA-1
	)
	deltaEnd := int64(len(src)) - trailerLength

	baseBytes := src[baseOffset:baseEnd]
	deltaBytes := src[deltaOffset:deltaEnd]

	baseOID, err := objfmt.ParseHex(refBaseBlobOID, objfmt.SHA1)
	require.NoError(t, err)
	deltaOID, err := objfmt.ParseHex(refDeltaTargetBlobOID, objfmt.SHA1)
	require.NoError(t, err)

	basePack, baseIdx := buildSinglePackAndIdx(t,
		[]packEntryWire{{oid: baseOID, body: baseBytes}})
	deltaPack, deltaIdx := buildSinglePackAndIdx(t,
		[]packEntryWire{{oid: deltaOID, body: deltaBytes}})

	require.NoError(t, os.WriteFile(
		filepath.Join(root, "objects", "pack", "ref-delta-base.pack"),
		basePack, 0o644))
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "objects", "pack", "ref-delta-base.idx"),
		baseIdx, 0o644))
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "objects", "pack", "ref-delta-only.pack"),
		deltaPack, 0o644))
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "objects", "pack", "ref-delta-only.idx"),
		deltaIdx, 0o644))
}

// makeOrphanRefDeltaRepo synthesises a repo containing exactly one
// pack: a REF_DELTA entry whose base OID is unreachable from any
// pack in the store (the base blob is *not* shipped). Returns the
// REF_DELTA's own OID for the test to feed into `ObjectInfo`.
func makeOrphanRefDeltaRepo(t *testing.T, root string) objfmt.Hash {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "objects", "pack"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "HEAD"),
		[]byte("ref: refs/heads/main\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "refs"), 0o755))

	wd, err := os.Getwd()
	require.NoError(t, err)
	srcPack := filepath.Join(wd, "..", "..", "testdata", "objfmt", "ref-delta.pack")
	src, err := os.ReadFile(srcPack)
	require.NoError(t, err)

	const (
		deltaOffset   = int64(207)
		trailerLength = int64(20)
	)
	deltaBytes := src[deltaOffset : int64(len(src))-trailerLength]
	deltaOID, err := objfmt.ParseHex(refDeltaTargetBlobOID, objfmt.SHA1)
	require.NoError(t, err)

	pack, idx := buildSinglePackAndIdx(t,
		[]packEntryWire{{oid: deltaOID, body: deltaBytes}})
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "objects", "pack", "orphan-ref-delta.pack"),
		pack, 0o644))
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "objects", "pack", "orphan-ref-delta.idx"),
		idx, 0o644))
	return deltaOID
}

// makeDeepOfsDeltaChain synthesises a SHA-1 pack at root containing
// one terminal blob followed by depth OFS_DELTA entries, each pointing
// back at its immediate predecessor. The returned OID identifies the
// chain *head* — the deepest delta — and is the OID a depth-bound test
// asks `Store.ObjectInfo` to resolve.
//
// Every OFS_DELTA carries the same hand-rolled body: a 1-byte
// type/size header (type=6, encoded size=2), a 1-byte OFS varint
// pointing back at the previous entry's offset, and a zlib stream
// that decompresses to two leading varints (source size = target
// size = 2). The walker only inflates the head's delta body via
// [objfmt.Pack.ReadDeltaHeader]; subsequent iterations only read
// [objfmt.Pack.ReadHeader] and so never look past the OFS varint.
//
// The terminal entry is a real (zlib-encoded) one-byte blob so the
// last hop lands on a non-delta header. The chain is laid out with
// the base at the lowest offset and each delta at a higher offset, so
// `at -= ofsBase` walks from head to base in the canonical direction.
func makeDeepOfsDeltaChain(t *testing.T, root string, depth int) objfmt.Hash {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "objects", "pack"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "HEAD"),
		[]byte("ref: refs/heads/main\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "refs"), 0o755))

	// --- Entry 0: terminal blob -------------------------------------
	// One-byte body so the loose-object framing fits in a tiny zlib
	// stream. The exact byte is irrelevant — the walker never inflates
	// the body of a non-delta entry.
	baseBody := buildBlobEntry(t, []byte("x"))

	// Each OFS_DELTA shares the same bytes shape. The OFS varint is
	// single-byte (relative offset < 128) for as long as the
	// per-entry stride stays small; a synthetic delta with a 2-byte
	// inflated header plus a single opcode comfortably fits.
	deltaBodyTail := buildSyntheticDeltaBody(t)

	pack := new(bytes.Buffer)
	pack.Write([]byte("PACK"))
	_ = binary.Write(pack, binary.BigEndian, uint32(2))
	_ = binary.Write(pack, binary.BigEndian, uint32(depth+1))

	type entryRecord struct {
		oid    objfmt.Hash
		offset uint32
		crc    uint32
	}
	records := make([]entryRecord, 0, depth+1)

	// Place the terminal blob first.
	baseOffset := uint32(pack.Len())
	pack.Write(baseBody)
	records = append(records, entryRecord{
		oid:    syntheticOID(0),
		offset: baseOffset,
		crc:    crc32.ChecksumIEEE(baseBody),
	})

	// Build each delta entry: 1 header byte (type=6, size=2) + 1 OFS
	// varint pointing back to the previous entry's start + the shared
	// zlib delta body.
	prevOffset := int64(baseOffset)
	for d := 1; d <= depth; d++ {
		startOff := pack.Len()
		var entry bytes.Buffer
		entry.WriteByte(0x62) // type=6 (OFS_DELTA), size_low4=2, no continuation
		// OFS varint: relative offset back to the previous entry. The
		// chain stride is small enough that a single byte suffices —
		// the assertion below guards against silently producing an
		// unparseable header if the stride ever drifts above 127.
		rel := int64(startOff) - prevOffset
		require.Lessf(t, rel, int64(0x80),
			"depth-chain stride grew past 1-byte OFS varint at depth %d", d)
		entry.WriteByte(byte(rel))
		entry.Write(deltaBodyTail)

		records = append(records, entryRecord{
			oid:    syntheticOID(uint8(d)),
			offset: uint32(startOff),
			crc:    crc32.ChecksumIEEE(entry.Bytes()),
		})
		pack.Write(entry.Bytes())
		prevOffset = int64(startOff)
	}

	trailer := sha1.Sum(pack.Bytes())
	pack.Write(trailer[:])

	// --- Idx (v2 SHA-1) ---------------------------------------------
	slices.SortFunc(records, func(a, b entryRecord) int {
		return bytes.Compare(a.oid[:20], b.oid[:20])
	})

	idx := new(bytes.Buffer)
	idx.Write([]byte{0xff, 't', 'O', 'c'})
	_ = binary.Write(idx, binary.BigEndian, uint32(2))
	for n := range 256 {
		var count uint32
		for _, r := range records {
			if r.oid[0] <= byte(n) {
				count++
			}
		}
		_ = binary.Write(idx, binary.BigEndian, count)
	}
	for _, r := range records {
		idx.Write(r.oid[:20])
	}
	for _, r := range records {
		_ = binary.Write(idx, binary.BigEndian, r.crc)
	}
	for _, r := range records {
		_ = binary.Write(idx, binary.BigEndian, r.offset)
	}
	idx.Write(trailer[:])
	idxSum := sha1.Sum(idx.Bytes())
	idx.Write(idxSum[:])

	require.NoError(t, os.WriteFile(
		filepath.Join(root, "objects", "pack", "deep-chain.pack"),
		pack.Bytes(), 0o644))
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "objects", "pack", "deep-chain.idx"),
		idx.Bytes(), 0o644))

	// The chain head is the deepest delta — the entry the walker is
	// asked about. Its synthetic OID is `syntheticOID(uint8(depth))`.
	return syntheticOID(uint8(depth))
}

// buildBlobEntry frames body as a non-delta blob entry suitable for
// dropping verbatim into a synthetic pack: one type/size header byte
// (type=3 [TypeBlob]) followed by a zlib-compressed body. The size
// must fit in 4 bits (≤ 15 bytes) so the header stays one byte —
// generous for any synthetic-fixture payload.
func buildBlobEntry(t *testing.T, body []byte) []byte {
	t.Helper()
	require.LessOrEqual(t, len(body), 15,
		"buildBlobEntry only frames bodies that fit a 4-bit size field")
	var buf bytes.Buffer
	// Type=3 (blob), size in low 4 bits, no continuation.
	buf.WriteByte(0x30 | byte(len(body)))
	buf.Write(zlibCompress(t, body))
	return buf.Bytes()
}

// buildSyntheticDeltaBody returns a zlib stream that decompresses to
// the canonical delta-payload prologue: source size = target size = 2,
// followed by a single no-op insert opcode. The walker only inflates
// the leading varints via [objfmt.Pack.ReadDeltaHeader]; the trailing
// opcode is never executed by `Store.ObjectInfo`.
func buildSyntheticDeltaBody(t *testing.T) []byte {
	t.Helper()
	// Two single-byte varints (each value 2; high bit clear) plus one
	// extra byte to pad the inflate buffer past the varints.
	return zlibCompress(t, []byte{0x02, 0x02, 0x00})
}

// syntheticOID fabricates a deterministic [objfmt.Hash] keyed on a
// 1-byte tag. The hashes are not real SHA-1s of any object; they are
// only needed to populate the idx so the walker can resolve the
// chain head by OID. Distinct tags yield distinct OIDs.
func syntheticOID(tag uint8) objfmt.Hash {
	var h objfmt.Hash
	for i := range 20 {
		h[i] = tag
	}
	// Perturb one byte so two adjacent tags do not collide on the
	// fan-out's first-byte bucket — keeps the v2 idx fan-out
	// monotonic for the few entries the test installs.
	h[19] ^= byte(tag * 13)
	return h
}

// zlibCompress encodes body with the default zlib compressor. Used
// by the synthetic-pack helpers above to build per-entry bodies.
func zlibCompress(t *testing.T, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	_, err := io.Copy(zw, bytes.NewReader(body))
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

// packEntryWire is one (oid, raw-on-disk-body) pair used by
// [buildSinglePackAndIdx] to assemble a synthetic pack/idx pair. The
// body is the per-object byte slice from a canonical pack — type/size
// header, optional delta-base bytes, and the zlib-compressed payload —
// copied verbatim so the resulting pack records the same delta encoding
// the source pack used.
type packEntryWire struct {
	oid  objfmt.Hash
	body []byte
}

// buildSinglePackAndIdx assembles a SHA-1 pack version 2 plus its v2
// idx for entries (in entries order). Each entry's `body` is appended
// after the 12-byte pack header; the trailer is computed afresh and
// the idx records each (oid, offset, crc32) triple keyed on the body
// slice the entry contributed.
//
// The function is intentionally narrow in scope: it is only used by
// the cross-pack REF_DELTA tests in this file, where the per-object
// bodies come from slicing the canonical `ref-delta.pack` fixture.
// Generalising it would invite premature abstraction; if a second
// caller appears, the right move is to lift it into a shared helper
// alongside the other test scaffolding in this package.
func buildSinglePackAndIdx(t *testing.T, entries []packEntryWire) (packBytes, idxBytes []byte) {
	t.Helper()

	// --- Pack body ---------------------------------------------------
	pack := new(bytes.Buffer)
	pack.Write([]byte("PACK"))
	_ = binary.Write(pack, binary.BigEndian, uint32(2))
	_ = binary.Write(pack, binary.BigEndian, uint32(len(entries)))

	type recorded struct {
		oid    objfmt.Hash
		offset uint32
		crc    uint32
	}
	records := make([]recorded, len(entries))
	for i, e := range entries {
		records[i].oid = e.oid
		records[i].offset = uint32(pack.Len())
		records[i].crc = crc32.ChecksumIEEE(e.body)
		pack.Write(e.body)
	}
	trailer := sha1.Sum(pack.Bytes())
	pack.Write(trailer[:])

	// --- Idx (v2 SHA-1) ---------------------------------------------
	// Sort records by OID so the binary-search invariant holds.
	slices.SortFunc(records, func(a, b recorded) int {
		return bytes.Compare(a.oid[:20], b.oid[:20])
	})

	idx := new(bytes.Buffer)
	idx.Write([]byte{0xff, 't', 'O', 'c'})
	_ = binary.Write(idx, binary.BigEndian, uint32(2))

	// 256-entry fan-out: cumulative count of OIDs whose first byte ≤ N.
	for n := range 256 {
		var count uint32
		for _, r := range records {
			if r.oid[0] <= byte(n) {
				count++
			}
		}
		_ = binary.Write(idx, binary.BigEndian, count)
	}
	for _, r := range records {
		idx.Write(r.oid[:20])
	}
	for _, r := range records {
		_ = binary.Write(idx, binary.BigEndian, r.crc)
	}
	for _, r := range records {
		_ = binary.Write(idx, binary.BigEndian, r.offset)
	}
	// Pack-trailer copy followed by idx self-checksum.
	idx.Write(trailer[:])
	idxSum := sha1.Sum(idx.Bytes())
	idx.Write(idxSum[:])

	return pack.Bytes(), idx.Bytes()
}

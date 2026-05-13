package objstore

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
)

// TestStore_ConcurrentReadsRaceClean is the cross-method `-race` probe
// for the [Store[objfmt.SHA1Hash]] doc claim that "*Store[objfmt.SHA1Hash] is safe for concurrent reads
// from multiple goroutines once Open has returned." The per-method
// concurrent tests in `peel_test.go` and `object_info_test.go` already
// fence each cache mutation against itself; this test fences the public
// surface as a whole — every method on the same Store[objfmt.SHA1Hash], hammered from
// many goroutines simultaneously, must stay race-clean AND return the
// same answers every iteration.
//
// The fixture combines:
//
//   - The four loose objects from `testdata/repos/loose-objects/`
//     (blob, tree, commit, annotated tag pointing at the commit) so the
//     loose-object lookup, the [Store[objfmt.SHA1Hash].Peel] cache, and the loose path
//     of [Store[objfmt.SHA1Hash].ObjectInfo] all see contention.
//   - The canonical `three-objects.{pack,idx}` pair from
//     `testdata/objfmt/` so a known-good pack offset feeds the CRC
//     verification path of [Store[objfmt.SHA1Hash].ObjectInfo].
//   - The canonical `ref-delta.{pack,idx}` pair so a probe of the
//     REF_DELTA target OID drives the cross-pack base resolver
//     ([Store[objfmt.SHA1Hash].lookupRefDeltaBase]) and races every worker on the
//     shared `refDeltaCache` install.
//   - A hand-rolled `packed-refs` file that resolves `refs/heads/main`
//     (so [Store[objfmt.SHA1Hash].Head] is non-unborn), records the annotated tag at
//     `refs/tags/v1` with its `^peel` line, and pins the packed-only
//     commit at `refs/tags/three` so [Store[objfmt.SHA1Hash].IterRefs] surfaces it.
//
// The per-iteration probe set is seven methods (IterRefs, Head[objfmt.SHA1Hash], Peel,
// ObjectInfo×4 covering loose blob + loose commit + packed commit +
// REF_DELTA target, and Algo). The shared start barrier pins every
// worker on `<-start` until the test loop releases them, maximising
// the contention window the race detector gets to inspect.
func TestStore_ConcurrentReadsRaceClean(t *testing.T) {
	t.Parallel()

	s := openConcurrencyFixture(t)

	// Snapshot expectations once on the orchestrator so the worker loop
	// stays a tight assertion sweep — every iteration compares against
	// the same byte values without re-reading anything from disk.
	tagOID := hashFromHex(t, looseFixtureTagOID, objfmt.SHA1)
	commitOID := hashFromHex(t, looseFixtureCommitOID, objfmt.SHA1)
	blobOID := hashFromHex(t, looseFixtureBlobOID, objfmt.SHA1)
	packedOID := hashFromHex(t, threeCommitOID, objfmt.SHA1)
	refDeltaOID := hashFromHex(t, refDeltaTargetBlobOID, objfmt.SHA1)

	wantHead, err := s.Head()
	require.NoError(t, err)
	require.Equal(t, wantHead.OID, commitOID,
		"fixture invariant: HEAD must resolve to the loose commit")

	wantRefs := snapshotRefs(t, s)
	require.NotEmpty(t, wantRefs)

	wantPeeled, ok, err := s.Peel(tagOID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, wantPeeled, commitOID)

	wantBlobInfo, err := s.ObjectInfo(blobOID)
	require.NoError(t, err)
	require.Equal(t, objfmt.TypeBlob, wantBlobInfo.Type)

	wantCommitInfo, err := s.ObjectInfo(commitOID)
	require.NoError(t, err)
	require.Equal(t, objfmt.TypeCommit, wantCommitInfo.Type)

	wantPackedInfo, err := s.ObjectInfo(packedOID)
	require.NoError(t, err)
	require.Equal(t, objfmt.TypeCommit, wantPackedInfo.Type)
	require.Equal(t, threeCommitInflatedSize, wantPackedInfo.Size)

	wantRefDeltaInfo, err := s.ObjectInfo(refDeltaOID)
	require.NoError(t, err)
	require.Equal(t, objfmt.TypeBlob, wantRefDeltaInfo.Type,
		"REF_DELTA target must resolve through the cross-pack base lookup")
	require.Equal(t, ofsDeltaTargetBlobSize, wantRefDeltaInfo.Size)

	// Reset the caches before the hammer phase so the workers race on
	// cold caches rather than every probe trivially hitting the
	// memoised entry the orchestrator just installed. This is the part
	// of the surface the per-method tests do not cover at this scale:
	// many writers concurrently seeding the same cache slot.
	s.peelMu.Lock()
	s.peelCache = make(map[objfmt.SHA1Hash]peelEntry[objfmt.SHA1Hash])
	s.peelMu.Unlock()
	s.refDeltaMu.Lock()
	s.refDeltaCache = make(map[objfmt.SHA1Hash]refDeltaCacheEntry[objfmt.SHA1Hash])
	s.refDeltaMu.Unlock()

	workers := max(runtime.GOMAXPROCS(0)*4, 32)
	const iterations = 64

	var (
		wg    sync.WaitGroup
		start = make(chan struct{})
	)
	wg.Add(workers)
	for w := range workers {
		go func() {
			defer wg.Done()
			<-start
			for i := range iterations {
				// IterRefs: drain the iterator and assert it agrees
				// with the orchestrator's snapshot. Comparing the
				// per-iteration map catches both backend-state races
				// (a partial yield) and result corruption that a
				// `-race` run would not flag on its own.
				gotRefs := drainRefs(t, s)
				if !assert.Equalf(t, wantRefs, gotRefs,
					"worker %d iter %d: ref snapshot drift", w, i) {
					return
				}

				// Head: a tiny constant probe whose answer must not
				// change over the run. Cheap enough to include every
				// iteration and proves the cached resolution stays
				// stable under concurrent reads of every other field.
				gotHead, err := s.Head()
				if !assert.NoErrorf(t, err, "worker %d iter %d: Head", w, i) {
					return
				}
				if !assert.Equalf(t, wantHead, gotHead,
					"worker %d iter %d: Head drift", w, i) {
					return
				}

				// Peel: the highest-contention probe — every worker
				// asks the same OID, so all but the first win the cache
				// race and the others must observe the seeded value.
				peeled, ok, err := s.Peel(tagOID)
				if !assert.NoErrorf(t, err, "worker %d iter %d: Peel", w, i) {
					return
				}
				if !assert.Truef(t, ok, "worker %d iter %d: Peel ok", w, i) {
					return
				}
				if !assert.Equalf(t, wantPeeled, peeled,
					"worker %d iter %d: Peel drift", w, i) {
					return
				}

				// ObjectInfo for the loose blob — exercises the
				// loose-first short-circuit before the pack backend.
				gotBlob, err := s.ObjectInfo(blobOID)
				if !assert.NoErrorf(t, err, "worker %d iter %d: ObjectInfo blob", w, i) {
					return
				}
				if !assert.Equalf(t, wantBlobInfo, gotBlob,
					"worker %d iter %d: blob Info drift", w, i) {
					return
				}

				// ObjectInfo for the loose commit — same loose-path
				// shape as the blob but with a different type, guarding
				// against accidental sharing of a per-call buffer that
				// would only show up on a type-mismatched second probe.
				gotCommit, err := s.ObjectInfo(commitOID)
				if !assert.NoErrorf(t, err, "worker %d iter %d: ObjectInfo commit", w, i) {
					return
				}
				if !assert.Equalf(t, wantCommitInfo, gotCommit,
					"worker %d iter %d: commit Info drift", w, i) {
					return
				}

				// ObjectInfo for a packed OID — drives the pack
				// lookup, the header read, and the CRC verification
				// path. Every worker hammering this in lockstep is the
				// only place an unsynchronised read of `pack` /
				// `IdxFor` would surface.
				gotPacked, err := s.ObjectInfo(packedOID)
				if !assert.NoErrorf(t, err, "worker %d iter %d: ObjectInfo packed", w, i) {
					return
				}
				if !assert.Equalf(t, wantPackedInfo, gotPacked,
					"worker %d iter %d: packed Info drift", w, i) {
					return
				}

				// ObjectInfo for the REF_DELTA target — drives the
				// cross-pack base resolver and races every worker on
				// the shared `refDeltaCache` install. This is the only
				// probe in the loop that exercises that mutex; without
				// it the cache reset above would be cosmetic.
				gotRefDelta, err := s.ObjectInfo(refDeltaOID)
				if !assert.NoErrorf(t, err, "worker %d iter %d: ObjectInfo refDelta", w, i) {
					return
				}
				if !assert.Equalf(t, wantRefDeltaInfo, gotRefDelta,
					"worker %d iter %d: refDelta Info drift", w, i) {
					return
				}

				// Algo is a cheap accessor; including it in the loop
				// guards the const-after-Open contract under read
				// pressure with no measurable runtime cost.
				if !assert.Equalf(t, objfmt.SHA1, s.Algo(),
					"worker %d iter %d: Algo drift", w, i) {
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()
}

// snapshotRefs drains [Store[objfmt.SHA1Hash].IterRefs] into a name -> OID map. Used
// twice: once on the orchestrator to capture the expected set, and
// once per worker iteration to compare against it. A map (rather than
// the raw [RefEntry[objfmt.SHA1Hash]] slice) keeps the comparison order-independent so a
// future backend that yields refs in a different order does not flip
// this test into a flake.
func snapshotRefs(t *testing.T, s *Store[objfmt.SHA1Hash]) map[string]objfmt.SHA1Hash {
	t.Helper()

	out := make(map[string]objfmt.SHA1Hash)
	for entry, err := range s.IterRefs() {
		require.NoError(t, err)
		out[entry.Name] = entry.OID
	}
	return out
}

// drainRefs is the worker-side sibling of [snapshotRefs]: it reports
// per-iteration failures via [testing.T.Errorf] rather than aborting
// the worker mid-flight with `require.NoError`, so a single corrupt
// iteration leaves the rest of the goroutines running for the race
// detector to keep inspecting.
func drainRefs(t *testing.T, s *Store[objfmt.SHA1Hash]) map[string]objfmt.SHA1Hash {
	t.Helper()

	out := make(map[string]objfmt.SHA1Hash)
	for entry, err := range s.IterRefs() {
		if err != nil {
			t.Errorf("IterRefs: %v", err)
			return nil
		}
		out[entry.Name] = entry.OID
	}
	return out
}

// openConcurrencyFixture builds a self-contained Store[objfmt.SHA1Hash] rooted at a
// fresh `t.TempDir()`, layering together the bytes the per-method
// concurrent tests would otherwise have to duplicate:
//
//   - HEAD points at `refs/heads/main`, resolved through packed-refs
//     to the loose-objects fixture's commit.
//   - The four loose objects from `testdata/repos/loose-objects/` are
//     copied verbatim under `objects/<aa>/<rest>`.
//   - The canonical `three-objects.{pack,idx}` pair from
//     `testdata/objfmt/` is dropped into `objects/pack/` so the pack
//     backend has a real CRC-verifiable target.
//   - The canonical `ref-delta.{pack,idx}` pair lives alongside it so
//     the cross-pack base resolver — and the `refDeltaCache` it
//     populates — sees real contention from the worker probes.
//   - A `packed-refs` file with the `peeled fully-peeled sorted`
//     traits exposes `refs/heads/main`, `refs/tags/v1` (with its
//     annotated-tag peel line), and `refs/tags/three` (the packed
//     commit's anchor).
//
// Built byte-by-byte rather than reusing [materializeFixture] because
// the combined shape does not match any single committed fixture and
// promoting it into a fixture directory would couple two unrelated
// generators (`testdata/_gen/repos.sh` and `testdata/_gen/objfmt.sh`)
// for one test's benefit.
func openConcurrencyFixture(t *testing.T) *Store[objfmt.SHA1Hash] {
	t.Helper()

	root := t.TempDir()
	gitDir := filepath.Join(root, ".git")
	require.NoError(t, os.MkdirAll(filepath.Join(gitDir, "objects", "pack"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(gitDir, "refs"), 0o755))

	// HEAD: symbolic ref onto the branch the packed-refs entry resolves.
	require.NoError(t, os.WriteFile(filepath.Join(gitDir, "HEAD"),
		[]byte("ref: refs/heads/main\n"), 0o644))

	// Copy the four loose objects into their canonical fanout slots.
	// The bytes are zlib-encoded loose objects committed to the repo;
	// the `looseObjects[objfmt.SHA1Hash]` reader treats them as opaque, so a verbatim
	// copy is sufficient.
	wd, err := os.Getwd()
	require.NoError(t, err)
	looseSrc := filepath.Join(wd, "..", "..", "testdata", "repos",
		"loose-objects", "dotgit", "objects")
	for _, oid := range []string{
		looseFixtureBlobOID,
		looseFixtureTreeOID,
		looseFixtureCommitOID,
		looseFixtureTagOID,
	} {
		copyLooseObject(t, looseSrc, filepath.Join(gitDir, "objects"), oid)
	}

	// Copy two canonical pack/idx pairs. `three-objects` carries one
	// commit, one tree, and one blob — all non-delta — so the
	// CRC-verifying pack lookup has a clean target. `ref-delta` adds
	// a REF_DELTA-encoded blob whose base lives in the same pack; the
	// resolver still routes through `s.packs.Lookup` (the cross-pack
	// scan) and races every worker on `refDeltaCache`, which is the
	// only mutex this test cares about beyond `peelCache`.
	objfmtSrc := filepath.Join(wd, "..", "..", "testdata", "objfmt")
	for _, name := range []string{
		"three-objects.pack", "three-objects.idx",
		"ref-delta.pack", "ref-delta.idx",
	} {
		copyFile(t,
			filepath.Join(objfmtSrc, name),
			filepath.Join(gitDir, "objects", "pack", name))
	}

	// Write a packed-refs file that names every probe target. The
	// `peeled fully-peeled sorted` trait header lets the loose-refs
	// backend's peel-aware short-circuit fire if it ever wants to;
	// today the `Peel` API still re-reads the loose tag body, but the
	// trait line keeps the fixture honest about the shape canonical
	// Git would emit. Entries are sorted by ref name to honour the
	// `sorted` trait.
	packedRefsBody := "" +
		"# pack-refs with: peeled fully-peeled sorted\n" +
		looseFixtureCommitOID + " refs/heads/main\n" +
		threeCommitOID + " refs/tags/three\n" +
		looseFixtureTagOID + " refs/tags/v1\n" +
		"^" + looseFixtureCommitOID + "\n"
	require.NoError(t, os.WriteFile(
		filepath.Join(gitDir, "packed-refs"),
		[]byte(packedRefsBody), 0o644))

	s, err := Open[objfmt.SHA1Hash](gitDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// copyLooseObject installs the loose object identified by hex into the
// fanout layout under dstObjects, sourced from srcObjects. Both paths
// are `objects/` directories; the helper writes `<aa>/<rest>` under
// dstObjects, creating the fanout subdirectory on demand.
func copyLooseObject(t *testing.T, srcObjects, dstObjects, hex string) {
	t.Helper()

	srcPath := filepath.Join(srcObjects, hex[:2], hex[2:])
	dstDir := filepath.Join(dstObjects, hex[:2])
	require.NoError(t, os.MkdirAll(dstDir, 0o755))
	copyFile(t, srcPath, filepath.Join(dstDir, hex[2:]))
}

// copyFile copies the file at src to dst with mode 0o644. Used for
// both pack/idx pairs and loose-object payloads; the source and
// destination always live under directories the caller has already
// created.
func copyFile(t *testing.T, src, dst string) {
	t.Helper()

	in, err := os.Open(src)
	require.NoError(t, err)
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	require.NoError(t, err)
	defer func() { _ = out.Close() }()
	_, err = io.Copy(out, in)
	require.NoError(t, err)
}

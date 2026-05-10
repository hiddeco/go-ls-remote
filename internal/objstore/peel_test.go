package objstore

import (
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Stable OIDs for the `loose-tag-of-tag` fixture. The fixture
// generator pins identity, dates, and gpg-signing off so these values
// do not drift across regenerations. The chain shape is
// v2 -> v1 -> commit, with v1 and v2 both annotated tags.
const (
	tagOfTagOuterTagOID = "9031c208c72cff5280bc08519835fbc4aef0f151"
	tagOfTagInnerTagOID = "41c635a733969a347fb8332142a51c4b9dc325fe"
	tagOfTagCommitOID   = "619aaeb618057787fe6afee2c284331701ef9583"
)

// openStoreFromFixture is the [Store[objfmt.SHA1Hash]]-level sibling of
// [openLooseObjectsFromFixture]: materializes the named fixture, opens
// the full Store[objfmt.SHA1Hash], and registers the cleanup. Centralised so each test
// stays focused on the assertion it is making.
func openStoreFromFixture(t *testing.T, name string) *Store[objfmt.SHA1Hash] {
	t.Helper()
	root := materializeFixture(t, name)
	s, err := Open[objfmt.SHA1Hash](root)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestStorePeel_AnnotatedTagResolvesToCommit(t *testing.T) {
	// Hit path: the canonical fixture's `v1` annotated tag must peel
	// straight through to the commit it points at. The peeled OID is
	// the commit fixture, not the tag fixture, so a regression in the
	// tag-body parser surfaces as a wrong-OID assertion rather than a
	// silent miss.
	s := openStoreFromFixture(t, "loose-objects")

	tagOID := hashFromHex(t, looseFixtureTagOID, objfmt.SHA1)
	commitOID := hashFromHex(t, looseFixtureCommitOID, objfmt.SHA1)

	peeled, ok, err := s.Peel(tagOID)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, commitOID, peeled)
}

func TestStorePeel_NonTagInputReturnsNotPeelable(t *testing.T) {
	// Blobs, trees, and commits are not peelable; the contract is
	// (Hash{}, false, nil) — never an error. The three-way table
	// covers each non-tag [objfmt.ObjectType] so a mis-classification
	// in the fast path (e.g. peeling a commit) shows up as a single
	// failing subtest rather than masking the whole sweep.
	s := openStoreFromFixture(t, "loose-objects")

	cases := []struct {
		name string
		oid  string
	}{
		{"blob", looseFixtureBlobOID},
		{"tree", looseFixtureTreeOID},
		{"commit", looseFixtureCommitOID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			peeled, ok, err := s.Peel(hashFromHex(t, tc.oid, objfmt.SHA1))
			require.NoError(t, err)
			assert.False(t, ok)
			assert.Equal(t, objfmt.SHA1Hash{}, peeled)
		})
	}
}

func TestStorePeel_TagOfTagRecursesToTerminalCommit(t *testing.T) {
	// Recursive peel: v2 -> v1 -> commit. The outer tag's terminal
	// target is the commit, not the inner tag. This is the canonical
	// "annotated tag of an annotated tag" shape `git tag -a v2 v1`
	// produces, and the recursion is the part of [Store[objfmt.SHA1Hash].Peel] not
	// exercised by the single-link fixture.
	s := openStoreFromFixture(t, "loose-tag-of-tag")

	outer := hashFromHex(t, tagOfTagOuterTagOID, objfmt.SHA1)
	commit := hashFromHex(t, tagOfTagCommitOID, objfmt.SHA1)

	peeled, ok, err := s.Peel(outer)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, commit, peeled)
}

func TestStorePeel_TagOfTagInnerLinkAlsoResolves(t *testing.T) {
	// The inner link of the v2 -> v1 -> commit chain is itself a
	// peelable tag. Confirms the cache key is the input OID (not the
	// chain head) so an independent call on the inner tag is decided
	// on its own merits.
	s := openStoreFromFixture(t, "loose-tag-of-tag")

	inner := hashFromHex(t, tagOfTagInnerTagOID, objfmt.SHA1)
	commit := hashFromHex(t, tagOfTagCommitOID, objfmt.SHA1)

	peeled, ok, err := s.Peel(inner)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, commit, peeled)
}

func TestStorePeel_CacheHitSurvivesUnreadableObject(t *testing.T) {
	// After the first Peel succeeds the answer must come from the
	// in-memory cache; a second call must NOT touch the loose-object
	// file. We prove that by chmod-ing the file unreadable between
	// the two calls — the second call still returns the right OID,
	// which would be impossible without the cache. Skipped as root
	// because chmod-0000 cannot block reads under uid 0 (the same
	// guard `loose_objects_test.go` uses for its permission probe).
	if os.Geteuid() == 0 {
		t.Skip("running as root; chmod-0000 cannot block reads")
	}

	root := materializeFixture(t, "loose-objects")
	s, err := Open[objfmt.SHA1Hash](root)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	tagOID := hashFromHex(t, looseFixtureTagOID, objfmt.SHA1)
	commitOID := hashFromHex(t, looseFixtureCommitOID, objfmt.SHA1)

	peeled, ok, err := s.Peel(tagOID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, commitOID, peeled)

	// Disarm the loose-object file. A backend re-read would now fail
	// with EACCES; the cache must hide that entirely.
	tagPath := filepath.Join(root, ".git", "objects",
		looseFixtureTagOID[:2], looseFixtureTagOID[2:])
	require.NoError(t, os.Chmod(tagPath, 0o000))
	t.Cleanup(func() { _ = os.Chmod(tagPath, 0o644) })

	peeled, ok, err = s.Peel(tagOID)
	require.NoError(t, err, "cache hit must not touch the file system")
	assert.True(t, ok)
	assert.Equal(t, commitOID, peeled)
}

func TestStorePeel_CacheHitForNegativeResult(t *testing.T) {
	// Negative results (`ok=false`) cache under the same shape so
	// repeated peel attempts on a non-tag OID stay O(1). The
	// observable proof: chmod the underlying loose object unreadable
	// after the first call; the second call still reports
	// (Hash{}, false, nil) because it never re-reads.
	if os.Geteuid() == 0 {
		t.Skip("running as root; chmod-0000 cannot block reads")
	}

	root := materializeFixture(t, "loose-objects")
	s, err := Open[objfmt.SHA1Hash](root)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	blobOID := hashFromHex(t, looseFixtureBlobOID, objfmt.SHA1)

	peeled, ok, err := s.Peel(blobOID)
	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, objfmt.SHA1Hash{}, peeled)

	blobPath := filepath.Join(root, ".git", "objects",
		looseFixtureBlobOID[:2], looseFixtureBlobOID[2:])
	require.NoError(t, os.Chmod(blobPath, 0o000))
	t.Cleanup(func() { _ = os.Chmod(blobPath, 0o644) })

	peeled, ok, err = s.Peel(blobOID)
	require.NoError(t, err, "cached negative result must not re-read")
	assert.False(t, ok)
	assert.Equal(t, objfmt.SHA1Hash{}, peeled)
}

func TestStorePeel_DepthBoundCollapsesToNotPeelable(t *testing.T) {
	// The `loose-tag-deep` fixture is a 17-link annotated-tag chain.
	// `Peel(v17)` must walk only 16 links and then surface the "not
	// peelable" shape rather than recurse forever (or wrap an
	// error). Per the doc, depth overrun is a non-error: callers see
	// the same (Hash{}, false, nil) shape they get for any other
	// non-peelable input.
	s := openStoreFromFixture(t, "loose-tag-deep")

	v17 := readRefOID(t, s, "refs/tags/v17")

	peeled, ok, err := s.Peel(v17)
	require.NoError(t, err)
	assert.False(t, ok, "17-link chain must hit the depth bound")
	assert.Equal(t, objfmt.SHA1Hash{}, peeled)
}

func TestStorePeel_DepthBoundOverrunIsNotCached(t *testing.T) {
	// Depth overrun must NOT poison the cache: a future bump of
	// `maxPeelDepth` is supposed to make a previously-overrunning
	// chain resolvable on the next call without a Store[objfmt.SHA1Hash] restart, and
	// the only way that contract holds is if the first call's
	// "not peelable" answer was never written to the cache. Probe the
	// invariant by removing the chain head between two `Peel` calls.
	// If the overrun result was cached the second call returns the
	// cached `(Hash{}, false, nil)` cleanly; if it was NOT cached the
	// second call retries `loose.Find` against the now-missing object
	// and reports the underlying I/O failure. Confirming the second
	// shape pins the no-cache behaviour.
	s := openStoreFromFixture(t, "loose-tag-deep")
	v17 := readRefOID(t, s, "refs/tags/v17")

	peeled, ok, err := s.Peel(v17)
	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, objfmt.SHA1Hash{}, peeled)

	// Delete the chain-head loose object so any uncached re-read fails
	// loudly. If the overrun was cached the second call would return
	// (Hash{}, false, nil) silently from the cache.
	objPath := filepath.Join(s.loose.commonDir, "objects",
		v17.Hex()[:2], v17.Hex()[2:])
	require.NoError(t, os.Remove(objPath))

	peeled2, ok2, err2 := s.Peel(v17)
	require.NoError(t, err2,
		"deleting the head plus a cache miss should still surface as a clean miss")
	require.False(t, ok2,
		"the missing-object cache miss must collapse to not-peelable")
	require.Equal(t, objfmt.SHA1Hash{}, peeled2)

	// A direct probe via `loose.Find` confirms the file really is gone:
	// without that, the cache-miss assertion above would be vacuous.
	_, _, body, foundOK, findErr := s.loose.Find(v17)
	require.NoError(t, findErr)
	require.False(t, foundOK,
		"sanity: deleted loose object must miss on Find")
	if body != nil {
		_ = body.Close()
	}
}

func TestStorePeel_DepthBoundShortChainStillResolves(t *testing.T) {
	// Sanity check: the 16-link chain (v16 -> v15 -> ... -> v1 ->
	// commit) sits exactly at the depth limit and must still resolve.
	// Pairing this with the v17 case fences the bound from both sides:
	// a regression that off-by-ones the threshold would flip exactly
	// one of the two assertions.
	s := openStoreFromFixture(t, "loose-tag-deep")

	v16 := readRefOID(t, s, "refs/tags/v16")
	peeled, ok, err := s.Peel(v16)
	require.NoError(t, err)
	require.True(t, ok, "16-link chain sits at the bound and must peel")

	// The terminal commit is the same one v1 points at; reading it
	// from the v1 ref keeps the assertion grounded in the fixture
	// rather than a hardcoded OID that could drift.
	v1 := readRefOID(t, s, "refs/tags/v1")
	v1Peeled, v1Ok, err := s.Peel(v1)
	require.NoError(t, err)
	require.True(t, v1Ok)
	assert.Equal(t, v1Peeled, peeled,
		"chain peel must agree with single-link peel of v1")
}

func TestStorePeel_UnknownOIDIsNotAnError(t *testing.T) {
	// Canonical Git's `peel_to_object` returns NULL for an OID it
	// cannot find; the API mirrors that with (Hash{}, false, nil)
	// rather than wrapping `os.ErrNotExist`. Callers that want a
	// "does this OID exist" probe build it on top of `Lookup` /
	// `Find`, not on top of Peel.
	s := openStoreFromFixture(t, "loose-objects")

	missing := hashFromHex(t,
		"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", objfmt.SHA1)
	peeled, ok, err := s.Peel(missing)
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Equal(t, objfmt.SHA1Hash{}, peeled)

	zero := objfmt.SHA1Hash{}
	peeled, ok, err = s.Peel(zero)
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Equal(t, objfmt.SHA1Hash{}, peeled)
}

func TestStorePeel_ConcurrentCallsConverge(t *testing.T) {
	// Race detector probe: many goroutines call Peel on the same OID
	// at the same time. Every one must observe the same answer, and
	// the cache mutation must not race (the test runs under
	// `go test -race` in CI). The fixture is a single-link tag so
	// the work is light and the contention is concentrated on the
	// cache read/write.
	s := openStoreFromFixture(t, "loose-objects")

	tagOID := hashFromHex(t, looseFixtureTagOID, objfmt.SHA1)
	commitOID := hashFromHex(t, looseFixtureCommitOID, objfmt.SHA1)

	const workers = 32
	var (
		wg    sync.WaitGroup
		start = make(chan struct{})
	)
	wg.Add(workers)
	for i := range workers {
		go func() {
			defer wg.Done()
			<-start
			// A short busy-loop per worker amplifies the contention
			// window on the cache mutex without prolonging the test
			// runtime noticeably.
			for j := range 64 {
				peeled, ok, err := s.Peel(tagOID)
				if err != nil || !ok || peeled != commitOID {
					t.Errorf("worker %d iter %d: got (%v, %v, %v); want (%x, true, nil)",
						i, j, peeled.Hex(), ok, err, commitOID)
					return
				}
			}
			runtime.Gosched()
		}()
	}
	close(start)
	wg.Wait()
}

func TestStore_PeelRef_FullyPeeledShortCircuits(t *testing.T) {
	// `packed-refs-fully-peeled` ships no objects directory; `Peel` on
	// any OID would miss with the "not peelable" shape. The annotated
	// tag entry carries `^<peel-oid>` in `packed-refs`, so PeelRef must
	// return the recorded peel hex without consulting the object store.
	// Asserting equality against the recorded hex is itself the proof:
	// no object-body read could synthesize that value here.
	root := materializeFixture(t, "packed-refs-fully-peeled")
	s, err := Open[objfmt.SHA1Hash](root)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	wantPeel := hashFromHex(t,
		"dddddddddddddddddddddddddddddddddddddddd", objfmt.SHA1)

	peeled, ok, err := s.PeelRef("refs/tags/v1")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, wantPeel, peeled)
}

func TestStore_PeelRef_FullyPeeledNoPeelShortCircuits(t *testing.T) {
	// Branch entry in a fully-peeled fixture: the absence of `^<oid>` is
	// definitive. PeelRef must return (zero, false, nil) without
	// consulting the object store.
	root := materializeFixture(t, "packed-refs-fully-peeled")
	s, err := Open[objfmt.SHA1Hash](root)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	peeled, ok, err := s.PeelRef("refs/heads/main")
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Equal(t, objfmt.SHA1Hash{}, peeled)
}

func TestStore_PeelRef_NoTraitFallsThrough(t *testing.T) {
	// `loose-tag-deep` ships annotated tags as loose ref files under
	// `refs/tags/` plus the corresponding loose tag objects. With no
	// `packed-refs` and no `fully-peeled` trait, PeelKnown is false and
	// PeelRef must fall through to Peel and return the chain's terminal
	// commit — the same shape as a direct `Peel` call on the ref's OID.
	s := openStoreFromFixture(t, "loose-tag-deep")

	v1OID := readRefOID(t, s, "refs/tags/v1")
	wantPeel, ok, err := s.Peel(v1OID)
	require.NoError(t, err)
	require.True(t, ok)

	peeled, ok, err := s.PeelRef("refs/tags/v1")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, wantPeel, peeled)

	// Sanity: the ref entry itself must NOT report PeelKnown for this
	// fixture. If it did, the test would not exercise the fall-through
	// path it claims to.
	entry, found, err := s.refs.Lookup("refs/tags/v1")
	require.NoError(t, err)
	require.True(t, found)
	require.False(t, entry.PeelKnown,
		"sanity: a loose ref without packed-refs must not have a known peel")
	assert.Equal(t, v1OID, entry.OID)
}

func TestStore_PeelRef_ReftableUsesRecordPeel(t *testing.T) {
	// Reftable records always populate the peel slot, so every Lookup
	// must surface PeelKnown=true. PeelRef short-circuits on the record
	// without falling through to the object-body read.
	root := materializeFixture(t, "with-reftable-content")
	s, err := Open[objfmt.SHA1Hash](root)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	// `refs/heads/main` is a commit, so the per-record peel slot is
	// zero. PeelRef returns (zero, false, nil).
	peeled, ok, err := s.PeelRef("refs/heads/main")
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Equal(t, objfmt.SHA1Hash{}, peeled)

	// The short-circuit signal: the same Lookup that PeelRef consults
	// reports PeelKnown=true for the reftable backend.
	entry, found, err := s.refs.Lookup("refs/heads/main")
	require.NoError(t, err)
	require.True(t, found)
	assert.True(t, entry.PeelKnown,
		"reftable Lookup must populate PeelKnown=true")
}

func TestStore_PeelRef_MissingRef(t *testing.T) {
	// A ref name absent from the backend must surface as the same
	// "no peel" shape as a non-peelable input — never an error.
	s := openStoreFromFixture(t, "loose-objects")

	peeled, ok, err := s.PeelRef("refs/heads/does-not-exist")
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Equal(t, objfmt.SHA1Hash{}, peeled)
}

// readRefOID resolves name through the store's iterator. Used by the
// depth-chain tests so the assertion does not depend on a hardcoded
// hex that the fixture generator might rotate. Iteration is O(N) in
// the ref count but the chain fixture only ships 17 refs, so the
// cost is negligible compared to keeping the assertion fixture-stable.
func readRefOID(t *testing.T, s *Store[objfmt.SHA1Hash], name string) objfmt.SHA1Hash {
	t.Helper()
	for entry, err := range s.IterRefs() {
		require.NoError(t, err)
		if entry.Name == name {
			return entry.OID
		}
	}
	t.Fatalf("ref %q not found in store", name)
	return objfmt.SHA1Hash{}
}

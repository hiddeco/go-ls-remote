package reftable

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fixturePath resolves a reftable fixture relative to testdata/reftable.
func fixturePath(t *testing.T, rel string) string {
	t.Helper()
	return filepath.Join(fixtureRoot(t), rel)
}

func TestOpenReader(t *testing.T) {
	t.Run("sha1_fixture_opens", func(t *testing.T) {
		r, err := OpenReader[objfmt.SHA1Hash](fixturePath(t, "single-sha1/0001-0001-aaaaaaaa.ref"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = r.Close() })
		assert.Equal(t, objfmt.SHA1, r.HashAlgo())
		require.NoError(t, r.Close())
	})

	t.Run("sha256_fixture_opens", func(t *testing.T) {
		r, err := OpenReader[objfmt.SHA256Hash](fixturePath(t, "single-sha256/0001-0001-aaaaaaaa.ref"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = r.Close() })
		assert.Equal(t, objfmt.SHA256, r.HashAlgo())
		require.NoError(t, r.Close())
	})

	t.Run("with_index_opens", func(t *testing.T) {
		r, err := OpenReader[objfmt.SHA1Hash](fixturePath(t, "with-index-sha1/0001-0001-aaaaaaaa.ref"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = r.Close() })
		assert.Equal(t, objfmt.SHA1, r.HashAlgo())
	})

	t.Run("hash_algo_mismatch_rejected", func(t *testing.T) {
		// Opening a SHA-256 file as a SHA-1 reader (and vice versa)
		// surfaces as [ErrMixedHashAlgo] before any record is touched.
		_, err := OpenReader[objfmt.SHA1Hash](fixturePath(t, "single-sha256/0001-0001-aaaaaaaa.ref"))
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrMixedHashAlgo), "want ErrMixedHashAlgo, got %v", err)

		_, err = OpenReader[objfmt.SHA256Hash](fixturePath(t, "single-sha1/0001-0001-aaaaaaaa.ref"))
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrMixedHashAlgo), "want ErrMixedHashAlgo, got %v", err)
	})

	t.Run("corrupt_trailer_rejected", func(t *testing.T) {
		_, err := OpenReader[objfmt.SHA1Hash](fixturePath(t, "corrupt-trailer-sha1.ref"))
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrTrailerChecksum), "want ErrTrailerChecksum, got %v", err)
	})

	t.Run("truncated_rejected", func(t *testing.T) {
		_, err := OpenReader[objfmt.SHA1Hash](fixturePath(t, "truncated-sha1.ref"))
		require.Error(t, err)
		// Either the header guard or the trailer guard fires; both
		// surface as ErrShortFile under the chosen 50-byte truncation.
		assert.True(t, errors.Is(err, ErrShortFile), "want ErrShortFile, got %v", err)
	})

	t.Run("missing_path", func(t *testing.T) {
		_, err := OpenReader[objfmt.SHA1Hash](fixturePath(t, "does-not-exist.ref"))
		require.Error(t, err)
	})
}

func TestReader_HashAlgo(t *testing.T) {
	t.Run("sha1", func(t *testing.T) {
		r, err := OpenReader[objfmt.SHA1Hash](fixturePath(t, "single-sha1/0001-0001-aaaaaaaa.ref"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = r.Close() })
		assert.Equal(t, objfmt.SHA1, r.HashAlgo())
	})

	t.Run("sha256", func(t *testing.T) {
		r, err := OpenReader[objfmt.SHA256Hash](fixturePath(t, "single-sha256/0001-0001-aaaaaaaa.ref"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = r.Close() })
		assert.Equal(t, objfmt.SHA256, r.HashAlgo())
	})
}

func TestReader_Close_Idempotent(t *testing.T) {
	r, err := OpenReader[objfmt.SHA1Hash](fixturePath(t, "single-sha1/0001-0001-aaaaaaaa.ref"))
	require.NoError(t, err)
	require.NoError(t, r.Close())
	// A second Close must not panic and must not return an error.
	require.NoError(t, r.Close())
}

func TestReader_IterRefs(t *testing.T) {
	t.Run("single_sha1_yields_records", func(t *testing.T) {
		r, err := OpenReader[objfmt.SHA1Hash](fixturePath(t, "single-sha1/0001-0001-aaaaaaaa.ref"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = r.Close() })

		var (
			recs       []RefRecord[objfmt.SHA1Hash]
			iterErr    error
			seenHEAD   bool
			seenMain   bool
			mainRecord RefRecord[objfmt.SHA1Hash]
		)
		for rec, err := range r.IterRefs() {
			if err != nil {
				iterErr = err
				break
			}
			recs = append(recs, rec)
			switch string(rec.Name) {
			case "HEAD":
				seenHEAD = true
			case "refs/heads/main":
				seenMain = true
				mainRecord = rec
			}
		}
		require.NoError(t, iterErr)
		require.NotEmpty(t, recs)
		assert.True(t, seenHEAD, "expected HEAD ref")
		assert.True(t, seenMain, "expected refs/heads/main")

		// HEAD is a symref pointing at refs/heads/main; refs/heads/main
		// carries a real OID. Both shapes must round-trip through the
		// public surface.
		assert.False(t, mainRecord.Value.IsZero(), "main ref must carry a non-zero OID")
		assert.Empty(t, mainRecord.Target, "main ref is a value record, not a symref")
	})

	t.Run("sha256_yields_records", func(t *testing.T) {
		r, err := OpenReader[objfmt.SHA256Hash](fixturePath(t, "single-sha256/0001-0001-aaaaaaaa.ref"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = r.Close() })

		var count int
		for rec, err := range r.IterRefs() {
			require.NoError(t, err)
			assert.NotEmpty(t, rec.Name)
			count++
		}
		assert.Greater(t, count, 0)
	})

	t.Run("with_index_yields_all_records", func(t *testing.T) {
		r, err := OpenReader[objfmt.SHA1Hash](fixturePath(t, "with-index-sha1/0001-0001-aaaaaaaa.ref"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = r.Close() })

		branches := make(map[string]bool)
		var (
			count   int
			iterErr error
		)
		for rec, err := range r.IterRefs() {
			if err != nil {
				iterErr = err
				break
			}
			count++
			name := string(rec.Name)
			if name != "" && name != "HEAD" && name != "refs/heads/main" {
				branches[name] = true
			}
		}
		require.NoError(t, iterErr)

		// The fixture writes 120 branch refs in addition to HEAD plus
		// refs/heads/main from the prelude commit; assert the floor on
		// branch count rather than the exact total to stay tolerant of
		// canonical Git's writer implementation details.
		assert.GreaterOrEqual(t, len(branches), 120, "expected at least 120 branch refs")
		assert.GreaterOrEqual(t, count, 120)
		// Spot-check a couple of well-known branch names.
		assert.True(t, branches["refs/heads/branch-1"], "branch-1 missing")
		assert.True(t, branches["refs/heads/branch-120"], "branch-120 missing")
	})

	t.Run("iter_stops_on_break", func(t *testing.T) {
		r, err := OpenReader[objfmt.SHA1Hash](fixturePath(t, "with-index-sha1/0001-0001-aaaaaaaa.ref"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = r.Close() })

		// Break early. The pull side must terminate cleanly without
		// further state changes; nothing observable to assert beyond
		// "no panic, no leaked goroutines under -race".
		var seen int
		for _, err := range r.IterRefs() {
			require.NoError(t, err)
			seen++
			if seen >= 3 {
				break
			}
		}
		assert.Equal(t, 3, seen)

		// A second iteration must restart from the beginning.
		var total int
		for range r.IterRefs() {
			total++
		}
		assert.Greater(t, total, seen)
	})

	// at_scale_fixtures_yield_exact_counts smoke-tests the
	// `many-refs-{1k,10k}-sha1` fixtures: each is a single-table
	// reftable produced by a batched `update-ref --stdin -z` plus
	// the init-created HEAD symref. The branch count is exact (1000
	// or 10000) because the fixture generator uses a fixed loop; HEAD
	// adds one symref record on top.
	t.Run("at_scale_fixtures_yield_exact_counts", func(t *testing.T) {
		for _, sc := range []struct {
			fixture      string
			wantBranches int
		}{
			{"many-refs-1k-sha1", 1000},
			{"many-refs-10k-sha1", 10000},
		} {
			t.Run(sc.fixture, func(t *testing.T) {
				r, err := OpenReader[objfmt.SHA1Hash](
					fixturePath(t, sc.fixture+"/0001-0001-aaaaaaaa.ref"))
				require.NoError(t, err)
				t.Cleanup(func() { _ = r.Close() })

				var (
					branches int
					seenHEAD bool
				)
				for rec, err := range r.IterRefs() {
					require.NoError(t, err)
					if string(rec.Name) == "HEAD" {
						seenHEAD = true
						continue
					}
					branches++
				}
				assert.Equal(t, sc.wantBranches, branches,
					"branch count for %s", sc.fixture)
				assert.True(t, seenHEAD, "HEAD missing in %s", sc.fixture)
			})
		}
	})
}

func TestReader_FindRef(t *testing.T) {
	t.Run("single_sha1_hit", func(t *testing.T) {
		r, err := OpenReader[objfmt.SHA1Hash](fixturePath(t, "single-sha1/0001-0001-aaaaaaaa.ref"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = r.Close() })

		rec, ok, err := r.FindRef("refs/heads/main")
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, []byte("refs/heads/main"), rec.Name)
		assert.False(t, rec.Value.IsZero(), "refs/heads/main must carry a non-zero OID")
	})

	t.Run("single_sha1_head_symref", func(t *testing.T) {
		r, err := OpenReader[objfmt.SHA1Hash](fixturePath(t, "single-sha1/0001-0001-aaaaaaaa.ref"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = r.Close() })

		rec, ok, err := r.FindRef("HEAD")
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, []byte("HEAD"), rec.Name)
		assert.Equal(t, []byte("refs/heads/main"), rec.Target)
		assert.True(t, rec.Value.IsZero(), "HEAD is a symref; Value must be zero")
	})

	t.Run("single_sha1_miss", func(t *testing.T) {
		r, err := OpenReader[objfmt.SHA1Hash](fixturePath(t, "single-sha1/0001-0001-aaaaaaaa.ref"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = r.Close() })

		rec, ok, err := r.FindRef("refs/heads/nonexistent")
		require.NoError(t, err)
		assert.False(t, ok)
		assert.Empty(t, rec.Name)
	})

	t.Run("with_index_hit", func(t *testing.T) {
		r, err := OpenReader[objfmt.SHA1Hash](fixturePath(t, "with-index-sha1/0001-0001-aaaaaaaa.ref"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = r.Close() })

		// branch-50 lives in the middle of the namespace; resolving it
		// must traverse the ref index rather than walking every block.
		rec, ok, err := r.FindRef("refs/heads/branch-50")
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, []byte("refs/heads/branch-50"), rec.Name)
		assert.False(t, rec.Value.IsZero())
	})

	t.Run("with_index_miss", func(t *testing.T) {
		r, err := OpenReader[objfmt.SHA1Hash](fixturePath(t, "with-index-sha1/0001-0001-aaaaaaaa.ref"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = r.Close() })

		_, ok, err := r.FindRef("refs/heads/branch-9999")
		require.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("sha256_hit", func(t *testing.T) {
		r, err := OpenReader[objfmt.SHA256Hash](fixturePath(t, "single-sha256/0001-0001-aaaaaaaa.ref"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = r.Close() })

		rec, ok, err := r.FindRef("refs/heads/main")
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, []byte("refs/heads/main"), rec.Name)
		assert.False(t, rec.Value.IsZero())
	})
}

// TestReader_IterRefs_NameContract verifies the documented contract
// that rec.Name's bytes may be overwritten by a later yield once the
// ping-pong's scratch buffer cycles back. A regression that cloned
// Name per record (silently restoring the pre-byte-typed per-record
// alloc) would let the saved slice's bytes survive unchanged; this
// test would then fail. Conversely, if a future change kept Name
// stable for the iter's lifetime by some other mechanism, the test
// would also catch it — and is the place to update the contract.
//
// The fixture's namespace (`refs/heads/branch-N`) yields keys of
// similar length, so once both scratch buffers grow they are reused
// for subsequent decodes and earlier rec.Name slices observe their
// bytes overwritten. The test grabs a snapshot a few records in
// (skipping HEAD, whose 4-byte buffer is too small to ever be
// reused) and then advances enough records that the buffer the
// snapshot aliased is decoded into again.
func TestReader_IterRefs_NameContract(t *testing.T) {
	r, err := OpenReader[objfmt.SHA1Hash](
		fixturePath(t, "many-refs-1k-sha1/0001-0001-aaaaaaaa.ref"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	// Skip the first few records so the ping-pong buffers have grown
	// large enough that further decodes reuse them. The snapshot is
	// taken from a record whose Name slice aliases one of those
	// grown buffers.
	const skip = 8
	const advance = 50

	var snapName []byte
	var snapStr string
	var saw int
	for rec, err := range r.IterRefs() {
		require.NoError(t, err)
		if rec.Name == nil {
			continue
		}
		saw++
		if saw == skip {
			snapName = rec.Name
			snapStr = string(snapName)
			continue
		}
		if saw >= skip+advance {
			break
		}
	}
	require.GreaterOrEqual(t, saw, skip+advance,
		"fixture must have at least %d records to exercise the contract", skip+advance)
	require.NotEmpty(t, snapStr, "snapshot must have been captured")
	assert.NotEqualf(t, snapStr, string(snapName),
		"rec.Name captured at yield %d must be invalidated by yield %d "+
			"per the documented lifetime; got stable bytes %q across "+
			"the scratch ping-pong", skip, saw, snapStr)
}

// TestReader_FindRef_NameStability verifies that rec.Name from a
// FindRef call is stable for the returned record's lifetime — i.e.
// a subsequent FindRef on the same Reader does not overwrite the
// previous record's Name. This is the FindRef counterpart to
// TestReader_IterRefs_NameContract.
func TestReader_FindRef_NameStability(t *testing.T) {
	r, err := OpenReader[objfmt.SHA1Hash](
		fixturePath(t, "many-refs-1k-sha1/0001-0001-aaaaaaaa.ref"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	first, found, err := r.FindRef("refs/heads/branch-1")
	require.NoError(t, err)
	require.True(t, found)
	snapshot := string(first.Name)

	// A subsequent FindRef must not overwrite the previous record's
	// bytes — each FindRef escapes its scratch to the heap with the
	// returned record.
	_, _, err = r.FindRef("refs/heads/branch-500")
	require.NoError(t, err)

	assert.Equal(t, snapshot, string(first.Name),
		"first record's Name must remain valid after a second FindRef")
}

// TestReader_FindRef_AllocBudget pins the per-call alloc count for
// a forge-scale single-block lookup. With the byte-typed Name and
// scratch-buffer ping-pong, both the index descent and the leaf-block
// walk decode keys into a single growing scratch buffer that lives for
// the duration of one `FindRef` call. After the first comparator call
// the buffer is sized large enough for every subsequent decode at the
// same level (and the level below), so the steady-state per-decode
// cost is zero. The remaining allocs come from `parseBlock`, the
// `[]byte(name)` probe conversion, and a small fixed amount of
// iterator harness bookkeeping. A ceiling of 11 leaves room for
// Go-runtime alloc drift between point releases.
func TestReader_FindRef_AllocBudget(t *testing.T) {
	r, err := OpenReader[objfmt.SHA1Hash](
		fixturePath(t, "many-refs-10k-sha1/0001-0001-aaaaaaaa.ref"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	avg := testing.AllocsPerRun(50, func() {
		_, _, err := r.FindRef("refs/heads/branch-5000")
		if err != nil {
			t.Fatal(err)
		}
	})
	t.Logf("FindRef allocs/op: %.2f", avg)
	assert.LessOrEqualf(t, avg, 11.0,
		"FindRef allocs/op: got %.0f, want <= 11", avg)
}

// TestReader_IterRefs_AllocBudget pins the per-iteration cost of
// draining the iterator over 10k refs. With the byte-typed Name and
// the scratch-buffer ping-pong, every record decode reuses the
// walker's two scratch buffers (after the first record sizes them
// large enough for the fixture's namespace); the symref target
// aliases directly into the block bytes; and [parseBlock] now decodes
// the restart-offset table lazily (via [block.restart]) so no
// per-block slice is materialised on the iter path, which never
// touches the table. The residual allocations come from iterator
// harness bookkeeping (the closure capture for the `iter.Seq2`
// pair). A ceiling of 25 leaves room for Go-runtime alloc drift
// between point releases; a regression that brings even one alloc
// per record back would push this to ~10000.
func TestReader_IterRefs_AllocBudget(t *testing.T) {
	r, err := OpenReader[objfmt.SHA1Hash](
		fixturePath(t, "many-refs-10k-sha1/0001-0001-aaaaaaaa.ref"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	avg := testing.AllocsPerRun(20, func() {
		for rec, err := range r.IterRefs() {
			if err != nil {
				t.Fatal(err)
			}
			_ = rec
		}
	})
	t.Logf("IterRefs allocs/op: %.2f", avg)
	assert.LessOrEqualf(t, avg, 25.0,
		"IterRefs allocs/op: got %.0f, want <= 25", avg)
}

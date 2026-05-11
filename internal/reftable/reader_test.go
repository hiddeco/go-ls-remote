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
			switch rec.Name {
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
		assert.Empty(t, mainRecord.TargetRef, "main ref is a value record, not a symref")
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
			if rec.Name != "" && rec.Name != "HEAD" && rec.Name != "refs/heads/main" {
				branches[rec.Name] = true
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
					if rec.Name == "HEAD" {
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
		assert.Equal(t, "refs/heads/main", rec.Name)
		assert.False(t, rec.Value.IsZero(), "refs/heads/main must carry a non-zero OID")
	})

	t.Run("single_sha1_head_symref", func(t *testing.T) {
		r, err := OpenReader[objfmt.SHA1Hash](fixturePath(t, "single-sha1/0001-0001-aaaaaaaa.ref"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = r.Close() })

		rec, ok, err := r.FindRef("HEAD")
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, "HEAD", rec.Name)
		assert.Equal(t, "refs/heads/main", rec.TargetRef)
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
		assert.Equal(t, "refs/heads/branch-50", rec.Name)
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
		assert.Equal(t, "refs/heads/main", rec.Name)
		assert.False(t, rec.Value.IsZero())
	})
}

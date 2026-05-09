package reftable

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stackDir resolves a fixture stack directory under testdata/reftable.
func stackDir(t *testing.T, rel string) string {
	t.Helper()
	return filepath.Join(fixtureRoot(t), rel)
}

func TestOpenStack(t *testing.T) {
	t.Run("single_table_behaves_like_reader", func(t *testing.T) {
		stack, err := OpenStack(stackDir(t, "single-sha1"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = stack.Close() })

		rdr, err := OpenReader(fixturePath(t, "single-sha1/0001-0001-aaaaaaaa.ref"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = rdr.Close() })

		// Same set of records, same values.
		want := map[string]RefRecord{}
		for rec, err := range rdr.IterRefs() {
			require.NoError(t, err)
			want[rec.Name] = rec
		}
		got := map[string]RefRecord{}
		for rec, err := range stack.IterRefs() {
			require.NoError(t, err)
			got[rec.Name] = rec
		}
		assert.Equal(t, want, got)

		// FindRef matches reader for both a hit (refs/heads/main) and a
		// miss (a definitely-absent name).
		mainStack, ok, err := stack.FindRef("refs/heads/main")
		require.NoError(t, err)
		require.True(t, ok)
		mainRdr, ok2, err := rdr.FindRef("refs/heads/main")
		require.NoError(t, err)
		require.True(t, ok2)
		assert.Equal(t, mainRdr, mainStack)

		_, ok, err = stack.FindRef("refs/heads/does-not-exist")
		require.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("stack_shadow_sha1_table_2_shadows_table_1", func(t *testing.T) {
		stack, err := OpenStack(stackDir(t, "stack-shadow-sha1"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = stack.Close() })

		// The fixture's last reftable re-points refs/heads/main at the
		// second commit's OID. The merged view must surface that value,
		// not the earlier table's.
		lastReader, err := OpenReader(fixturePath(t, "stack-shadow-sha1/0003-0003-cccccccc.ref"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = lastReader.Close() })
		want, ok, err := lastReader.FindRef("refs/heads/main")
		require.NoError(t, err)
		require.True(t, ok)

		got, ok, err := stack.FindRef("refs/heads/main")
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, want, got, "stack must reflect the latest table's value")

		// Sanity: the earliest table either has no refs/heads/main or a
		// different value. Either way, the stacked view must NOT report
		// the earliest value when a later table shadows it.
		firstReader, err := OpenReader(fixturePath(t, "stack-shadow-sha1/0001-0001-aaaaaaaa.ref"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = firstReader.Close() })
		first, hadFirst, err := firstReader.FindRef("refs/heads/main")
		require.NoError(t, err)
		if hadFirst {
			assert.NotEqual(t, first.Value, got.Value, "stacked main must not equal earliest table's main")
		}
	})

	t.Run("empty_tables_list_yields_empty_stack", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "tables.list"), nil, 0o644))

		stack, err := OpenStack(dir)
		require.NoError(t, err)
		t.Cleanup(func() { _ = stack.Close() })

		var count int
		for _, err := range stack.IterRefs() {
			require.NoError(t, err)
			count++
		}
		assert.Zero(t, count)

		rec, ok, err := stack.FindRef("refs/heads/main")
		require.NoError(t, err)
		assert.False(t, ok)
		assert.Equal(t, RefRecord{}, rec)

		assert.Equal(t, objfmt.Algo(0), stack.HashAlgo())
	})

	t.Run("missing_tables_list_returns_error", func(t *testing.T) {
		dir := t.TempDir()
		_, err := OpenStack(dir)
		require.Error(t, err)
	})

	t.Run("trailing_newline_tolerated", func(t *testing.T) {
		// Build a temp directory that mirrors single-sha1 and contains
		// the canonical trailing-newline tables.list. The committed
		// fixture already exercises this, but a freshly written file
		// pins the contract explicitly.
		dir := t.TempDir()
		src, err := os.ReadFile(fixturePath(t, "single-sha1/0001-0001-aaaaaaaa.ref"))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "0001-0001-aaaaaaaa.ref"), src, 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "tables.list"), []byte("0001-0001-aaaaaaaa.ref\n"), 0o644))

		stack, err := OpenStack(dir)
		require.NoError(t, err)
		t.Cleanup(func() { _ = stack.Close() })

		rec, ok, err := stack.FindRef("refs/heads/main")
		require.NoError(t, err)
		require.True(t, ok)
		assert.False(t, rec.Value.IsZero())
	})

	t.Run("no_trailing_newline_tolerated", func(t *testing.T) {
		dir := t.TempDir()
		src, err := os.ReadFile(fixturePath(t, "single-sha1/0001-0001-aaaaaaaa.ref"))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "0001-0001-aaaaaaaa.ref"), src, 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "tables.list"), []byte("0001-0001-aaaaaaaa.ref"), 0o644))

		stack, err := OpenStack(dir)
		require.NoError(t, err)
		t.Cleanup(func() { _ = stack.Close() })

		_, ok, err := stack.FindRef("refs/heads/main")
		require.NoError(t, err)
		require.True(t, ok)
	})

	t.Run("empty_middle_line_rejected", func(t *testing.T) {
		dir := t.TempDir()
		src, err := os.ReadFile(fixturePath(t, "single-sha1/0001-0001-aaaaaaaa.ref"))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "0001-0001-aaaaaaaa.ref"), src, 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "tables.list"), []byte("0001-0001-aaaaaaaa.ref\n\n0001-0001-aaaaaaaa.ref\n"), 0o644))

		_, err = OpenStack(dir)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrInvalidTablesList), "want ErrInvalidTablesList, got %v", err)
	})

	t.Run("sha256_stack", func(t *testing.T) {
		stack, err := OpenStack(stackDir(t, "single-sha256"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = stack.Close() })

		assert.Equal(t, objfmt.SHA256, stack.HashAlgo())

		rec, ok, err := stack.FindRef("refs/heads/main")
		require.NoError(t, err)
		require.True(t, ok)
		assert.False(t, rec.Value.IsZero())
	})

	t.Run("mixed_hash_algos_rejected", func(t *testing.T) {
		dir := t.TempDir()

		// Copy one SHA-1 reftable and one SHA-256 reftable into the
		// same directory under unique basenames, then point
		// tables.list at both.
		sha1Bytes, err := os.ReadFile(fixturePath(t, "single-sha1/0001-0001-aaaaaaaa.ref"))
		require.NoError(t, err)
		sha256Bytes, err := os.ReadFile(fixturePath(t, "single-sha256/0001-0001-aaaaaaaa.ref"))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "0001-0001-aaaaaaaa.ref"), sha1Bytes, 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "0002-0002-bbbbbbbb.ref"), sha256Bytes, 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "tables.list"),
			[]byte("0001-0001-aaaaaaaa.ref\n0002-0002-bbbbbbbb.ref\n"), 0o644))

		_, err = OpenStack(dir)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrMixedHashAlgo), "want ErrMixedHashAlgo, got %v", err)
	})

	t.Run("missing_table_returns_error", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "tables.list"),
			[]byte("does-not-exist.ref\n"), 0o644))

		_, err := OpenStack(dir)
		require.Error(t, err)
	})
}

func TestStack_FindRef(t *testing.T) {
	t.Run("hit_and_miss", func(t *testing.T) {
		stack, err := OpenStack(stackDir(t, "single-sha1"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = stack.Close() })

		rec, ok, err := stack.FindRef("refs/heads/main")
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, "refs/heads/main", rec.Name)
		assert.False(t, rec.Value.IsZero())

		rec, ok, err = stack.FindRef("refs/heads/missing")
		require.NoError(t, err)
		assert.False(t, ok)
		assert.Equal(t, RefRecord{}, rec)
	})
}

func TestStack_IterRefs(t *testing.T) {
	t.Run("sorted_order", func(t *testing.T) {
		stack, err := OpenStack(stackDir(t, "with-index-sha1"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = stack.Close() })

		var names []string
		for rec, err := range stack.IterRefs() {
			require.NoError(t, err)
			names = append(names, rec.Name)
		}
		require.NotEmpty(t, names)
		assert.True(t, slices.IsSorted(names), "names must be sorted lexicographically: %v", names)
	})

	t.Run("shadow_stack_yields_latest_value", func(t *testing.T) {
		stack, err := OpenStack(stackDir(t, "stack-shadow-sha1"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = stack.Close() })

		// Pick out the merged refs/heads/main and confirm it equals the
		// last reader's record.
		var fromIter RefRecord
		for rec, err := range stack.IterRefs() {
			require.NoError(t, err)
			if rec.Name == "refs/heads/main" {
				fromIter = rec
			}
		}
		require.NotEmpty(t, fromIter.Name, "merged view must include refs/heads/main")

		lastReader, err := OpenReader(fixturePath(t, "stack-shadow-sha1/0003-0003-cccccccc.ref"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = lastReader.Close() })
		want, ok, err := lastReader.FindRef("refs/heads/main")
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, want, fromIter)
	})
}

func TestStack_Close(t *testing.T) {
	t.Run("idempotent", func(t *testing.T) {
		stack, err := OpenStack(stackDir(t, "single-sha1"))
		require.NoError(t, err)
		require.NoError(t, stack.Close())
		// A second Close must succeed.
		require.NoError(t, stack.Close())
	})

	t.Run("idempotent_on_empty_stack", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "tables.list"), nil, 0o644))
		stack, err := OpenStack(dir)
		require.NoError(t, err)
		require.NoError(t, stack.Close())
		require.NoError(t, stack.Close())
	})
}

package objfmt

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPack_openPackReader(t *testing.T) {
	t.Parallel()
	t.Run("opens a regular file and reads at offset", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "blob.bin")
		want := []byte("PACK\x00\x00\x00\x02hello world")
		require.NoError(t, os.WriteFile(path, want, 0o600))

		r, err := openPackReader(path)
		require.NoError(t, err)
		defer func() { _ = r.Close() }()

		// Satisfies the interface contract.
		var _ io.ReaderAt = r
		var _ io.Closer = r
		assert.Equal(t, int64(len(want)), r.Len())

		got := make([]byte, 4)
		n, err := r.ReadAt(got, 0)
		require.NoError(t, err)
		assert.Equal(t, 4, n)
		assert.Equal(t, []byte("PACK"), got)

		got = make([]byte, 5)
		n, err = r.ReadAt(got, 8)
		require.NoError(t, err)
		assert.Equal(t, 5, n)
		assert.Equal(t, []byte("hello"), got)
	})

	t.Run("returns an error for a missing path", func(t *testing.T) {
		t.Parallel()
		_, err := openPackReader(filepath.Join(t.TempDir(), "nope.pack"))
		assert.Error(t, err)
	})
}

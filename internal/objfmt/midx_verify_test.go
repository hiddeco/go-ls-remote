package objfmt

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMidx_VerifyChecksum(t *testing.T) {
	t.Parallel()
	t.Run("intact SHA-1 fixture verifies", func(t *testing.T) {
		t.Parallel()
		m, err := OpenMidx[SHA1Hash](idxFixture(t, "multi-pack-index"), SHA1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = m.Close() })
		assert.NoError(t, m.VerifyChecksum())
	})

	t.Run("intact SHA-256 fixture verifies", func(t *testing.T) {
		t.Parallel()
		m, err := OpenMidx[SHA256Hash](idxFixture(t, "sha256-multi-pack-index"), SHA256)
		require.NoError(t, err)
		t.Cleanup(func() { _ = m.Close() })
		assert.NoError(t, m.VerifyChecksum())
	})

	t.Run("a flipped byte fails verification", func(t *testing.T) {
		t.Parallel()
		// Copy the fixture so the on-disk original is untouched, then
		// flip a byte inside the trailing 20-byte SHA-1. Flipping the
		// trailer keeps the body parseable — `OpenMidx` still
		// validates the chunk table and OOFF pack-index range — so
		// the failure surfaces from `VerifyChecksum` rather than at
		// open time.
		dst := filepath.Join(t.TempDir(), "multi-pack-index")
		src, err := os.Open(idxFixture(t, "multi-pack-index"))
		require.NoError(t, err)
		w, err := os.Create(dst)
		require.NoError(t, err)
		_, err = io.Copy(w, src)
		require.NoError(t, err)
		require.NoError(t, src.Close())
		require.NoError(t, w.Close())

		f, err := os.OpenFile(dst, os.O_RDWR, 0)
		require.NoError(t, err)
		st, err := f.Stat()
		require.NoError(t, err)
		off := st.Size() - 5 // inside the SHA-1 trailer
		var b [1]byte
		_, err = f.ReadAt(b[:], off)
		require.NoError(t, err)
		b[0] ^= 0xff
		_, err = f.WriteAt(b[:], off)
		require.NoError(t, err)
		require.NoError(t, f.Close())

		m, err := OpenMidx[SHA1Hash](dst, SHA1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = m.Close() })
		assert.Error(t, m.VerifyChecksum())
	})

	t.Run("returns an error after Close", func(t *testing.T) {
		t.Parallel()
		m, err := OpenMidx[SHA1Hash](idxFixture(t, "multi-pack-index"), SHA1)
		require.NoError(t, err)
		require.NoError(t, m.Close())
		assert.Error(t, m.VerifyChecksum())
	})
}

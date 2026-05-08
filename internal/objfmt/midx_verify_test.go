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
	t.Run("intact SHA-1 fixture verifies", func(t *testing.T) {
		m, err := OpenMidx(idxFixture(t, "multi-pack-index"), SHA1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = m.Close() })
		assert.NoError(t, m.VerifyChecksum())
	})

	t.Run("intact SHA-256 fixture verifies", func(t *testing.T) {
		m, err := OpenMidx(idxFixture(t, "sha256-multi-pack-index"), SHA256)
		require.NoError(t, err)
		t.Cleanup(func() { _ = m.Close() })
		assert.NoError(t, m.VerifyChecksum())
	})

	t.Run("a flipped byte fails verification", func(t *testing.T) {
		// Copy the fixture so the on-disk original is untouched, then
		// flip a byte well inside the body — comfortably away from
		// the trailing 20-byte SHA-1.
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
		off := st.Size() - 60
		var b [1]byte
		_, err = f.ReadAt(b[:], off)
		require.NoError(t, err)
		b[0] ^= 0xff
		_, err = f.WriteAt(b[:], off)
		require.NoError(t, err)
		require.NoError(t, f.Close())

		m, err := OpenMidx(dst, SHA1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = m.Close() })
		assert.Error(t, m.VerifyChecksum())
	})

	t.Run("returns an error after Close", func(t *testing.T) {
		m, err := OpenMidx(idxFixture(t, "multi-pack-index"), SHA1)
		require.NoError(t, err)
		require.NoError(t, m.Close())
		assert.Error(t, m.VerifyChecksum())
	})
}

package objfmt

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// copyFixture clones a checked-in pack into a temp directory so the
// test can mutate it without touching the source artifact.
func copyFixture(t *testing.T, name string) string {
	t.Helper()
	src, err := os.Open(packFixture(t, name))
	require.NoError(t, err)
	defer func() { _ = src.Close() }()

	dst := filepath.Join(t.TempDir(), name)
	w, err := os.Create(dst)
	require.NoError(t, err)
	defer func() { _ = w.Close() }()

	_, err = io.Copy(w, src)
	require.NoError(t, err)
	return dst
}

func TestPack_VerifyChecksum(t *testing.T) {
	t.Parallel()
	t.Run("an intact three-object SHA-1 pack verifies", func(t *testing.T) {
		t.Parallel()
		p, err := OpenPack[SHA1Hash](packFixture(t, "three-objects.pack"), SHA1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = p.Close() })

		assert.NoError(t, p.VerifyChecksum())
	})

	t.Run("an intact OFS_DELTA pack verifies", func(t *testing.T) {
		t.Parallel()
		p, err := OpenPack[SHA1Hash](packFixture(t, "ofs-delta.pack"), SHA1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = p.Close() })

		assert.NoError(t, p.VerifyChecksum())
	})

	t.Run("an intact SHA-256 pack verifies", func(t *testing.T) {
		t.Parallel()
		p, err := OpenPack[SHA256Hash](packFixture(t, "sha256-empty.pack"), SHA256)
		require.NoError(t, err)
		t.Cleanup(func() { _ = p.Close() })

		assert.NoError(t, p.VerifyChecksum())
	})

	t.Run("an intact non-empty SHA-256 pack verifies", func(t *testing.T) {
		t.Parallel()
		p, err := OpenPack[SHA256Hash](packFixture(t, "sha256-three.pack"), SHA256)
		require.NoError(t, err)
		t.Cleanup(func() { _ = p.Close() })

		assert.NoError(t, p.VerifyChecksum())
	})

	t.Run("a flipped byte fails verification", func(t *testing.T) {
		t.Parallel()
		dst := copyFixture(t, "three-objects.pack")

		// Flip a byte in the middle of the pack body. The trailer
		// is the last 20 bytes, so anything earlier than that is
		// safe to corrupt without changing the trailer itself.
		f, err := os.OpenFile(dst, os.O_RDWR, 0)
		require.NoError(t, err)
		st, err := f.Stat()
		require.NoError(t, err)
		off := st.Size() - 30
		var b [1]byte
		_, err = f.ReadAt(b[:], off)
		require.NoError(t, err)
		b[0] ^= 0xff
		_, err = f.WriteAt(b[:], off)
		require.NoError(t, err)
		require.NoError(t, f.Close())

		p, err := OpenPack[SHA1Hash](dst, SHA1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = p.Close() })

		assert.Error(t, p.VerifyChecksum())
	})
}

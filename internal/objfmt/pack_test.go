package objfmt

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func packFixture(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join("..", "..", "testdata", "objfmt", name)
}

func writeBytes(t *testing.T, dir, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(p, b, 0o600))
	return p
}

// trailerPad returns n zero bytes — enough to satisfy [OpenPack]'s
// minimal-length expectation when the test only cares about the
// 12-byte header.
func trailerPad(n int) []byte { return make([]byte, n) }

func TestPack_OpenPack(t *testing.T) {
	t.Run("empty SHA-1 pack reports zero objects", func(t *testing.T) {
		p, err := OpenPack(packFixture(t, "empty.pack"), SHA1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = p.Close() })

		assert.Equal(t, SHA1, p.Algo())
		assert.Equal(t, uint32(2), p.Version())
		assert.Equal(t, uint32(0), p.Count())
	})

	t.Run("three-object SHA-1 pack reports three objects", func(t *testing.T) {
		p, err := OpenPack(packFixture(t, "three-objects.pack"), SHA1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = p.Close() })

		assert.Equal(t, SHA1, p.Algo())
		assert.Equal(t, uint32(2), p.Version())
		assert.Equal(t, uint32(3), p.Count())
	})

	t.Run("SHA-256 empty pack opens with the SHA256 algo", func(t *testing.T) {
		p, err := OpenPack(packFixture(t, "sha256-empty.pack"), SHA256)
		require.NoError(t, err)
		t.Cleanup(func() { _ = p.Close() })

		assert.Equal(t, SHA256, p.Algo())
		assert.Equal(t, uint32(0), p.Count())
	})

	t.Run("rejects a non-PACK magic", func(t *testing.T) {
		dir := t.TempDir()
		buf := append([]byte("JUNK"), make([]byte, 8)...)
		buf = append(buf, trailerPad(20)...)
		path := writeBytes(t, dir, "junk.pack", buf)

		_, err := OpenPack(path, SHA1)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not a pack")
	})

	t.Run("rejects pack version 1", func(t *testing.T) {
		dir := t.TempDir()
		hdr := make([]byte, 12)
		copy(hdr, "PACK")
		binary.BigEndian.PutUint32(hdr[4:8], 1)
		path := writeBytes(t, dir, "v1.pack", append(hdr, trailerPad(20)...))

		_, err := OpenPack(path, SHA1)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "version")
	})

	t.Run("rejects pack version 99", func(t *testing.T) {
		dir := t.TempDir()
		hdr := make([]byte, 12)
		copy(hdr, "PACK")
		binary.BigEndian.PutUint32(hdr[4:8], 99)
		path := writeBytes(t, dir, "v99.pack", append(hdr, trailerPad(20)...))

		_, err := OpenPack(path, SHA1)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "version")
	})

	t.Run("rejects an unknown algo", func(t *testing.T) {
		_, err := OpenPack(packFixture(t, "empty.pack"), Algo(0))
		require.Error(t, err)
	})

	t.Run("rejects a missing file", func(t *testing.T) {
		_, err := OpenPack(filepath.Join(t.TempDir(), "nope.pack"), SHA1)
		require.Error(t, err)
	})
}

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
	t.Parallel()
	t.Run("empty SHA-1 pack reports zero objects", func(t *testing.T) {
		t.Parallel()
		p, err := OpenPack[SHA1Hash](packFixture(t, "empty.pack"), SHA1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = p.Close() })

		assert.Equal(t, SHA1, p.Algo())
		assert.Equal(t, uint32(2), p.Version())
		assert.Equal(t, uint32(0), p.Count())
	})

	t.Run("three-object SHA-1 pack reports three objects", func(t *testing.T) {
		t.Parallel()
		p, err := OpenPack[SHA1Hash](packFixture(t, "three-objects.pack"), SHA1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = p.Close() })

		assert.Equal(t, SHA1, p.Algo())
		assert.Equal(t, uint32(2), p.Version())
		assert.Equal(t, uint32(3), p.Count())
	})

	t.Run("SHA-256 empty pack opens with the SHA256 algo", func(t *testing.T) {
		t.Parallel()
		p, err := OpenPack[SHA256Hash](packFixture(t, "sha256-empty.pack"), SHA256)
		require.NoError(t, err)
		t.Cleanup(func() { _ = p.Close() })

		assert.Equal(t, SHA256, p.Algo())
		assert.Equal(t, uint32(0), p.Count())
	})

	t.Run("three-object SHA-256 pack reports three objects", func(t *testing.T) {
		t.Parallel()
		p, err := OpenPack[SHA256Hash](packFixture(t, "sha256-three.pack"), SHA256)
		require.NoError(t, err)
		t.Cleanup(func() { _ = p.Close() })

		assert.Equal(t, SHA256, p.Algo())
		assert.Equal(t, uint32(2), p.Version())
		assert.Equal(t, uint32(3), p.Count())
	})

	t.Run("accepts pack version 3", func(t *testing.T) {
		t.Parallel()
		// Version 3 is reserved by [gitformat-pack.adoc] but never
		// emitted by canonical Git; OpenPack must still accept it
		// since the doc declares it valid. Construct a minimal
		// zero-object v3 pack inline rather than checking in a
		// fixture canonical Git would not produce.
		//
		// [gitformat-pack.adoc]: https://github.com/git/git/blob/v2.54.0/Documentation/gitformat-pack.adoc
		dir := t.TempDir()
		hdr := make([]byte, 12, 12+20)
		copy(hdr, "PACK")
		binary.BigEndian.PutUint32(hdr[4:8], 3)
		binary.BigEndian.PutUint32(hdr[8:12], 0)
		path := writeBytes(t, dir, "v3.pack", append(hdr, trailerPad(20)...))

		p, err := OpenPack[SHA1Hash](path, SHA1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = p.Close() })

		assert.Equal(t, uint32(3), p.Version())
		assert.Equal(t, uint32(0), p.Count())
	})

	t.Run("rejects a non-PACK magic", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		buf := append([]byte("JUNK"), make([]byte, 8)...)
		buf = append(buf, trailerPad(20)...)
		path := writeBytes(t, dir, "junk.pack", buf)

		_, err := OpenPack[SHA1Hash](path, SHA1)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not a pack")
	})

	t.Run("rejects pack version 1", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		hdr := make([]byte, 12, 12+20)
		copy(hdr, "PACK")
		binary.BigEndian.PutUint32(hdr[4:8], 1)
		path := writeBytes(t, dir, "v1.pack", append(hdr, trailerPad(20)...))

		_, err := OpenPack[SHA1Hash](path, SHA1)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "version")
	})

	t.Run("rejects pack version 99", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		hdr := make([]byte, 12, 12+20)
		copy(hdr, "PACK")
		binary.BigEndian.PutUint32(hdr[4:8], 99)
		path := writeBytes(t, dir, "v99.pack", append(hdr, trailerPad(20)...))

		_, err := OpenPack[SHA1Hash](path, SHA1)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "version")
	})

	t.Run("rejects a nil algo", func(t *testing.T) {
		t.Parallel()
		_, err := OpenPack[SHA1Hash](packFixture(t, "empty.pack"), nil)
		require.Error(t, err)
	})

	t.Run("rejects a missing file", func(t *testing.T) {
		t.Parallel()
		_, err := OpenPack[SHA1Hash](filepath.Join(t.TempDir(), "nope.pack"), SHA1)
		require.Error(t, err)
	})
}

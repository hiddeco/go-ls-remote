package objfmt

import (
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSentinelErrors(t *testing.T) {
	t.Parallel()
	t.Run("OpenPack on a non-PACK file wraps ErrBadMagic", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		buf := append([]byte("JUNK"), make([]byte, 8)...)
		buf = append(buf, trailerPad(20)...)
		path := writeBytes(t, dir, "junk.pack", buf)

		_, err := OpenPack[SHA1Hash](path, SHA1)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrBadMagic)
	})

	t.Run("OpenPack on a too-short file wraps ErrShortFile", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := writeBytes(t, dir, "tiny.pack", []byte{0x00, 0x01, 0x02})

		_, err := OpenPack[SHA1Hash](path, SHA1)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrShortFile)
	})

	t.Run("OpenPack on an unsupported version wraps ErrUnsupportedVersion", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		hdr := make([]byte, 12, 12+20)
		copy(hdr, "PACK")
		binary.BigEndian.PutUint32(hdr[4:8], 99)
		path := writeBytes(t, dir, "v99.pack", append(hdr, trailerPad(20)...))

		_, err := OpenPack[SHA1Hash](path, SHA1)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrUnsupportedVersion)
	})

	t.Run("OpenPack with a nil algo wraps ErrUnsupportedAlgo", func(t *testing.T) {
		t.Parallel()
		_, err := OpenPack[SHA1Hash](packFixture(t, "empty.pack"), nil)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrUnsupportedAlgo)
	})

	t.Run("OpenIdx on truncated input wraps ErrTruncated", func(t *testing.T) {
		t.Parallel()
		// A two-byte file is short enough to slip past the v2-magic
		// check (which requires 4 bytes) and lands in the v1
		// fan-out-truncation branch.
		path := filepath.Join(t.TempDir(), "tiny.idx")
		require.NoError(t, os.WriteFile(path, []byte{0xff, 't'}, 0o600))

		_, err := OpenIdx[SHA1Hash](path, SHA1)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrTruncated)
	})

	t.Run("OpenIdx on an unsupported v2 version wraps ErrUnsupportedVersion", func(t *testing.T) {
		t.Parallel()
		buf := make([]byte, 0, 8+256*4+20+20)
		buf = append(buf, 0xff, 't', 'O', 'c', 0, 0, 0, 99)
		buf = append(buf, make([]byte, 256*4+20+20)...)
		path := filepath.Join(t.TempDir(), "v99.idx")
		require.NoError(t, os.WriteFile(path, buf, 0o600))

		_, err := OpenIdx[SHA1Hash](path, SHA1)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrUnsupportedVersion)
	})

	t.Run("OpenMidx on a non-MIDX file wraps ErrBadMagic", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		buf := make([]byte, midxHeaderSize+20)
		copy(buf, "JUNK")
		path := writeBytes(t, dir, "junk.midx", buf)

		_, err := OpenMidx[SHA1Hash](path, SHA1)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrBadMagic)
	})

	t.Run("OpenMidx on a hash-version mismatch wraps ErrAlgoMismatch", func(t *testing.T) {
		t.Parallel()
		// Read the SHA-1 fixture and lie about the hash version so the
		// caller-asserted [SHA256] disagrees with the on-disk byte.
		data, err := os.ReadFile(idxFixture(t, "multi-pack-index"))
		require.NoError(t, err)
		path := filepath.Join(t.TempDir(), "multi-pack-index")
		require.NoError(t, os.WriteFile(path, data, 0o600))

		_, err = OpenMidx[SHA256Hash](path, SHA256)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrAlgoMismatch)
	})

	t.Run("Pack.VerifyChecksum on a flipped byte wraps ErrChecksumMismatch", func(t *testing.T) {
		t.Parallel()
		dst := copyFixture(t, "three-objects.pack")

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

		err = p.VerifyChecksum()
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrChecksumMismatch)
	})

	t.Run("Idx.VerifyChecksum on a flipped byte wraps ErrChecksumMismatch", func(t *testing.T) {
		t.Parallel()
		dst := filepath.Join(t.TempDir(), "three-objects.idx")
		src, err := os.Open(idxFixture(t, "three-objects.idx"))
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

		idx, err := OpenIdx[SHA1Hash](dst, SHA1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = idx.Close() })

		err = idx.VerifyChecksum()
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrChecksumMismatch)
	})

	t.Run("sentinels are distinct", func(t *testing.T) {
		t.Parallel()
		// Sanity-check that wrapping doesn't accidentally collapse two
		// sentinels into the same value.
		require.NotErrorIs(t, ErrBadMagic, ErrTruncated)
		require.NotErrorIs(t, ErrTruncated, ErrCorrupt)
		assert.NotErrorIs(t, ErrCorrupt, ErrChecksumMismatch)
	})
}

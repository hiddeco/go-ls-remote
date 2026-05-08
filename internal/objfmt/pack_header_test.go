package objfmt

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeMinPack writes a one-object pack to a temporary file: the
// 12-byte `PACK\x00\x00\x00\x02 + nrObjects=1` header, then `body`,
// then a 20-byte zero trailer. The trailer is not a real SHA-1 — these
// helpers exist for [Pack.ReadHeader] tests that never call
// [Pack.VerifyChecksum] — but it satisfies [OpenPack]'s minimum-length
// check.
func writeMinPack(t *testing.T, body []byte) string {
	t.Helper()
	hdr := make([]byte, 12)
	copy(hdr, "PACK")
	binary.BigEndian.PutUint32(hdr[4:8], 2)
	binary.BigEndian.PutUint32(hdr[8:12], 1)
	buf := append(hdr, body...)
	buf = append(buf, trailerPad(20)...)
	return writeBytes(t, t.TempDir(), "synthetic.pack", buf)
}

func TestPack_ReadHeader(t *testing.T) {
	t.Run("decodes a single-byte non-delta header", func(t *testing.T) {
		p, err := OpenPack(packFixture(t, "three-objects.pack"), SHA1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = p.Close() })

		// blob "hello fixture\n" at offset 179, size 14, single
		// header byte 0x3e (type=blob, size=14).
		hdr, err := p.ReadHeader(179)
		require.NoError(t, err)
		assert.Equal(t, TypeBlob, hdr.Type)
		assert.Equal(t, int64(14), hdr.Size)
		assert.Equal(t, int64(180), hdr.BodyAt)
		assert.Equal(t, int64(0), hdr.DeltaRef.OfsBase)
		assert.True(t, hdr.DeltaRef.RefBase.IsZero())
	})

	t.Run("decodes a multi-byte non-delta header", func(t *testing.T) {
		p, err := OpenPack(packFixture(t, "three-objects.pack"), SHA1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = p.Close() })

		// commit at offset 12, size 178, header bytes 0x92 0x0b.
		hdr, err := p.ReadHeader(12)
		require.NoError(t, err)
		assert.Equal(t, TypeCommit, hdr.Type)
		assert.Equal(t, int64(178), hdr.Size)
		assert.Equal(t, int64(14), hdr.BodyAt)

		// tree at offset 131, size 37, header bytes 0xa5 0x02.
		hdr, err = p.ReadHeader(131)
		require.NoError(t, err)
		assert.Equal(t, TypeTree, hdr.Type)
		assert.Equal(t, int64(37), hdr.Size)
		assert.Equal(t, int64(133), hdr.BodyAt)
	})

	t.Run("decodes an OFS_DELTA header", func(t *testing.T) {
		p, err := OpenPack(packFixture(t, "ofs-delta.pack"), SHA1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = p.Close() })

		// Delta at offset 207. Header byte 0x69 -> type=ofs-delta,
		// size=9. OFS varint 0x80 0x43 -> 195. Base = 207 - 195 = 12.
		hdr, err := p.ReadHeader(207)
		require.NoError(t, err)
		assert.Equal(t, TypeOfsDelta, hdr.Type)
		assert.Equal(t, int64(9), hdr.Size)
		assert.Equal(t, int64(12), hdr.DeltaRef.OfsBase)
		assert.Equal(t, int64(210), hdr.BodyAt)
		assert.True(t, hdr.DeltaRef.RefBase.IsZero())
	})

	t.Run("decodes a REF_DELTA header", func(t *testing.T) {
		p, err := OpenPack(packFixture(t, "ref-delta.pack"), SHA1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = p.Close() })

		// Delta at offset 207. Header byte 0x79 -> type=ref-delta,
		// size=9. Then 20 raw bytes of base hash.
		hdr, err := p.ReadHeader(207)
		require.NoError(t, err)
		assert.Equal(t, TypeRefDelta, hdr.Type)
		assert.Equal(t, int64(9), hdr.Size)
		assert.Equal(t, int64(228), hdr.BodyAt)
		assert.Equal(t, int64(0), hdr.DeltaRef.OfsBase)
		want, err := ParseHex("87bab3f4f5c79ca006911993eaec265a51c49a8b", SHA1)
		require.NoError(t, err)
		assert.Equal(t, want, hdr.DeltaRef.RefBase)
	})

	t.Run("rejects offsets past EOF", func(t *testing.T) {
		p, err := OpenPack(packFixture(t, "three-objects.pack"), SHA1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = p.Close() })

		_, err = p.ReadHeader(int64(p.r.Len()) + 16)
		assert.Error(t, err)
	})

	t.Run("rejects an OFS_DELTA whose base lands at or past the delta", func(t *testing.T) {
		// Hand-crafted single-object pack with an OFS_DELTA at offset
		// 12. The first header byte 0x60 encodes type=6 (OFS_DELTA),
		// size=0, no continuation. The OFS varint 0x0c encodes the
		// offset 12 with no continuation, so `OfsBase = 12 - 12 = 0`
		// and `get_delta_base`'s `base_offset <= 0` guard
		// (`packfile.c::1290`) fires.
		path := writeMinPack(t, []byte{0x60, 0x0c})

		p, err := OpenPack(path, SHA1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = p.Close() })

		_, err = p.ReadHeader(12)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrCorrupt)
		assert.Contains(t, err.Error(), "OFS_DELTA base offset out of range")
	})
}

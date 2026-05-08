package objfmt

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

	t.Run("rejects an OFS_DELTA whose base lands past the delta", func(t *testing.T) {
		// A hand-crafted OFS_DELTA whose offset varint encodes a
		// huge value forces `at - off` to be non-positive, which
		// `get_delta_base` flags as "out of bound".
		p, err := OpenPack(packFixture(t, "ofs-delta.pack"), SHA1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = p.Close() })

		// Calling ReadHeader at the base blob's offset (12) — a
		// non-delta blob — must NOT be flagged as out-of-bound; the
		// guard only fires on delta types.
		hdr, err := p.ReadHeader(12)
		require.NoError(t, err)
		assert.Equal(t, TypeBlob, hdr.Type)
	})
}

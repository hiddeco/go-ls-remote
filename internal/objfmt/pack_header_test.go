package objfmt

import (
	"encoding/binary"
	"sync"
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

	t.Run("decodes headers from a SHA-256 pack", func(t *testing.T) {
		// Pack is SHA-256 so `algo.Size() == 32`; the peek size and
		// any REF_DELTA stride change with the algo, but every
		// non-delta header decodes the same way.
		p, err := OpenPack(packFixture(t, "sha256-three.pack"), SHA256)
		require.NoError(t, err)
		t.Cleanup(func() { _ = p.Close() })

		// Per `sha256-three.offsets.txt`: commit at offset 12 size
		// 202, tree at 144 size 49, blob at 206 size 13.
		hdr, err := p.ReadHeader(12)
		require.NoError(t, err)
		assert.Equal(t, TypeCommit, hdr.Type)
		assert.Equal(t, int64(202), hdr.Size)

		hdr, err = p.ReadHeader(144)
		require.NoError(t, err)
		assert.Equal(t, TypeTree, hdr.Type)
		assert.Equal(t, int64(49), hdr.Size)

		hdr, err = p.ReadHeader(206)
		require.NoError(t, err)
		assert.Equal(t, TypeBlob, hdr.Type)
		assert.Equal(t, int64(13), hdr.Size)
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

		_, err = p.ReadHeader(p.r.Len() + 16)
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

	t.Run("rejects a pack header that overruns the peek buffer", func(t *testing.T) {
		// Bytes 27..31 of the file are read as the type/size header
		// when [Pack.ReadHeader] is called with `at=27`, since the
		// peek slice is clamped by `r.Len() - at = 5`. All five bytes
		// have the continuation bit set, so the size loop runs out of
		// buffer before it can hit the `shift >= 64` overflow guard.
		buf := make([]byte, 32)
		copy(buf, "PACK")
		binary.BigEndian.PutUint32(buf[4:8], 2)
		binary.BigEndian.PutUint32(buf[8:12], 1)
		for i := 27; i < 32; i++ {
			buf[i] = 0xFF
		}
		path := writeBytes(t, t.TempDir(), "overrun.pack", buf)

		p, err := OpenPack(path, SHA1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = p.Close() })

		_, err = p.ReadHeader(27)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrTruncated)
		assert.Contains(t, err.Error(), "pack header overruns buffer")
	})

	t.Run("rejects a pack header whose size shift overflows", func(t *testing.T) {
		// Ten consecutive 0xFF bytes at offset 12: every iteration of
		// the size loop bumps `shift` by 7, so after the tenth byte
		// `shift = 4 + 9*7 = 67 >= 64` and the overflow guard fires.
		body := make([]byte, 10)
		for i := range body {
			body[i] = 0xFF
		}
		path := writeMinPack(t, body)

		p, err := OpenPack(path, SHA1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = p.Close() })

		_, err = p.ReadHeader(12)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrCorrupt)
		assert.Contains(t, err.Error(), "pack header size overflow")
	})

	t.Run("rejects an OFS_DELTA offset that overruns the buffer", func(t *testing.T) {
		// Read at offset 26 in a 32-byte file: the peek slice is six
		// bytes. The first byte is type=6 (OFS_DELTA, 0x60); the
		// remaining five bytes are 0xFF — every byte has the
		// continuation bit set so `readOfsBase` runs out of buffer
		// before the offset reaches the `next < off` overflow guard
		// (which needs roughly eight iterations to wrap).
		buf := make([]byte, 32)
		copy(buf, "PACK")
		binary.BigEndian.PutUint32(buf[4:8], 2)
		binary.BigEndian.PutUint32(buf[8:12], 1)
		buf[26] = 0x60
		for i := 27; i < 32; i++ {
			buf[i] = 0xFF
		}
		path := writeBytes(t, t.TempDir(), "ofs-overrun.pack", buf)

		p, err := OpenPack(path, SHA1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = p.Close() })

		_, err = p.ReadHeader(26)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrTruncated)
		assert.Contains(t, err.Error(), "OFS_DELTA offset overruns buffer")
	})

	t.Run("rejects an OFS_DELTA offset varint that overflows", func(t *testing.T) {
		// At offset 12 the peek slice is large enough to hold a full
		// nine-byte continuation chain. The accumulated offset grows
		// by ~7 bits per byte, so the eighth iteration produces an
		// `off` of roughly `1 << 56`, and the next iteration's
		// pre-shift guard (`off >= 1 << 56`) fires before the shift
		// can wrap into the sign bit.
		body := make([]byte, 10)
		body[0] = 0x60 // type=OFS_DELTA, size=0
		for i := 1; i < len(body); i++ {
			body[i] = 0xFF
		}
		path := writeMinPack(t, body)

		p, err := OpenPack(path, SHA1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = p.Close() })

		_, err = p.ReadHeader(12)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrCorrupt)
		assert.Contains(t, err.Error(), "OFS_DELTA offset")
	})

	t.Run("rejects a REF_DELTA header that overruns the buffer", func(t *testing.T) {
		// Read the type/size header at offset 15 of a 32-byte file:
		// the peek slice is 17 bytes. A single 0x70 type byte
		// (REF_DELTA, size=0) consumes one byte, leaving 16 — short
		// of the 20-byte SHA-1 base hash, so the REF_DELTA-specific
		// overrun check fires.
		buf := make([]byte, 32)
		copy(buf, "PACK")
		binary.BigEndian.PutUint32(buf[4:8], 2)
		binary.BigEndian.PutUint32(buf[8:12], 1)
		buf[15] = 0x70
		path := writeBytes(t, t.TempDir(), "ref-overrun.pack", buf)

		p, err := OpenPack(path, SHA1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = p.Close() })

		_, err = p.ReadHeader(15)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrTruncated)
		assert.Contains(t, err.Error(), "REF_DELTA base hash overruns buffer")
	})

	t.Run("rejects reserved object type 5", func(t *testing.T) {
		// Header byte 0x50 encodes type bits 0b101 = 5, the reserved
		// type that canonical Git rejects in
		// `packfile.c::unpack_object_header_buffer`.
		path := writeMinPack(t, []byte{0x50})

		p, err := OpenPack(path, SHA1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = p.Close() })

		_, err = p.ReadHeader(12)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrCorrupt)
		assert.Contains(t, err.Error(), "unknown pack object type 5")
	})

	t.Run("safe under concurrent calls on the same Pack", func(t *testing.T) {
		// `Pack` is documented as safe for concurrent reads. The
		// per-call peek scratch comes from a `sync.Pool`, so this
		// test pins the contract: many goroutines hammering
		// `ReadHeader` on a shared `Pack` must each see the
		// expected header without buffer aliasing.
		p, err := OpenPack(packFixture(t, "three-objects.pack"), SHA1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = p.Close() })

		const goroutines = 32
		const iterations = 256
		var wg sync.WaitGroup
		wg.Add(goroutines)
		for range goroutines {
			go func() {
				defer wg.Done()
				for range iterations {
					hdr, err := p.ReadHeader(179)
					require.NoError(t, err)
					require.Equal(t, TypeBlob, hdr.Type)
					require.Equal(t, int64(14), hdr.Size)
				}
			}()
		}
		wg.Wait()
	})
}

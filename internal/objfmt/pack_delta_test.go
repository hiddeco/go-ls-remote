package objfmt

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPack_ReadDeltaHeader(t *testing.T) {
	t.Run("decodes the source and target sizes of an OFS_DELTA", func(t *testing.T) {
		p, err := OpenPack(packFixture(t, "ofs-delta.pack"), SHA1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = p.Close() })

		hdr, err := p.ReadHeader(207)
		require.NoError(t, err)
		require.Equal(t, TypeOfsDelta, hdr.Type)

		// Base blob (`87bab3f4...`) is 17605 bytes; the deltified
		// blob (`3dc05f9f...`) is 17600 bytes — see the
		// `ofs-delta.offsets.txt` sidecar.
		src, tgt, err := p.ReadDeltaHeader(hdr.BodyAt)
		require.NoError(t, err)
		assert.Equal(t, int64(17605), src)
		assert.Equal(t, int64(17600), tgt)
	})

	t.Run("decodes the source and target sizes of a REF_DELTA", func(t *testing.T) {
		p, err := OpenPack(packFixture(t, "ref-delta.pack"), SHA1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = p.Close() })

		hdr, err := p.ReadHeader(207)
		require.NoError(t, err)
		require.Equal(t, TypeRefDelta, hdr.Type)

		src, tgt, err := p.ReadDeltaHeader(hdr.BodyAt)
		require.NoError(t, err)
		assert.Equal(t, int64(17605), src)
		assert.Equal(t, int64(17600), tgt)
	})

	t.Run("rejects a corrupt zlib stream", func(t *testing.T) {
		p, err := OpenPack(packFixture(t, "ofs-delta.pack"), SHA1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = p.Close() })

		// Far-past-EOF offset: zlib initialisation will fail.
		_, _, err = p.ReadDeltaHeader(p.r.Len() - 4)
		assert.Error(t, err)
	})
}

// TestPack_ReadDeltaHeader_AllocsAfterWarmup pins the post-warmup
// allocation budget for repeated calls. The first call instantiates
// the zlib reader (cold path); the warmup phase puts that reader into
// the pool and the steady state reuses it via [zlib.Resetter.Reset],
// so per-call allocations should be a small constant (the
// function-local 64-byte peek buffer plus the [io.SectionReader]
// value).
func TestPack_ReadDeltaHeader_AllocsAfterWarmup(t *testing.T) {
	p, err := OpenPack(packFixture(t, "ofs-delta.pack"), SHA1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close() })
	const bodyAt = int64(210)

	// Warm the pool: a single call moves a `*zlib.Reader` through
	// New -> Put. Subsequent calls take the pooled entry via
	// Get -> Reset.
	if _, _, err := p.ReadDeltaHeader(bodyAt); err != nil {
		t.Fatal(err)
	}

	avg := testing.AllocsPerRun(100, func() {
		if _, _, err := p.ReadDeltaHeader(bodyAt); err != nil {
			t.Fatal(err)
		}
	})

	// Budget covers the function-local 64-byte peek buffer, the
	// [io.SectionReader], and the small handful of slices the
	// `compress/flate` reset path still allocates internally
	// (Huffman scratch, history window mux). The pre-pool baseline
	// was 10 allocs/op; the budget here is set to fail loudly on a
	// regression to the unpooled cold path while leaving headroom
	// for stdlib churn in the reset-internal allocations.
	const maxAllocs = 5
	if avg > maxAllocs {
		t.Fatalf("post-warmup allocs/op = %.1f, want <= %d (regression)",
			avg, maxAllocs)
	}
}

func TestPack_readDeltaVarint(t *testing.T) {
	t.Run("decodes a single-byte varint", func(t *testing.T) {
		v, n, err := readDeltaVarint([]byte{0x09})
		require.NoError(t, err)
		assert.Equal(t, int64(9), v)
		assert.Equal(t, 1, n)
	})

	t.Run("decodes the OFS_DELTA fixture's source size", func(t *testing.T) {
		// 0xc5 0x89 0x01 -> 0x45 | (0x09 << 7) | (0x01 << 14)
		v, n, err := readDeltaVarint([]byte{0xc5, 0x89, 0x01})
		require.NoError(t, err)
		assert.Equal(t, int64(17605), v)
		assert.Equal(t, 3, n)
	})

	t.Run("rejects a truncated varint", func(t *testing.T) {
		_, _, err := readDeltaVarint([]byte{0x80, 0x80})
		assert.Error(t, err)
	})

	t.Run("rejects an empty buffer", func(t *testing.T) {
		_, _, err := readDeltaVarint(nil)
		assert.Error(t, err)
	})
}

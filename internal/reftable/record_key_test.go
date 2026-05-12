package reftable

import (
	"errors"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// encodeVarint encodes v in the reftable varint format. It is the
// inverse of [decodeVarint] and only used by tests to hand-build
// fixture bytes. Mirrors [reftable/record.c::put_var_int].
//
// [reftable/record.c::put_var_int]: https://github.com/git/git/blob/v2.54.0/reftable/record.c#L56
func encodeVarint(v uint64) []byte {
	var buf [varintMaxBytes]byte
	pos := len(buf) - 1
	buf[pos] = byte(v & 0x7f)
	v >>= 7
	for v > 0 {
		pos--
		v--
		buf[pos] = 0x80 | byte(v&0x7f)
		v >>= 7
	}
	return append([]byte(nil), buf[pos:]...)
}

// encodeKey produces the on-disk bytes for a single prefix-compressed
// record key, given the prior key (for prefix sharing) and the 3-bit
// extra field. This is the inverse of [decodeKey].
func encodeKey(prevKey, key []byte, extra uint8) []byte {
	prefixLen := 0
	for prefixLen < len(prevKey) && prefixLen < len(key) && prevKey[prefixLen] == key[prefixLen] {
		prefixLen++
	}
	suffixLen := len(key) - prefixLen
	out := encodeVarint(uint64(prefixLen))
	out = append(out, encodeVarint(uint64(suffixLen)<<3|uint64(extra&0x7))...)
	out = append(out, key[prefixLen:]...)
	return out
}

func Test_decodeKey(t *testing.T) {
	t.Run("first_record_no_prefix", func(t *testing.T) {
		// Restart-point records always carry prefix_length=0, so the
		// decoded key equals the suffix.
		raw := encodeKey(nil, []byte("refs/heads/main"), 1)
		key, extra, n, err := decodeKey(raw, nil, nil)
		require.NoError(t, err)
		assert.Equal(t, []byte("refs/heads/main"), key)
		assert.Equal(t, uint8(1), extra)
		assert.Equal(t, len(raw), n)
	})

	t.Run("second_record_shares_prefix", func(t *testing.T) {
		prev := []byte("refs/heads/main")
		raw := encodeKey(prev, []byte("refs/heads/master"), 2)
		key, extra, n, err := decodeKey(raw, prev, nil)
		require.NoError(t, err)
		assert.Equal(t, []byte("refs/heads/master"), key)
		assert.Equal(t, uint8(2), extra)
		assert.Equal(t, len(raw), n)
	})

	t.Run("three_step_chain", func(t *testing.T) {
		// Walks a small chain to confirm the running prevKey works.
		keys := []string{
			"refs/heads/branch-1",
			"refs/heads/branch-12",
			"refs/heads/branch-2",
			"refs/heads/main",
		}
		extras := []uint8{1, 1, 1, 1}
		var raw []byte
		offsets := []int{}
		var prev []byte
		for i, k := range keys {
			offsets = append(offsets, len(raw))
			raw = append(raw, encodeKey(prev, []byte(k), extras[i])...)
			prev = []byte(k)
		}

		var got []byte
		for i, k := range keys {
			decoded, ex, n, err := decodeKey(raw[offsets[i]:], got, nil)
			require.NoError(t, err)
			assert.Equal(t, []byte(k), decoded)
			assert.Equal(t, extras[i], ex)
			// The next record begins exactly n bytes later in raw.
			if i+1 < len(offsets) {
				assert.Equal(t, offsets[i+1]-offsets[i], n)
			}
			got = decoded
		}
	})

	t.Run("extra_field_three_bits", func(t *testing.T) {
		// All eight extra values round-trip through encode/decode.
		for ex := range uint8(8) {
			raw := encodeKey(nil, []byte("HEAD"), ex)
			key, gotEx, _, err := decodeKey(raw, nil, nil)
			require.NoError(t, err)
			assert.Equal(t, []byte("HEAD"), key)
			assert.Equal(t, ex, gotEx)
		}
	})

	t.Run("truncated_suffix_rejected", func(t *testing.T) {
		// Encoded key claims a 4-byte suffix but only supplies 2.
		raw := encodeKey(nil, []byte("HEAD"), 1)
		short := raw[:len(raw)-2]
		_, _, _, err := decodeKey(short, nil, nil)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrTruncatedRecord), "want ErrTruncatedRecord, got %v", err)
	})

	t.Run("prefix_exceeds_prev_rejected", func(t *testing.T) {
		// Hand-craft a record that claims to share 20 bytes with a
		// 5-byte previous key. Reading must reject the input.
		raw := []byte{}
		raw = append(raw, encodeVarint(20)...)     // prefix_length
		raw = append(raw, encodeVarint(0<<3|1)...) // suffix_length=0, extra=1
		_, _, _, err := decodeKey(raw, []byte("HEAD"), nil)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrTruncatedRecord), "want ErrTruncatedRecord, got %v", err)
	})

	t.Run("reuses_scratch_when_cap_sufficient", func(t *testing.T) {
		// When the caller hands in a scratch slice whose cap is at
		// least the decoded key's length, decodeKey reuses the
		// underlying array. Pointer-identity (via [unsafe.SliceData])
		// is the direct shape of that contract: both slices index into
		// the same backing storage.
		raw := encodeKey(nil, []byte("refs/heads/main"), 1)
		scratch := make([]byte, 0, 64)
		key, _, _, err := decodeKey(raw, nil, scratch)
		require.NoError(t, err)
		assert.Equal(t, []byte("refs/heads/main"), key)
		assert.Same(t, unsafe.SliceData(key), unsafe.SliceData(scratch),
			"scratch and key must share the backing array")
	})

	t.Run("allocates_when_scratch_cap_insufficient", func(t *testing.T) {
		// With nil scratch (or scratch too small), decodeKey allocates
		// fresh, leaving any caller-supplied bytes untouched.
		raw := encodeKey(nil, []byte("refs/heads/main"), 1)
		scratch := []byte{} // cap=0
		key, _, _, err := decodeKey(raw, nil, scratch)
		require.NoError(t, err)
		assert.Equal(t, []byte("refs/heads/main"), key)
		assert.Empty(t, scratch, "scratch must be untouched when too small")
	})
}

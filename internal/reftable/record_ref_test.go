package reftable

import (
	"errors"
	"testing"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// encodeRefRecord builds the on-disk bytes for a single ref_record. It
// is the inverse of [decodeRefRecord] and exists only to hand-build
// fixture bytes in this test file. Callers supply the running prevKey
// for prefix compression; for the first record in a block (or any
// restart-point record) prevKey is nil and prefix_length encodes as 0.
//
// value, peeled, and target are interpreted per valueType:
//
//   - 0: deletion — value/peeled/target ignored.
//   - 1: single OID — value[:hashSize] is written.
//   - 2: two OIDs — value[:hashSize] then peeled[:hashSize].
//   - 3: symref — varint(len(target)) || target.
//
// The function panics on misuse (e.g. value too short for hashSize) so
// fixture bugs surface as test failures rather than malformed bytes.
func encodeRefRecord(prevKey []byte, name string, valueType uint8, updateIndexDelta uint64, value, peeled []byte, target string, hashSize int) []byte {
	out := encodeKey(prevKey, []byte(name), valueType)
	out = append(out, encodeVarint(updateIndexDelta)...)
	switch valueType {
	case 0:
		// deletion — no value bytes follow.
	case 1:
		if len(value) < hashSize {
			panic("encodeRefRecord: value shorter than hashSize")
		}
		out = append(out, value[:hashSize]...)
	case 2:
		if len(value) < hashSize || len(peeled) < hashSize {
			panic("encodeRefRecord: value/peeled shorter than hashSize")
		}
		out = append(out, value[:hashSize]...)
		out = append(out, peeled[:hashSize]...)
	case 3:
		out = append(out, encodeVarint(uint64(len(target)))...)
		out = append(out, []byte(target)...)
	default:
		panic("encodeRefRecord: unsupported valueType")
	}
	return out
}

// hashFromBytes left-pads (for SHA-1, hashSize=20) or fills (for
// SHA-256, hashSize=32) the running hash with the given bytes.
// Mirrors the convention in [objfmt.Hash]: the SHA-1 id sits in the
// low 20 bytes, leaving the high 12 bytes zero.
func hashFromBytes(b []byte, hashSize int) objfmt.Hash {
	var h objfmt.Hash
	copy(h[:hashSize], b)
	return h
}

func Test_decodeRefRecord(t *testing.T) {
	t.Run("value_single_oid", func(t *testing.T) {
		// First record in a block: prevKey nil, prefix_length encodes 0.
		oid := make([]byte, 20)
		for i := range oid {
			oid[i] = byte(i + 1)
		}
		raw := encodeRefRecord(nil, "refs/heads/main", 1, 7, oid, nil, "", 20)

		rec, key, n, err := decodeRefRecord(raw, nil, 100, 20)
		require.NoError(t, err)
		assert.Equal(t, len(raw), n)
		assert.Equal(t, []byte("refs/heads/main"), key)
		assert.Equal(t, "refs/heads/main", rec.Name)
		assert.Equal(t, uint8(1), rec.ValueType)
		assert.Equal(t, uint64(107), rec.UpdateIndex)
		assert.Equal(t, hashFromBytes(oid, 20), rec.Value)
		assert.Equal(t, objfmt.Hash{}, rec.Peeled)
		assert.Empty(t, rec.Target)
	})

	t.Run("value_two_oids_peeled", func(t *testing.T) {
		val := make([]byte, 20)
		peel := make([]byte, 20)
		for i := range 20 {
			val[i] = byte(0xAA)
			peel[i] = byte(0x55)
		}
		raw := encodeRefRecord(nil, "refs/tags/v1", 2, 0, val, peel, "", 20)

		rec, _, n, err := decodeRefRecord(raw, nil, 50, 20)
		require.NoError(t, err)
		assert.Equal(t, len(raw), n)
		assert.Equal(t, "refs/tags/v1", rec.Name)
		assert.Equal(t, uint8(2), rec.ValueType)
		assert.Equal(t, uint64(50), rec.UpdateIndex)
		assert.Equal(t, hashFromBytes(val, 20), rec.Value)
		assert.Equal(t, hashFromBytes(peel, 20), rec.Peeled)
		assert.NotEqual(t, rec.Value, rec.Peeled)
		assert.Empty(t, rec.Target)
	})

	t.Run("symref", func(t *testing.T) {
		raw := encodeRefRecord(nil, "HEAD", 3, 1, nil, nil, "refs/heads/main", 20)

		rec, _, n, err := decodeRefRecord(raw, nil, 10, 20)
		require.NoError(t, err)
		assert.Equal(t, len(raw), n)
		assert.Equal(t, "HEAD", rec.Name)
		assert.Equal(t, uint8(3), rec.ValueType)
		assert.Equal(t, uint64(11), rec.UpdateIndex)
		assert.Equal(t, "refs/heads/main", rec.Target)
		assert.Equal(t, objfmt.Hash{}, rec.Value)
		assert.Equal(t, objfmt.Hash{}, rec.Peeled)
	})

	t.Run("deletion_tombstone", func(t *testing.T) {
		raw := encodeRefRecord(nil, "refs/heads/gone", 0, 3, nil, nil, "", 20)

		rec, _, n, err := decodeRefRecord(raw, nil, 0, 20)
		require.NoError(t, err)
		assert.Equal(t, len(raw), n)
		assert.Equal(t, "refs/heads/gone", rec.Name)
		assert.Equal(t, uint8(0), rec.ValueType)
		assert.Equal(t, uint64(3), rec.UpdateIndex)
		assert.Equal(t, objfmt.Hash{}, rec.Value)
		assert.Equal(t, objfmt.Hash{}, rec.Peeled)
		assert.Empty(t, rec.Target)
	})

	t.Run("prefix_compressed_chain", func(t *testing.T) {
		// Three records sharing prefixes. Each record's prevKey is the
		// fully reconstructed key from the previous step. The buffer is
		// concatenated so each decode starts at the previous decode's
		// end offset.
		oid := make([]byte, 20)
		for i := range oid {
			oid[i] = byte(i + 1)
		}
		names := []string{
			"refs/heads/main",
			"refs/heads/maint",
			"refs/tags/v1",
		}

		var buf []byte
		var prev []byte
		offsets := []int{}
		for _, n := range names {
			offsets = append(offsets, len(buf))
			buf = append(buf, encodeRefRecord(prev, n, 1, 0, oid, nil, "", 20)...)
			prev = []byte(n)
		}

		var key []byte
		for i, name := range names {
			rec, k, _, err := decodeRefRecord(buf[offsets[i]:], key, 0, 20)
			require.NoError(t, err)
			assert.Equal(t, name, rec.Name)
			assert.Equal(t, []byte(name), k)
			key = k
		}
	})

	t.Run("sha256_hash_size", func(t *testing.T) {
		oid := make([]byte, 32)
		for i := range oid {
			oid[i] = byte(i + 1)
		}
		raw := encodeRefRecord(nil, "refs/heads/main", 1, 0, oid, nil, "", 32)

		rec, _, n, err := decodeRefRecord(raw, nil, 5, 32)
		require.NoError(t, err)
		assert.Equal(t, len(raw), n)
		assert.Equal(t, uint64(5), rec.UpdateIndex)
		assert.Equal(t, hashFromBytes(oid, 32), rec.Value)
		// Every byte of the 32-byte buffer is populated for SHA-256.
		for i := range 32 {
			assert.Equal(t, byte(i+1), rec.Value[i])
		}
	})

	t.Run("reserved_value_type", func(t *testing.T) {
		// Hand-craft a record whose value_type=4 (reserved). The decoder
		// must reject it before attempting to read any value bytes.
		raw := encodeKey(nil, []byte("HEAD"), 4)
		raw = append(raw, encodeVarint(0)...)

		_, _, _, err := decodeRefRecord(raw, nil, 0, 20)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrUnsupportedValueType), "want ErrUnsupportedValueType, got %v", err)
	})

	t.Run("truncated_value", func(t *testing.T) {
		// value_type=1 expects 20 bytes of OID; supply only 10.
		raw := encodeKey(nil, []byte("refs/heads/main"), 1)
		raw = append(raw, encodeVarint(0)...)
		raw = append(raw, make([]byte, 10)...)

		_, _, _, err := decodeRefRecord(raw, nil, 0, 20)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrTruncatedRecord), "want ErrTruncatedRecord, got %v", err)
	})

	t.Run("symref_target_truncated", func(t *testing.T) {
		// value_type=3, target_len claims 16 but only 4 bytes follow.
		raw := encodeKey(nil, []byte("HEAD"), 3)
		raw = append(raw, encodeVarint(0)...)  // update_index_delta
		raw = append(raw, encodeVarint(16)...) // target_len
		raw = append(raw, []byte("refs")...)   // truncated target

		_, _, _, err := decodeRefRecord(raw, nil, 0, 20)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrTruncatedRecord), "want ErrTruncatedRecord, got %v", err)
	})

	t.Run("update_index_overflow", func(t *testing.T) {
		// min_update_index near MaxUint64 plus a non-zero delta would
		// wrap; the decoder must reject rather than silently overflow.
		raw := encodeRefRecord(nil, "HEAD", 0, 5, nil, nil, "", 20)

		_, _, _, err := decodeRefRecord(raw, nil, ^uint64(0)-2, 20)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrUpdateIndexOverflow), "want ErrUpdateIndexOverflow, got %v", err)
	})
}

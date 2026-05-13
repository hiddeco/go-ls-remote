package reftable

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
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

// sha1FromBytes left-pads (or fills) a [objfmt.SHA1Hash] with the given
// bytes. Mirrors the convention used at ref-record decode time: the
// 20-byte hash sits directly in the typed array.
func sha1FromBytes(b []byte) objfmt.SHA1Hash {
	var h objfmt.SHA1Hash
	copy(h[:], b)
	return h
}

// sha256FromBytes fills a [objfmt.SHA256Hash] with the given bytes.
func sha256FromBytes(b []byte) objfmt.SHA256Hash {
	var h objfmt.SHA256Hash
	copy(h[:], b)
	return h
}

func Test_decodeRefRecord(t *testing.T) {
	t.Parallel()

	t.Run("value_single_oid", func(t *testing.T) {
		t.Parallel()

		// First record in a block: prevKey nil, prefix_length encodes 0.
		oid := make([]byte, 20)
		for i := range oid {
			oid[i] = byte(i + 1)
		}
		raw := encodeRefRecord(nil, "refs/heads/main", 1, 7, oid, nil, "", 20)

		rec, n, err := decodeRefRecord[objfmt.SHA1Hash](raw, nil, nil, 100)
		require.NoError(t, err)
		assert.Equal(t, len(raw), n)
		assert.Equal(t, []byte("refs/heads/main"), rec.Name)
		assert.Equal(t, uint8(1), rec.ValueType)
		assert.Equal(t, sha1FromBytes(oid), rec.Value)
		assert.Equal(t, objfmt.SHA1Hash{}, rec.Peeled)
		assert.Empty(t, rec.Target)
	})

	t.Run("value_two_oids_peeled", func(t *testing.T) {
		t.Parallel()

		val := make([]byte, 20)
		peel := make([]byte, 20)
		for i := range 20 {
			val[i] = byte(0xAA)
			peel[i] = byte(0x55)
		}
		raw := encodeRefRecord(nil, "refs/tags/v1", 2, 0, val, peel, "", 20)

		rec, n, err := decodeRefRecord[objfmt.SHA1Hash](raw, nil, nil, 50)
		require.NoError(t, err)
		assert.Equal(t, len(raw), n)
		assert.Equal(t, []byte("refs/tags/v1"), rec.Name)
		assert.Equal(t, uint8(2), rec.ValueType)
		assert.Equal(t, sha1FromBytes(val), rec.Value)
		assert.Equal(t, sha1FromBytes(peel), rec.Peeled)
		assert.NotEqual(t, rec.Value, rec.Peeled)
		assert.Empty(t, rec.Target)
	})

	t.Run("symref", func(t *testing.T) {
		t.Parallel()

		raw := encodeRefRecord(nil, "HEAD", 3, 1, nil, nil, "refs/heads/main", 20)

		rec, n, err := decodeRefRecord[objfmt.SHA1Hash](raw, nil, nil, 10)
		require.NoError(t, err)
		assert.Equal(t, len(raw), n)
		assert.Equal(t, []byte("HEAD"), rec.Name)
		assert.Equal(t, uint8(3), rec.ValueType)
		assert.Equal(t, []byte("refs/heads/main"), rec.Target)
		assert.Equal(t, objfmt.SHA1Hash{}, rec.Value)
		assert.Equal(t, objfmt.SHA1Hash{}, rec.Peeled)
	})

	t.Run("deletion_tombstone", func(t *testing.T) {
		t.Parallel()

		raw := encodeRefRecord(nil, "refs/heads/gone", 0, 3, nil, nil, "", 20)

		rec, n, err := decodeRefRecord[objfmt.SHA1Hash](raw, nil, nil, 0)
		require.NoError(t, err)
		assert.Equal(t, len(raw), n)
		assert.Equal(t, []byte("refs/heads/gone"), rec.Name)
		assert.Equal(t, uint8(0), rec.ValueType)
		assert.Equal(t, objfmt.SHA1Hash{}, rec.Value)
		assert.Equal(t, objfmt.SHA1Hash{}, rec.Peeled)
		assert.Empty(t, rec.Target)
	})

	t.Run("prefix_compressed_chain", func(t *testing.T) {
		t.Parallel()

		// Three records sharing prefixes. Each record's prevKey is the
		// fully reconstructed key from the previous step. The buffer is
		// concatenated so each decode starts at the previous decode's
		// end offset.
		//
		// Passes nil scratch every iteration so every decode allocates
		// fresh; pairs with the ping_pong_chain sibling below which
		// exercises the scratch-reuse path.
		oid := make([]byte, 20)
		for i := range oid {
			oid[i] = byte(i + 1)
		}
		names := []string{
			"refs/heads/main",
			"refs/heads/maint",
			"refs/tags/v1",
		}

		buf := make([]byte, 0, 256)
		var prev []byte
		offsets := make([]int, 0, len(names))
		for _, n := range names {
			offsets = append(offsets, len(buf))
			buf = append(buf, encodeRefRecord(prev, n, 1, 0, oid, nil, "", 20)...)
			prev = []byte(n)
		}

		var key []byte
		for i, name := range names {
			rec, _, err := decodeRefRecord[objfmt.SHA1Hash](buf[offsets[i]:], key, nil, 0)
			require.NoError(t, err)
			assert.Equal(t, []byte(name), rec.Name)
			key = rec.Name
		}
	})

	t.Run("ping_pong_chain", func(t *testing.T) {
		t.Parallel()

		// Walks the same chain as prefix_compressed_chain but threads
		// two real scratch buffers via [keyBuf], exercising the
		// scratch-reuse path that the walker uses. A regression in
		// `decodeKey`'s copy ordering (overwriting prev before its
		// prefix bytes are read) would produce a corrupted second key
		// and fail the assertions below.
		oid := make([]byte, 20)
		for i := range oid {
			oid[i] = byte(i + 1)
		}
		names := []string{
			"refs/heads/branch-1",
			"refs/heads/branch-12",
			"refs/heads/branch-2",
			"refs/heads/main",
		}

		buf := make([]byte, 0, 256)
		var prevName []byte
		offsets := make([]int, 0, len(names))
		for _, n := range names {
			offsets = append(offsets, len(buf))
			buf = append(buf, encodeRefRecord(prevName, n, 1, 0, oid, nil, "", 20)...)
			prevName = []byte(n)
		}

		var kb keyBuf
		for i, name := range names {
			prev, scratch := kb.Next()
			rec, _, err := decodeRefRecord[objfmt.SHA1Hash](
				buf[offsets[i]:], prev, scratch, 0)
			require.NoError(t, err)
			assert.Equal(t, []byte(name), rec.Name,
				"decode %d (%q): keyBuf-threaded scratch yielded wrong name", i, name)
			kb.Swap(rec.Name)
		}
	})

	t.Run("sha256_hash_size", func(t *testing.T) {
		t.Parallel()

		oid := make([]byte, 32)
		for i := range oid {
			oid[i] = byte(i + 1)
		}
		raw := encodeRefRecord(nil, "refs/heads/main", 1, 0, oid, nil, "", 32)

		rec, n, err := decodeRefRecord[objfmt.SHA256Hash](raw, nil, nil, 5)
		require.NoError(t, err)
		assert.Equal(t, len(raw), n)
		assert.Equal(t, sha256FromBytes(oid), rec.Value)
		// Every byte of the 32-byte buffer is populated for SHA-256.
		for i := range 32 {
			assert.Equal(t, byte(i+1), rec.Value[i])
		}
	})

	t.Run("reserved_value_type", func(t *testing.T) {
		t.Parallel()

		// Hand-craft a record whose value_type=4 (reserved). The decoder
		// must reject it before attempting to read any value bytes.
		raw := encodeKey(nil, []byte("HEAD"), 4)
		raw = append(raw, encodeVarint(0)...)

		_, _, err := decodeRefRecord[objfmt.SHA1Hash](raw, nil, nil, 0)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrUnsupportedValueType, "want ErrUnsupportedValueType, got %v", err)
	})

	t.Run("truncated_value", func(t *testing.T) {
		t.Parallel()

		// value_type=1 expects 20 bytes of OID; supply only 10.
		raw := encodeKey(nil, []byte("refs/heads/main"), 1)
		raw = append(raw, encodeVarint(0)...)
		raw = append(raw, make([]byte, 10)...)

		_, _, err := decodeRefRecord[objfmt.SHA1Hash](raw, nil, nil, 0)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrTruncatedRecord, "want ErrTruncatedRecord, got %v", err)
	})

	t.Run("symref_target_truncated", func(t *testing.T) {
		t.Parallel()

		// value_type=3, target_len claims 16 but only 4 bytes follow.
		raw := encodeKey(nil, []byte("HEAD"), 3)
		raw = append(raw, encodeVarint(0)...)  // update_index_delta
		raw = append(raw, encodeVarint(16)...) // target_len
		raw = append(raw, []byte("refs")...)   // truncated target

		_, _, err := decodeRefRecord[objfmt.SHA1Hash](raw, nil, nil, 0)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrTruncatedRecord, "want ErrTruncatedRecord, got %v", err)
	})

	t.Run("update_index_overflow", func(t *testing.T) {
		t.Parallel()

		// min_update_index near MaxUint64 plus a non-zero delta would
		// wrap; the decoder must reject rather than silently overflow.
		raw := encodeRefRecord(nil, "HEAD", 0, 5, nil, nil, "", 20)

		_, _, err := decodeRefRecord[objfmt.SHA1Hash](raw, nil, nil, ^uint64(0)-2)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrUpdateIndexOverflow, "want ErrUpdateIndexOverflow, got %v", err)
	})
}

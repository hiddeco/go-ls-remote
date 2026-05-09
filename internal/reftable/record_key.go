package reftable

import "fmt"

// decodeKey decodes the prefix-compressed key shared by every reftable
// record type (ref, index, obj, log) from the start of buf and
// reconstructs the full key by combining a `prefix_length` slice of
// prevKey with the new `suffix`.
//
// The on-disk layout is (reftable.adoc §"ref record" / §"index record"):
//
//	varint( prefix_length )
//	varint( (suffix_length << 3) | extra )
//	suffix
//
// `extra` carries the low 3 bits of the second varint and is type-
// specific:
//
//   - ref records: `value_type` (0=delete, 1=value, 2=value+peeled,
//     3=symref; 4..7 reserved),
//   - index and obj records: always 0,
//   - log records: `log_type`.
//
// Records sitting at a `restart_offset` always have prefix_length=0
// (reftable.adoc §"ref record"), so a caller doing binary search via
// the restart table can pass prevKey=nil.
//
// decodeKey returns the reconstructed key, the 3-bit extra field, the
// number of bytes consumed from buf, and any error. It allocates a
// new slice for the returned key; callers may retain it without fear
// of aliasing buf or prevKey. The fresh allocation per call is
// intentional for v0 — sharing a running buffer between callers would
// invalidate prevKey on the next decode and add lifetime rules the
// public iterators do not need. A future hot-path tuning could thread
// a reusable buffer through the block walkers if profiling shows the
// allocation matters.
//
// Errors:
//   - ErrTruncatedRecord wraps a buffer that ends mid-varint, mid-
//     suffix, or whose prefix_length exceeds prevKey.
//   - ErrVarintOverflow wraps a 10-byte-plus varint, propagated from
//     [decodeVarint].
//
// See `reftable/record.c::reftable_decode_key` for the canonical
// reference.
func decodeKey(buf []byte, prevKey []byte) ([]byte, uint8, int, error) {
	prefixLen, n1, err := decodeVarint(buf)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("reftable: prefix_length: %w", err)
	}

	rest := buf[n1:]
	packed, n2, err := decodeVarint(rest)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("reftable: suffix_length|extra: %w", err)
	}

	extra := uint8(packed & 0x7)
	suffixLen := packed >> 3

	rest = rest[n2:]
	if uint64(len(rest)) < suffixLen {
		return nil, 0, 0, fmt.Errorf("reftable: suffix wants %d bytes, have %d: %w", suffixLen, len(rest), ErrTruncatedRecord)
	}
	if prefixLen > uint64(len(prevKey)) {
		return nil, 0, 0, fmt.Errorf("reftable: prefix_length %d exceeds prev key %d: %w", prefixLen, len(prevKey), ErrTruncatedRecord)
	}

	key := make([]byte, prefixLen+suffixLen)
	copy(key, prevKey[:prefixLen])
	copy(key[prefixLen:], rest[:suffixLen])

	return key, extra, n1 + n2 + int(suffixLen), nil
}

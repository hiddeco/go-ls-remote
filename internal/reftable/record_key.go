package reftable

import "fmt"

// decodeKey decodes the prefix-compressed key shared by every reftable
// record type (ref, index, obj, log).
//
// The full key is reconstructed by combining a `prefix_length` slice
// of prevKey with the new `suffix` parsed from buf. The on-disk layout
// is (reftable.adoc §"ref record" / §"index record"):
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
// scratch is an optional caller-owned byte buffer. When `cap(scratch)
// >= prefix_length+suffix_length`, decodeKey reuses the underlying
// array; otherwise it allocates a fresh slice. The returned slice
// always aliases either `scratch`'s underlying array or a fresh
// allocation — never `prevKey` or `buf`. Pass nil to force a fresh
// allocation (matches the seek path's compare-and-discard usage).
//
// The returned key has `len == prefix_length + suffix_length`. Its
// `cap` is deliberately NOT bounded to len: callers feed the result
// back as the next call's scratch and rely on the underlying array
// retaining its full capacity so subsequent decodes of shorter or
// equal-length keys can reuse it without allocation. Callers therefore
// must use `len(key)`, not `cap(key)`, to size any follow-up read —
// bytes past `len(key)` are stale prior-key data from an earlier
// decode, not part of the current record.
//
// Errors:
//   - [ErrTruncatedRecord] wraps a buffer that ends mid-varint, mid-
//     suffix, or whose prefix_length exceeds prevKey.
//   - [ErrVarintOverflow] wraps a 10-byte-plus varint, propagated from
//     [decodeVarint].
//
// See `reftable/record.c::reftable_decode_key` for the canonical
// reference.
func decodeKey(buf, prevKey, scratch []byte) ([]byte, uint8, int, error) {
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

	keyLen := int(prefixLen) + int(suffixLen)
	var key []byte
	if cap(scratch) >= keyLen {
		// Preserve scratch's full cap on the return slice so the next
		// decode that ping-pongs this buffer back in as scratch can
		// reuse it for a key of any length up to that cap; capping
		// here would freeze buffers at their first-decode length and
		// defeat steady-state reuse in namespaces with varying key
		// lengths (e.g. branch-1, branch-10, branch-100).
		key = scratch[:keyLen]
	} else {
		key = make([]byte, keyLen)
	}
	copy(key, prevKey[:prefixLen])
	copy(key[prefixLen:], rest[:suffixLen])

	return key, extra, n1 + n2 + int(suffixLen), nil
}

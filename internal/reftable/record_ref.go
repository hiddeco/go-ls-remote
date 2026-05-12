package reftable

import (
	"errors"
	"fmt"
	"math"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
)

// Sentinel errors returned by the ref-record decoder. Callers match
// against these with [errors.Is]; the wrapping
// `fmt.Errorf("...: %w", ..., sentinel)` adds the offending value
// (declared lengths, value_type) for diagnostics.
var (
	// ErrUnsupportedValueType is returned when a ref_record carries a
	// value_type in the reserved range 0x4..0x7. The reftable spec
	// reserves these for future use; treating them as a hard error
	// stops a forward-incompatible reader from silently dropping
	// records it cannot interpret.
	ErrUnsupportedValueType = errors.New("reftable: unsupported value_type")

	// ErrUpdateIndexOverflow is returned when min_update_index +
	// update_index_delta would exceed uint64. A well-formed reftable
	// never approaches this — Git uses update indices as monotonic
	// counters with realistic ranges — but a hostile file could
	// declare extreme values, and silently wrapping would corrupt
	// downstream comparisons.
	ErrUpdateIndexOverflow = errors.New("reftable: update_index overflow")
)

// Reftable also defines obj_records (OID → ref-index lookup) and
// log_records (reflog). Neither is decoded here. The public read path
// lists refs forward and resolves OIDs against pack/idx, never against
// ref indexes; reflog is out of scope per the reftable spec's
// "Out of scope" section. A future contributor adding OID → ref lookup
// or reflog reading should implement the corresponding record decoders
// alongside this file.

// refRecord is the decoded form of one ref_record. Fields beyond Name
// and ValueType are populated only for the value types that carry
// them: Value for types 1 and 2, Peeled for type 2, Target for type 3.
// Type 0 (deletion / tombstone) leaves all three zero.
//
// The on-disk `update_index` is intentionally not retained. ls-remote
// consumers never observe it — the public [RefRecord] does not carry
// it and the [Stack] merge keys it implicitly via newest-table-wins
// ordering — so storing it per record would pay alloc and copy cost
// for a field that has no reader. [decodeRefRecord] still validates
// the on-disk `update_index_delta` for overflow (see
// [ErrUpdateIndexOverflow]) so malformed inputs are rejected eagerly.
//
// Name and Target are byte slices with borrowed-buffer lifetimes (see
// [decodeRefRecord]). Callers that retain a record past the next
// decode must copy these fields explicitly.
//
// The hash type parameter `H` carries the on-disk OID width: a SHA-1
// reftable instantiates `refRecord[objfmt.SHA1Hash]`, a SHA-256 file
// `refRecord[objfmt.SHA256Hash]`. Mixed-algorithm inputs surface at
// [OpenReader] / [OpenStack] time as [ErrMixedHashAlgo] before any
// record bytes reach the decoder.
//
// Field layout mirrors [RefRecord]: two slice headers (alignment 8)
// at the front, the two hash arrays (alignment 1) packed back-to-back,
// and ValueType (alignment 1) trailing into the struct's tail padding.
type refRecord[H objfmt.Hash] struct {
	Name      []byte
	Target    []byte
	Value     H
	Peeled    H
	ValueType uint8
}

// [reftable.adoc § ref record] — the low 3 bits of the second varint in
// each ref record carry value_type. These constants name the four
// defined values; 0x4..0x7 are reserved by the spec.
//
// [reftable.adoc § ref record]: https://github.com/git/git/blob/v2.54.0/Documentation/technical/reftable.adoc#ref-record
const (
	refValueDeletion uint8 = 0x0
	refValueSingle   uint8 = 0x1
	refValuePeeled   uint8 = 0x2
	refValueSymref   uint8 = 0x3
)

// decodeRefRecord decodes a single ref_record from the start of buf
// and reconstructs the record's key by combining a prefix slice of
// prevKey with the new suffix. The decoded key becomes the new prevKey
// for the next call (read it from the returned record's Name field);
// the caller threads it back via a ping-pong of two scratch buffers
// so the previous key remains valid while the next key is being
// decoded. The [keyBuf] helper encapsulates the swap.
//
// prevKey is the key from the previous record in the same block, or
// nil at a restart point or at the start of a block. scratch is a
// caller-owned buffer for the reconstructed key; pass the buffer
// that is NOT currently aliased by prevKey (the walker swaps the
// two each iteration). When `cap(scratch) >= keyLen`, decodeRefRecord
// reuses it; otherwise a fresh allocation happens.
//
// The returned [refRecord.Name] is a slice into either scratch's
// underlying array or a fresh allocation, and is valid until the
// next decode reuses scratch. [refRecord.Target], when present (for
// symref records), is a slice into buf and is valid for as long as
// buf is valid.
//
// minUpdateIndex is the file header's `min_update_index`. The decoder
// validates that `minUpdateIndex + update_index_delta` fits in uint64
// (returning [ErrUpdateIndexOverflow] on wrap) but does not retain
// the sum: [refRecord] no longer carries `UpdateIndex`.
//
// Errors:
//   - [ErrTruncatedRecord] wraps a buffer that ends mid-record, a
//     prefix_length that exceeds prevKey, or a target_len that runs
//     past buf.
//   - [ErrUpdateIndexOverflow] wraps minUpdateIndex+delta wrapping
//     uint64 (the wrapped value would compare incorrectly in a
//     merged stack).
//   - [ErrUnsupportedValueType] wraps value_type 4..7 (reserved in
//     the on-disk format).
//
// The hash size is the constant `len(*new(H))` — 20 for
// [objfmt.SHA1Hash] or 32 for [objfmt.SHA256Hash]. Folding it into the
// type parameter removes a redundant runtime argument and lets the
// compiler emit the value/peeled copies as constant-length moves.
//
// See [reftable/record.c::reftable_ref_record_decode] for the
// canonical implementation.
//
// [reftable/record.c::reftable_ref_record_decode]: https://github.com/git/git/blob/v2.54.0/reftable/record.c#L360
func decodeRefRecord[H objfmt.Hash](buf, prevKey, scratch []byte, minUpdateIndex uint64) (refRecord[H], int, error) {
	var zero refRecord[H]
	hashSize := len(zero.Value)

	key, valueType, n1, err := decodeKey(buf, prevKey, scratch)
	if err != nil {
		return zero, 0, fmt.Errorf("reftable: ref_record key: %w", err)
	}

	rest := buf[n1:]
	delta, n2, err := decodeVarint(rest)
	if err != nil {
		return zero, 0, fmt.Errorf("reftable: ref_record update_index_delta: %w", err)
	}

	// Detect minUpdateIndex + delta wrapping uint64. The sum is not
	// stored — the public [RefRecord] does not carry update_index, and
	// [refRecord] has been trimmed to match — but the check still flags
	// on-disk corruption that [reftable.adoc § ref record] disallows.
	//
	// [reftable.adoc § ref record]: https://github.com/git/git/blob/v2.54.0/Documentation/technical/reftable.adoc#ref-record
	if delta > math.MaxUint64-minUpdateIndex {
		return zero, 0, fmt.Errorf("reftable: min %d + delta %d wraps uint64: %w", minUpdateIndex, delta, ErrUpdateIndexOverflow)
	}

	rec := refRecord[H]{
		Name:      key,
		ValueType: valueType,
	}

	rest = rest[n2:]
	consumed := n1 + n2

	switch valueType {
	case refValueDeletion:
		// No value bytes follow.
	case refValueSingle:
		if len(rest) < hashSize {
			return zero, 0, fmt.Errorf("reftable: ref_record value wants %d bytes, have %d: %w", hashSize, len(rest), ErrTruncatedRecord)
		}
		copy(objfmt.HashBytes(&rec.Value), rest[:hashSize])
		consumed += hashSize
	case refValuePeeled:
		if len(rest) < 2*hashSize {
			return zero, 0, fmt.Errorf("reftable: ref_record value+peeled want %d bytes, have %d: %w", 2*hashSize, len(rest), ErrTruncatedRecord)
		}
		copy(objfmt.HashBytes(&rec.Value), rest[:hashSize])
		copy(objfmt.HashBytes(&rec.Peeled), rest[hashSize:2*hashSize])
		consumed += 2 * hashSize
	case refValueSymref:
		targetLen, n3, err := decodeVarint(rest)
		if err != nil {
			return zero, 0, fmt.Errorf("reftable: ref_record target_len: %w", err)
		}
		rest = rest[n3:]
		if uint64(len(rest)) < targetLen {
			return zero, 0, fmt.Errorf("reftable: ref_record target wants %d bytes, have %d: %w", targetLen, len(rest), ErrTruncatedRecord)
		}
		// Target aliases buf directly; no allocation. buf itself is a
		// slice into block.bytes (which slices into the Reader's
		// underlying file per `block.go`), so Target is valid for as
		// long as the Reader is open.
		rec.Target = rest[:targetLen]
		consumed += n3 + int(targetLen)
	default:
		// 0x4..0x7 are reserved per [reftable.adoc § ref record].
		//
		// [reftable.adoc § ref record]: https://github.com/git/git/blob/v2.54.0/Documentation/technical/reftable.adoc#ref-record
		return zero, 0, fmt.Errorf("reftable: value_type %#x: %w", valueType, ErrUnsupportedValueType)
	}

	return rec, consumed, nil
}

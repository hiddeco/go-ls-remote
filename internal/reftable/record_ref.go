package reftable

import (
	"errors"
	"fmt"

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
// "Out of scope" section. The footer's obj_position is preserved in
// the diagnostic dump but the records themselves stay undecoded. A
// future contributor adding OID → ref lookup or reflog reading should
// implement the corresponding record decoders alongside this file.

// refRecord is the decoded form of one ref_record. Fields beyond Name,
// UpdateIndex, and ValueType are populated only for the value types
// that carry them: Value for types 1 and 2, Peeled for type 2, Target
// for type 3. Type 0 (deletion / tombstone) leaves all three zero.
type refRecord struct {
	Name        string
	UpdateIndex uint64
	ValueType   uint8
	Value       objfmt.Hash
	Peeled      objfmt.Hash
	Target      string
}

// reftable.adoc §"ref record" — the low 3 bits of the second varint in
// each ref record carry value_type. These constants name the four
// defined values; 0x4..0x7 are reserved by the spec.
const (
	refValueDeletion uint8 = 0x0
	refValueSingle   uint8 = 0x1
	refValuePeeled   uint8 = 0x2
	refValueSymref   uint8 = 0x3
)

// decodeRefRecord decodes one ref_record at buf[0]. minUpdateIndex is
// the value taken from the file header; the function adds the on-disk
// update_index_delta to it to materialise the record's UpdateIndex.
//
// prevKey is the running prefix-decompression context: the fully
// reconstructed key from the previous record in the same block.
// Callers seeking via the restart table pass nil — restart-point
// records are required by the spec to encode prefix_length=0, so the
// suffix is the complete key.
//
// The function returns the decoded record, the reconstructed key (so
// callers can update prevKey), and the number of bytes consumed.
//
// Errors:
//   - [ErrTruncatedRecord] / [ErrVarintOverflow] propagate from the
//     key, varint, and value-byte readers when the buffer is short.
//   - [ErrUnsupportedValueType] is returned when the on-disk
//     value_type is in the reserved range 0x4..0x7.
//   - [ErrUpdateIndexOverflow] is returned when adding the delta to
//     minUpdateIndex would exceed uint64.
//
// hashSize must match the file's hash algorithm — 20 for [objfmt.SHA1]
// or 32 for [objfmt.SHA256]. Passing it explicitly keeps the decoder
// independent of the [objfmt.Algo] dispatch.
//
// See `reftable/record.c::reftable_ref_record_decode` for the
// canonical implementation.
func decodeRefRecord(buf, prevKey []byte, minUpdateIndex uint64, hashSize int) (refRecord, []byte, int, error) {
	key, valueType, n1, err := decodeKey(buf, prevKey)
	if err != nil {
		return refRecord{}, nil, 0, fmt.Errorf("reftable: ref_record key: %w", err)
	}

	rest := buf[n1:]
	delta, n2, err := decodeVarint(rest)
	if err != nil {
		return refRecord{}, nil, 0, fmt.Errorf("reftable: ref_record update_index_delta: %w", err)
	}

	// minUpdateIndex + delta must fit in uint64. Detect wrap-around
	// rather than silently truncating; a wrapped index would compare
	// incorrectly against the file header's max_update_index and
	// confuse merged-stack ordering.
	updateIndex := minUpdateIndex + delta
	if updateIndex < minUpdateIndex {
		return refRecord{}, nil, 0, fmt.Errorf("reftable: min %d + delta %d wraps uint64: %w", minUpdateIndex, delta, ErrUpdateIndexOverflow)
	}

	rec := refRecord{
		Name:        string(key),
		UpdateIndex: updateIndex,
		ValueType:   valueType,
	}

	rest = rest[n2:]
	consumed := n1 + n2

	switch valueType {
	case refValueDeletion:
		// No value bytes follow.
	case refValueSingle:
		if len(rest) < hashSize {
			return refRecord{}, nil, 0, fmt.Errorf("reftable: ref_record value wants %d bytes, have %d: %w", hashSize, len(rest), ErrTruncatedRecord)
		}
		copy(rec.Value[:hashSize], rest[:hashSize])
		consumed += hashSize
	case refValuePeeled:
		if len(rest) < 2*hashSize {
			return refRecord{}, nil, 0, fmt.Errorf("reftable: ref_record value+peeled want %d bytes, have %d: %w", 2*hashSize, len(rest), ErrTruncatedRecord)
		}
		copy(rec.Value[:hashSize], rest[:hashSize])
		copy(rec.Peeled[:hashSize], rest[hashSize:2*hashSize])
		consumed += 2 * hashSize
	case refValueSymref:
		targetLen, n3, err := decodeVarint(rest)
		if err != nil {
			return refRecord{}, nil, 0, fmt.Errorf("reftable: ref_record target_len: %w", err)
		}
		rest = rest[n3:]
		if uint64(len(rest)) < targetLen {
			return refRecord{}, nil, 0, fmt.Errorf("reftable: ref_record target wants %d bytes, have %d: %w", targetLen, len(rest), ErrTruncatedRecord)
		}
		rec.Target = string(rest[:targetLen])
		consumed += n3 + int(targetLen)
	default:
		// 0x4..0x7 are reserved per reftable.adoc §"ref record".
		return refRecord{}, nil, 0, fmt.Errorf("reftable: value_type %#x: %w", valueType, ErrUnsupportedValueType)
	}

	return rec, key, consumed, nil
}

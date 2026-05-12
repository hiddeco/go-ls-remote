package reftable

import (
	"errors"
	"fmt"
)

// Sentinel errors returned by the varint and record-key decoders.
// Callers match against these with [errors.Is]; the wrapping
// `fmt.Errorf("...: %w", ..., sentinel)` adds the offending value
// (declared lengths, byte counts) for diagnostics.
var (
	// ErrTruncatedRecord is returned when a varint or prefix-compressed
	// record key cannot be fully decoded from the input slice: the
	// buffer ends mid-varint, the suffix length exceeds the remaining
	// bytes, or the prefix length exceeds the previous key.
	ErrTruncatedRecord = errors.New("reftable: truncated record")

	// ErrVarintOverflow is returned when a varint's encoded length
	// exceeds 10 bytes — the most a uint64 can need under reftable's
	// encoding — or when the running value would overflow uint64.
	ErrVarintOverflow = errors.New("reftable: varint overflow")
)

// varintMaxBytes caps the number of bytes a single varint may consume.
// Reftable's varint is identical to ofs-delta in pack files: at most
// 10 bytes are needed to encode any uint64. An 11th continuation byte
// is malformed input.
const varintMaxBytes = 10

// decodeVarint decodes a single reftable varint from the start of buf
// and returns its value, the number of bytes consumed, and any error.
//
// Reftable's varint is identical to the ofs-delta encoding used in
// pack files ([reftable.adoc § Varint encoding]):
//
//	val = buf[0] & 0x7f
//	while (buf[i] & 0x80) {
//	    i++
//	    val = ((val + 1) << 7) | (buf[i] & 0x7f)
//	}
//
// The `+1` step on every continuation byte is what differs from the
// classic protobuf-style varint: it removes the redundant zero
// encodings, so 0x80 means 0x80 (not 0) and very large values fit in
// the 10-byte limit.
//
// Returns:
//   - ErrTruncatedRecord when buf ends before a continuation byte that
//     the high bit of the previous byte promised,
//   - ErrVarintOverflow when an 11th byte is needed or the running
//     value would step past uint64.
//
// See [reftable/record.c::get_var_int] for the canonical reference;
// the overflow check there mirrors the bit shift used here.
//
// [reftable.adoc § Varint encoding]: https://github.com/git/git/blob/v2.54.0/Documentation/technical/reftable.adoc#varint-encoding
// [reftable/record.c::get_var_int]: https://github.com/git/git/blob/v2.54.0/reftable/record.c#L22
func decodeVarint(buf []byte) (uint64, int, error) {
	if len(buf) == 0 {
		return 0, 0, fmt.Errorf("reftable: empty buffer for varint: %w", ErrTruncatedRecord)
	}

	c := buf[0]
	val := uint64(c & 0x7f)
	i := 1

	for c&0x80 != 0 {
		// The +1 on every continuation byte (see formula above) means
		// the running value cannot fit in fewer than `varintMaxBytes`
		// total bytes; rejecting the 11th byte stops malformed input
		// before the shift below could overflow uint64.
		if i >= varintMaxBytes {
			return 0, 0, fmt.Errorf("reftable: varint exceeds %d bytes: %w", varintMaxBytes, ErrVarintOverflow)
		}
		// `val + 1` and `val << 7` must both fit in uint64. The check
		// mirrors [reftable/record.c::get_var_int]: if the top 7 bits
		// of val are set, the next shift would lose data.
		//
		// [reftable/record.c::get_var_int]: https://github.com/git/git/blob/v2.54.0/reftable/record.c#L22
		val++
		if val>>(64-7) != 0 {
			return 0, 0, fmt.Errorf("reftable: varint value would overflow uint64: %w", ErrVarintOverflow)
		}
		if i >= len(buf) {
			return 0, 0, fmt.Errorf("reftable: varint truncated at byte %d: %w", i, ErrTruncatedRecord)
		}
		c = buf[i]
		i++
		val = (val << 7) | uint64(c&0x7f)
	}

	return val, i, nil
}

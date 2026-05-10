package objfmt

import (
	"bytes"
	"encoding/binary"
)

// v1 record stride: a uint32 offset followed by a 20-byte SHA-1.
// Documented in `Documentation/gitformat-pack.adoc` lines 196-218.
const idxV1RecordSize = 4 + 20

// FindOffset returns the byte offset of the named object in the
// corresponding pack file. ok is false if the object is not in this
// idx.
//
// Returns `(-1, false)` for misses so callers can distinguish "not in
// this idx" from a legitimate offset of 0 — a real (non-trailer)
// object in a pack file always sits past the 12-byte pack header, so
// 0 is never a valid match anyway, but `-1` makes the intent explicit
// in conditional chains.
func (i *Idx[H]) FindOffset(h H) (int64, bool) {
	switch i.ver {
	case 1:
		return i.findOffsetV1(h)
	case 2:
		return i.findOffsetV2(h)
	default:
		return -1, false
	}
}

// findOffsetV1 binary-searches the v1 main table within the slice
// bounded by the fan-out.
//
// v1 only ever stored SHA-1 ids, so an [Idx] opened with `H` =
// [SHA256Hash] against a v1 file will fall through this function and
// return a miss for every lookup.
func (i *Idx[H]) findOffsetV1(h H) (int64, bool) {
	if i.algo != SHA1 || i.count == 0 || len(i.data) < 256*4 {
		return -1, false
	}

	// Fan-out: fanout[N] = number of object names whose first byte is
	// ≤ N. Hence entries with first byte == prefix occupy indices
	// [fanout[prefix-1], fanout[prefix]). See `gitformat-pack.adoc`
	// lines 198-202 and the v1 read path in `packfile.c`'s
	// `nth_packed_object_offset`.
	prefix := h[0]
	var lo, hi uint32
	if prefix > 0 {
		lo = binary.BigEndian.Uint32(i.data[(int(prefix)-1)*4 : int(prefix)*4])
	}
	hi = binary.BigEndian.Uint32(i.data[int(prefix)*4 : (int(prefix)+1)*4])
	if hi > i.count {
		return -1, false
	}

	tableStart := 256 * 4
	want := hashBytes(&h)
	for lo < hi {
		// Overflow-safe midpoint: `(lo + hi) / 2` would wrap when both
		// summands set bit 31; `lo + (hi-lo)/2` does not.
		mid := lo + (hi-lo)/2
		entry := i.data[tableStart+int(mid)*idxV1RecordSize:]
		// `idxV1RecordSize` bytes: 4 offset + 20 name.
		got := entry[4 : 4+20]
		switch bytes.Compare(want, got) {
		case 0:
			off := binary.BigEndian.Uint32(entry[0:4])
			return int64(off), true
		case -1:
			hi = mid
		case 1:
			lo = mid + 1
		}
	}
	return -1, false
}

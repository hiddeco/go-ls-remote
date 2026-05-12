package objfmt

import (
	"bytes"
	"encoding/binary"
)

// midxLargeOffsetMSB marks an offset slot in OOFF whose lower 31 bits
// index the LOFF chunk rather than encoding the offset directly.
// Mirrors `MIDX_LARGE_OFFSET_NEEDED` in `midx.h`.
const midxLargeOffsetMSB uint32 = 0x80000000

// midxOOFFRecordSize is the per-object stride in the OOFF chunk: a
// 4-byte pack index followed by a 4-byte offset (or LOFF index when
// the MSB is set). Mirrors `MIDX_CHUNK_OFFSET_WIDTH` in `midx.h`.
const midxOOFFRecordSize = 8

// Find looks h up across every pack referenced by the midx and
// returns the pack index (into [Midx.PackNames]) plus the byte
// offset of the object in that pack.
//
// ok is false if the OID is absent, if the offset slot has the LOFF
// flag set but the file lacks an LOFF chunk (corruption), or if the
// midx is closed. Lookups mirror canonical Git's `bsearch_one_midx`
// (`midx.c`) and `nth_midxed_offset` ([midx.c:561-582]): a 256-entry
// fanout bounds a binary search across `OIDL`, then OOFF gives the
// pack id and either a direct 31-bit offset or an LOFF index.
//
// [midx.c:561-582]: https://github.com/git/git/blob/v2.54.0/midx.c#L561-L582
func (m *Midx[H]) Find(h H) (packIndex uint32, offset int64, ok bool) {
	var zero H
	hashLen := len(zero)
	if m.count == 0 || hashLen == 0 || len(m.data) == 0 {
		return 0, 0, false
	}

	idx, found := m.searchOID(h)
	if !found {
		return 0, 0, false
	}

	ooff := m.chunks[chunkOOFF]
	rec := m.data[ooff.off+int64(idx)*midxOOFFRecordSize:]
	packIndex = binary.BigEndian.Uint32(rec[0:4])
	raw := binary.BigEndian.Uint32(rec[4:8])
	if raw&midxLargeOffsetMSB == 0 {
		return packIndex, int64(raw), true
	}

	// Top bit set: lower 31 bits index into LOFF, an array of 8-byte
	// big-endian offsets. A missing LOFF chunk on a flagged record is
	// corruption — surface ok=false rather than indexing past the
	// midx body.
	loff, hasLOFF := m.chunks[chunkLOFF]
	if !hasLOFF {
		return 0, 0, false
	}
	loffIdx := raw &^ midxLargeOffsetMSB
	want := int64(loffIdx)*8 + 8
	if want > loff.len {
		return 0, 0, false
	}
	off := binary.BigEndian.Uint64(m.data[loff.off+int64(loffIdx)*8 : loff.off+want])
	return packIndex, int64(off), true
}

// searchOID binary-searches the OIDL chunk for h, narrowed by the
// 256-entry OIDF fanout, and returns the matching record index.
//
// The fanout entry at index N is the cumulative count of OIDs whose
// first byte is ≤ N, so OIDs whose first byte equals `prefix` occupy
// [fanout[prefix-1], fanout[prefix]) within OIDL.
func (m *Midx[H]) searchOID(h H) (uint32, bool) {
	var zero H
	hashLen := len(zero)
	oidf := m.chunks[chunkOIDF]
	oidl := m.chunks[chunkOIDL]
	if oidf.len != 256*4 || oidl.len != int64(m.count)*int64(hashLen) {
		return 0, false
	}

	prefix := h[0]
	var lo, hi uint32
	if prefix > 0 {
		base := oidf.off + (int64(prefix)-1)*4
		lo = binary.BigEndian.Uint32(m.data[base : base+4])
	}
	hi = binary.BigEndian.Uint32(
		m.data[oidf.off+int64(prefix)*4 : oidf.off+(int64(prefix)+1)*4])
	if hi > m.count {
		return 0, false
	}

	want := HashBytes(&h)
	for lo < hi {
		// Overflow-safe midpoint: `(lo + hi) / 2` would wrap when both
		// summands set bit 31; `lo + (hi-lo)/2` does not.
		mid := lo + (hi-lo)/2
		base := oidl.off + int64(mid)*int64(hashLen)
		got := m.data[base : base+int64(hashLen)]
		switch bytes.Compare(want, got) {
		case 0:
			return mid, true
		case -1:
			hi = mid
		case 1:
			lo = mid + 1
		}
	}
	return 0, false
}

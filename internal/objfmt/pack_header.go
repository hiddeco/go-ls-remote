package objfmt

import (
	"fmt"
)

// ObjectHeader is the decoded form of a pack object's leading
// type/size header, plus the delta base reference for delta types.
type ObjectHeader struct {
	// Type is the 3-bit pack object type.
	Type ObjectType

	// DeltaRef carries the base reference for delta types. For
	// non-delta types the zero value is meaningful (no base).
	DeltaRef DeltaRef

	// Size is the value encoded in the header's size bits. For
	// non-delta objects this is the inflated body size; for delta
	// types this is the inflated delta payload size, not the size
	// of the resolved object.
	Size int64

	// BodyAt is the absolute offset of the first byte of the
	// (zlib-compressed) body in the pack — the type/size header
	// and any delta base bytes precede it.
	BodyAt int64
}

// DeltaRef identifies the base of a delta object. Exactly one of
// OfsBase or RefBase is meaningful, picked by the [ObjectHeader.Type]
// of the carrying header.
type DeltaRef struct {
	// OfsBase is the absolute offset of the base object in the
	// same pack. Set when the delta type is [TypeOfsDelta].
	OfsBase int64

	// RefBase is the hash of the base object. The base may live in
	// a different pack. Set when the delta type is [TypeRefDelta].
	RefBase Hash
}

// ReadHeader decodes the pack object header that begins at offset
// `at`, returning the type, declared size, optional delta base, and
// the absolute offset of the body that follows.
//
// Header bit layout per `packfile.c::unpack_object_header_buffer`
// (lines 1135-1158):
//
//	byte 0      bit 7    = continuation flag
//	            bits 6-4 = type (1..7)
//	            bits 3-0 = size LSB
//	byte 1..n   bit 7    = continuation flag
//	            bits 6-0 = next 7 size bits, little-endian
//	                       (shifts 4, 11, 18, 25, ...)
//
// For [TypeOfsDelta] the type/size header is followed by a
// big-endian offset varint with the +1-per-continuation-byte quirk
// per `packfile.c::get_delta_base` (lines 1278-1292); the absolute
// base offset is `at - relativeOffset`. For [TypeRefDelta] the next
// `algo.Size()` raw bytes are the base hash. Reserved type 5 is
// rejected; types 1..4, 6, 7 are accepted.
func (p *Pack) ReadHeader(at int64) (ObjectHeader, error) {
	if at < 0 || at >= int64(p.r.Len()) {
		return ObjectHeader{}, fmt.Errorf("objfmt: header offset %d out of range", at)
	}

	// 32 bytes covers any plausible type/size header (2^137 max
	// per `packfile.c:1228`) plus the OFS_DELTA varint; the extra
	// algo.Size() bytes cover REF_DELTA.
	const peek = 32
	want := peek + p.algo.Size()
	if rem := int64(p.r.Len()) - at; rem < int64(want) {
		want = int(rem)
	}
	buf := make([]byte, want)
	n, err := p.r.ReadAt(buf, at)
	if err != nil && n == 0 {
		return ObjectHeader{}, fmt.Errorf("objfmt: read header at %d: %w", at, err)
	}
	buf = buf[:n]
	if len(buf) == 0 {
		return ObjectHeader{}, fmt.Errorf("objfmt: empty pack header: %w", ErrTruncated)
	}

	typeBits := (buf[0] >> 4) & 0x07
	size := int64(buf[0] & 0x0f)
	shift := uint(4)
	used := 1
	for buf[used-1]&0x80 != 0 {
		if used >= len(buf) {
			return ObjectHeader{}, fmt.Errorf("objfmt: pack header overruns buffer: %w", ErrTruncated)
		}
		size |= int64(buf[used]&0x7f) << shift
		shift += 7
		used++
		if shift >= 64 {
			return ObjectHeader{}, fmt.Errorf("objfmt: pack header size overflow: %w", ErrCorrupt)
		}
	}

	hdr := ObjectHeader{Type: ObjectType(typeBits), Size: size}

	switch hdr.Type {
	case TypeOfsDelta:
		offset, advanced, err := readOfsBase(buf[used:])
		if err != nil {
			return ObjectHeader{}, err
		}
		hdr.DeltaRef.OfsBase = at - offset
		if hdr.DeltaRef.OfsBase <= 0 || hdr.DeltaRef.OfsBase >= at {
			return ObjectHeader{}, fmt.Errorf("objfmt: OFS_DELTA base offset out of range: %w", ErrCorrupt)
		}
		used += advanced
	case TypeRefDelta:
		hashLen := p.algo.Size()
		if used+hashLen > len(buf) {
			return ObjectHeader{}, fmt.Errorf("objfmt: REF_DELTA base hash overruns buffer: %w", ErrTruncated)
		}
		copy(hdr.DeltaRef.RefBase[:hashLen], buf[used:used+hashLen])
		used += hashLen
	case TypeCommit, TypeTree, TypeBlob, TypeTag:
		// no extra header bytes
	default:
		return ObjectHeader{}, fmt.Errorf("objfmt: unknown pack object type %d: %w", typeBits, ErrCorrupt)
	}

	hdr.BodyAt = at + int64(used)
	return hdr, nil
}

// readOfsBase decodes the OFS_DELTA relative-offset varint at the
// head of buf. The encoding is unusual: it is big-endian (most
// significant 7 bits first) and adds one per continuation byte to
// make the encoding unique. See `packfile.c::get_delta_base` lines
// 1278-1292 for the reference implementation.
//
// The overflow guard mirrors canonical Git's `MSB(base_offset, 7)`
// check (`packfile.c::1284`): reject the input as soon as the next
// `<< 7` shift would lose data, rather than letting it wrap silently.
// Canonical expresses the predicate over `uint64`; we use `int64`
// throughout so the threshold is `1 << 56` (one bit lower than
// canonical's `1 << 57`) to also catch the sign-bit flip that signed
// arithmetic introduces.
func readOfsBase(buf []byte) (int64, int, error) {
	if len(buf) == 0 {
		return 0, 0, fmt.Errorf("objfmt: empty OFS_DELTA offset: %w", ErrTruncated)
	}
	c := buf[0]
	off := int64(c & 0x7f)
	used := 1
	for c&0x80 != 0 {
		if used >= len(buf) {
			return 0, 0, fmt.Errorf("objfmt: OFS_DELTA offset overruns buffer: %w", ErrTruncated)
		}
		off++
		if off >= 1<<56 {
			return 0, 0, fmt.Errorf("objfmt: OFS_DELTA offset overflow: %w", ErrCorrupt)
		}
		c = buf[used]
		off = (off << 7) | int64(c&0x7f)
		used++
	}
	return off, used, nil
}

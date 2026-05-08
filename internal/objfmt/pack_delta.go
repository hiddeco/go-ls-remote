package objfmt

import (
	"compress/zlib"
	"errors"
	"fmt"
	"io"
)

// ReadDeltaHeader decompresses the leading bytes of the delta payload
// at bodyAt and returns the encoded source (base) and target
// (resolved) sizes — the two varints that head every delta per
// `delta.h::get_delta_hdr_size` (lines 85-102).
//
// The function only inflates enough bytes to cover both varints; it
// does not read or apply the delta opcodes that follow. Callers
// resolving the delta proper must re-open a zlib reader at bodyAt.
//
// Note: this varint encoding is little-endian without the
// "+1 per continuation byte" quirk that OFS_DELTA's offset varint
// uses (see [Pack.ReadHeader]).
func (p *Pack) ReadDeltaHeader(bodyAt int64) (sourceSize, targetSize int64, err error) {
	if bodyAt < 0 || bodyAt >= int64(p.r.Len()) {
		return 0, 0, fmt.Errorf("objfmt: delta body offset %d out of range", bodyAt)
	}

	// 64 bytes is comfortably more than the worst case: two
	// 10-byte varints plus a small head margin. The actual delta
	// payload may be shorter — for tiny deltas the inflated stream
	// can fall short of 64 bytes — so a short read is not an error.
	const peek = 64
	section := io.NewSectionReader(p.r, bodyAt, int64(p.r.Len())-bodyAt)
	zr, err := zlib.NewReader(section)
	if err != nil {
		return 0, 0, fmt.Errorf("objfmt: delta zlib init: %w", err)
	}
	defer zr.Close()

	buf := make([]byte, peek)
	n, err := io.ReadFull(zr, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return 0, 0, fmt.Errorf("objfmt: read delta header: %w", err)
	}
	buf = buf[:n]

	src, advance, err := readDeltaVarint(buf)
	if err != nil {
		return 0, 0, fmt.Errorf("objfmt: parse source size: %w", err)
	}
	tgt, _, err := readDeltaVarint(buf[advance:])
	if err != nil {
		return 0, 0, fmt.Errorf("objfmt: parse target size: %w", err)
	}
	return src, tgt, nil
}

// readDeltaVarint decodes the little-endian 7-bit varint at the head
// of buf, returning the value and the number of bytes consumed.
//
// The encoding mirrors `delta.h::get_delta_hdr_size`: each byte
// contributes 7 bits, low byte first, with the high bit signalling a
// continuation. Distinct from the OFS_DELTA offset varint, which is
// big-endian and adds one per continuation byte.
func readDeltaVarint(buf []byte) (int64, int, error) {
	var (
		v     int64
		shift uint
		used  int
	)
	for used < len(buf) {
		c := buf[used]
		v |= int64(c&0x7f) << shift
		used++
		if c&0x80 == 0 {
			return v, used, nil
		}
		shift += 7
		if shift >= 64 {
			return 0, 0, errors.New("objfmt: delta varint overflow")
		}
	}
	if used == 0 {
		return 0, 0, errors.New("objfmt: empty delta varint")
	}
	return 0, 0, errors.New("objfmt: delta varint truncated")
}

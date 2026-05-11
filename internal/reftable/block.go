package reftable

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
)

// Sentinel errors returned by the block parser. Callers match against
// these with [errors.Is]; the wrapping `fmt.Errorf("...: %w", ..., sentinel)`
// adds the offending value (declared block_len, restart_count, type
// byte) for diagnostics.
var (
	// ErrTruncatedBlock is returned when a block's declared block_len
	// or restart_count cannot be reconciled with the input buffer:
	// the buffer is shorter than the declared block_len, the restart
	// table does not fit inside block_len, the restart table is empty
	// (the spec forbids that), or a restart_offset value sits before
	// the first ref block's first ref_record (firstByteOffset
	// underflow).
	ErrTruncatedBlock = errors.New("reftable: truncated block")

	// ErrBadBlockType is returned when a block's first byte is not one
	// of the four block types defined by the spec ('r', 'i', 'o', 'g').
	ErrBadBlockType = errors.New("reftable: unknown block type")

	// ErrLogBlockUnsupported is returned when a block's first byte is
	// 'g' (log block). Log blocks zlib-deflate their bodies; this
	// package does not yet decode them.
	ErrLogBlockUnsupported = errors.New("reftable: log blocks not supported")
)

// reftable.adoc §"Ref block format" / §"Index block format" — the
// first byte of a block carries one of these four type tags.
const (
	blockTypeRef   byte = 'r'
	blockTypeIndex byte = 'i'
	blockTypeObj   byte = 'o'
	blockTypeLog   byte = 'g'
)

// blockHeader carries the per-block fields decoded from a reftable
// block, regardless of block_type.
type blockHeader struct {
	blockType byte // 'r', 'i', 'o', 'g'
	// blockLen is the number of bytes from the block start through
	// restart_count, excluding optional padding. For the first ref
	// block in a file the leading 24 (v1) or 28 (v2) bytes are the
	// file header, included in block_len per spec.
	blockLen     uint32
	restartCount uint16 // number of restart_offset entries (1..65535)
}

// block is a parsed view of a reftable ref/index/obj block: the
// decoded header plus the metadata needed to lazily resolve the
// restart-offset table. The caller hands in the block's bytes;
// [block] does not own or copy them.
//
// The on-disk restart_offset table sits at the tail of the block (as
// `restartCount` uint24 entries followed by the uint16 restart_count
// itself). [parseBlock] validates the table fits inside `blockLen` but
// does not decode the entries up front — the [block.restart] accessor
// resolves one entry at a time, which is allocation-free and matches
// the access pattern of every consumer:
//
//   - [Reader.iterAllRefs] / [Reader.FindRef]'s linear scan never
//     touches the restart table at all.
//   - [block.seekRestart] (used by [descendIndex] in seek.go)
//     binary-searches the table with O(log restartCount) accesses.
//
// Eager decoding was the dominant per-block allocation at scale; see
// `reftable/block.c::block_reader_init` for the canonical (also lazy)
// shape.
type block struct {
	header blockHeader

	// bytes is the block payload sliced to exactly blockLen. Padding
	// after the block (when the file aligns blocks to block_size) is
	// excluded. For the first ref block in a file, bytes[0:firstByteOffset]
	// is the file header and a record iterator must start at
	// bytes[firstByteOffset+4] (skipping block_type + block_len).
	bytes []byte

	// firstByteOffset is the block-frame shift carried through from
	// [parseBlock]. It is 24 (v1) or 28 (v2) for the first ref block in
	// a file (whose on-disk restart_offset values are file-absolute)
	// and 0 for every other block. [block.restart] subtracts it to
	// rebase a decoded restart_offset into the block-local frame.
	firstByteOffset uint32

	// tableStart is the offset within bytes where the restart_offset
	// table begins (3 bytes per entry, `restartCount` entries, then
	// the uint16 restart_count at bytes[blockLen-2:]). Stored at parse
	// time so [block.restart] avoids re-deriving it on each access.
	tableStart uint32
}

// parseBlock decodes a block header and its restart-point table from
// the leading bytes of buf. The block's first byte
// (buf[firstByteOffset]) carries the block_type.
//
// firstByteOffset is the on-disk position of the block's first byte
// expressed in the same frame as the on-disk restart_offset values.
// For every block except the first ref block in a file, the two
// frames agree and firstByteOffset is 0. For the first ref block,
// reftable.adoc §"Ref block format" states "all restart_offset in the
// first block are relative to the start of the file (position 0), and
// include the file header"; the caller passes firstByteOffset=24 (the
// v1 header size) or 28 (v2) so parseBlock can rebase the table to
// the block-local frame.
//
// parseBlock returns:
//   - ErrBadBlockType for unknown type bytes,
//   - ErrLogBlockUnsupported for log blocks ('g'); their body is
//     zlib-deflated and out of scope here,
//   - ErrTruncatedBlock when buf is shorter than the declared
//     block_len, when the restart table does not fit inside block_len,
//     when restart_count is zero (forbidden by the spec), or when a
//     restart_offset would underflow firstByteOffset.
//
// The returned block's bytes field aliases buf; callers must not
// mutate the underlying storage while the block is in use.
func parseBlock(buf []byte, firstByteOffset uint32) (block, error) {
	// Need at least the 4-byte header (block_type + uint24 block_len)
	// before we can read block_len.
	if uint32(len(buf)) < firstByteOffset+4 {
		return block{}, fmt.Errorf("reftable: have %d bytes, need %d for block header: %w", len(buf), firstByteOffset+4, ErrTruncatedBlock)
	}

	// reftable.adoc §"Ref block format" — block_type is one byte,
	// followed by uint24 block_len.
	blockType := buf[firstByteOffset]
	switch blockType {
	case blockTypeRef, blockTypeIndex, blockTypeObj:
	case blockTypeLog:
		return block{}, fmt.Errorf("reftable: block type %q: %w", blockType, ErrLogBlockUnsupported)
	default:
		return block{}, fmt.Errorf("reftable: block type %#02x: %w", blockType, ErrBadBlockType)
	}

	blockLen := be24(buf[firstByteOffset+1 : firstByteOffset+4])
	if uint32(len(buf)) < blockLen {
		return block{}, fmt.Errorf("reftable: block_len %d exceeds buffer %d: %w", blockLen, len(buf), ErrTruncatedBlock)
	}

	// Smallest legal block_len: 4-byte header + 3-byte restart_offset
	// + 2-byte restart_count = 9 bytes (in the block-local frame; for
	// the first ref block the firstByteOffset preamble is folded in).
	// Guarding here keeps the bytes[blockLen-2:] read below from
	// underflowing on hostile inputs.
	if blockLen < firstByteOffset+9 {
		return block{}, fmt.Errorf("reftable: block_len %d too small for restart table: %w", blockLen, ErrTruncatedBlock)
	}
	bytes := buf[:blockLen]

	// reftable.adoc §"Ref block format" — restart_count is the last
	// two bytes of the block payload; the restart_offset table of
	// uint24 entries precedes it.
	restartCount := binary.BigEndian.Uint16(bytes[blockLen-2:])
	if restartCount == 0 {
		return block{}, fmt.Errorf("reftable: empty restart table: %w", ErrTruncatedBlock)
	}

	tableStart := int64(blockLen) - 2 - 3*int64(restartCount)
	// The restart table must sit after the 4-byte block header
	// (block_type + block_len) — at minimum tableStart >= 4 in the
	// block-local frame. Since firstByteOffset is folded into the
	// block_len for the first ref block, the comparison is against
	// firstByteOffset+4.
	if tableStart < int64(firstByteOffset)+4 {
		return block{}, fmt.Errorf("reftable: restart table of %d entries does not fit in block_len %d: %w", restartCount, blockLen, ErrTruncatedBlock)
	}

	// Per-entry validation runs at parse time so [block.restart] can
	// trust the table; rebasing happens at access time. The smallest
	// legal restart_offset in the block-local frame is firstByteOffset
	// (the on-disk value); anything below would underflow the subtract
	// done in [block.restart].
	for i := range int(restartCount) {
		off := be24(bytes[tableStart+3*int64(i) : tableStart+3*int64(i)+3])
		if off < firstByteOffset {
			return block{}, fmt.Errorf("reftable: restart_offset %d below firstByteOffset %d: %w", off, firstByteOffset, ErrTruncatedBlock)
		}
	}

	return block{
		header: blockHeader{
			blockType:    blockType,
			blockLen:     blockLen,
			restartCount: restartCount,
		},
		bytes:           bytes,
		firstByteOffset: firstByteOffset,
		tableStart:      uint32(tableStart),
	}, nil
}

// restart returns the block-local offset of the i-th restart record.
// The caller is responsible for keeping i in [0, restartCount); the
// restart table sits at the tail of the block (3 bytes per entry) and
// [parseBlock] has already validated that every entry rebases without
// underflow.
//
// Decoded lazily to keep [parseBlock] allocation-free at scale: a
// full-table walk happens only in [block.seekRestart], which performs
// O(log restartCount) accesses; the steady-state record walker in
// [Reader.iterAllRefs] never reaches this path.
func (b *block) restart(i int) uint32 {
	off := uint32(b.tableStart) + 3*uint32(i)
	return be24(b.bytes[off:off+3]) - b.firstByteOffset
}

// seekRestart returns the index of the largest restart point whose
// record key compares <= probe via cmp, or -1 if probe sorts before
// every restart point.
//
// cmp(i) returns -1, 0, or +1 if the record at [block.restart](i)
// sorts before, equal to, or after the probe key. It is the bridge to
// record-level decoding: the caller decodes the record's suffix
// (which equals the full key for restart records, since prefix_length
// is 0 by spec) and compares it to probe.
//
// Mirrors `reftable/block.c::block_iter_seek_key`: we binary-search
// for the first restart strictly greater than probe and back up by
// one to find the largest restart <= probe.
func (b *block) seekRestart(cmp func(restartIdx int) int) int {
	idx := sort.Search(int(b.header.restartCount), func(i int) bool {
		return cmp(i) > 0
	})
	return idx - 1
}

// be24 decodes a 3-byte big-endian unsigned integer.
//
// reftable.adoc encodes block_len and each restart_offset as uint24;
// encoding/binary has no Uint24 helper. [parseHeader] spells the same
// math out inline because it only needs to read one such value;
// [parseBlock] reads many in a hot loop, so factoring the helper here
// keeps the loop tight.
func be24(buf []byte) uint32 {
	return uint32(buf[0])<<16 | uint32(buf[1])<<8 | uint32(buf[2])
}

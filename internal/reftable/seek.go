package reftable

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

// blockProbeCounter records how many block reads a seek performs.
// Tests use it to assert the index path was taken (1 + log(blocks)
// block reads) versus the linear-walk fallback path (O(blocks) block
// reads). Only block descents count: the footer parse that reveals
// `ref_index_position` is once-per-Open and outside this metric.
type blockProbeCounter struct {
	indexBlocks int // number of index blocks descended
	refBlocks   int // number of ref blocks read
}

// readRefIndexPosition pulls `ref_index_position` from the file
// footer. The footer's section-position fields begin immediately after
// the header copy — at offset 24 for v1, 28 for v2. A zero return
// signals "no ref index", per reftable.adoc §"Footer".
//
// The caller must have already validated the footer with
// [verifyTrailer]; readRefIndexPosition does not re-check the CRC and
// does not bounds-check the file length beyond what [header.size] /
// [header.footerSize] imply.
func readRefIndexPosition(file []byte, h header) uint64 {
	footer := file[len(file)-h.footerSize():]
	return binary.BigEndian.Uint64(footer[h.size():])
}

// seekToLeaf walks from a reftable file's `ref_index_position` down
// through any number of index-block levels until it lands on the leaf
// ref block that should contain probe (or the closest leaf if probe
// sorts before/after every entry).
//
// indexRoot is the absolute file offset stored in the footer. When it
// is zero the caller asked for a fileless seek: seekToLeaf falls back
// to a linear scan of ref blocks beginning at the file header's first
// ref block. (Canonical Git does the same; reftable.adoc §"Ref index"
// notes that small files may omit the ref index, and §"Block
// alignment" requires multi-block unaligned files to carry one.)
//
// The returned slice is a window onto file aliasing exactly one
// block's worth of bytes (its declared block_len), and
// leafFirstByteOffset is 24 for the very first block of the file
// (which shares its frame with the v1 file header) or 28 for v2 — the
// caller threads it back into [parseBlock] so restart_offset values
// rebase correctly. Every other block returns leafFirstByteOffset = 0.
//
// Errors:
//   - [ErrTruncatedBlock] / [ErrBadBlockType] / [ErrLogBlockUnsupported]
//     propagate from [parseBlock].
//   - [ErrTruncatedRecord] / [ErrVarintOverflow] propagate from the
//     in-block index-record scan when a key cannot be decoded.
//   - A bare error wrapping ErrTruncatedBlock is returned when an
//     index_record's `block_position` falls outside file or when an
//     index level loops back on itself.
//
// counter, if non-nil, is incremented as walks happen. Callers
// constructing a Reader pass their own counter only in tests; a nil
// pointer skips the bookkeeping.
//
// Mirrors the descent in `reftable/table.c::table_iter_seek_indexed`
// and the per-block seek in `reftable/block.c::block_iter_seek_key`.
func seekToLeaf(file []byte, h header, indexRoot uint64, probe []byte, counter *blockProbeCounter) ([]byte, uint32, error) {
	if indexRoot == 0 {
		return seekLinear(file, h, probe, counter)
	}
	return seekIndexed(file, h, indexRoot, probe, counter)
}

// seekIndexed walks from indexRoot — an `i` block — through any
// further index levels and returns the leaf `r` block whose first key
// is the smallest known key ≥ probe. See [seekToLeaf] for the public
// contract.
func seekIndexed(file []byte, h header, indexRoot uint64, probe []byte, counter *blockProbeCounter) ([]byte, uint32, error) {
	// Cap the descent at a sensible bound. reftable.adoc §"Ref index"
	// allows multi-level indexes, but a healthy file converges to a
	// leaf in a handful of steps. The cap stops a malformed file from
	// looping us forever; we lift it well above any plausible depth.
	const maxDepth = 64

	pos := indexRoot
	for range maxDepth {
		if pos >= uint64(len(file)) {
			return nil, 0, fmt.Errorf("reftable: block_position %d outside file (%d bytes): %w", pos, len(file), ErrTruncatedBlock)
		}

		// The first ref block in a file shares its frame with the file
		// header (reftable.adoc §"Ref block format"); when an index
		// points back at file offset 0 we must hand parseBlock the
		// firstByteOffset shift so its restart-table rebase still
		// works. Every other block lives at its own offset and uses
		// firstByteOffset = 0.
		var firstByteOffset uint32
		var slice []byte
		if pos == 0 {
			firstByteOffset = uint32(h.size())
			slice = file
		} else {
			slice = file[pos:]
		}

		blk, err := parseBlock(slice, firstByteOffset)
		if err != nil {
			return nil, 0, fmt.Errorf("reftable: parse block at %d: %w", pos, err)
		}

		switch blk.header.blockType {
		case blockTypeRef:
			if counter != nil {
				counter.refBlocks++
			}
			return blk.bytes, firstByteOffset, nil
		case blockTypeIndex:
			if counter != nil {
				counter.indexBlocks++
			}
			next, err := descendIndex(&blk, probe)
			if err != nil {
				return nil, 0, fmt.Errorf("reftable: index at %d: %w", pos, err)
			}
			if next == pos {
				return nil, 0, fmt.Errorf("reftable: index at %d points to itself: %w", pos, ErrTruncatedBlock)
			}
			pos = next
		default:
			// blockTypeObj and blockTypeLog never appear under the ref
			// index; reject defensively.
			return nil, 0, fmt.Errorf("reftable: unexpected block type %q under ref index: %w", blk.header.blockType, ErrBadBlockType)
		}
	}
	return nil, 0, fmt.Errorf("reftable: ref index exceeds %d levels: %w", maxDepth, ErrTruncatedBlock)
}

// descendIndex picks the next-level block_position to read from an
// index block. It locates the smallest index_record whose key is ≥
// probe (using the restart table for an O(log) jump-in followed by a
// short linear scan, exactly like
// `reftable/block.c::block_iter_seek_key`). If probe sorts after
// every index_record, descendIndex falls through to the last record's
// block_position so callers always get a leaf back.
func descendIndex(blk *block, probe []byte) (uint64, error) {
	// Records live in bytes[startOff:restartOff). startOff is 4 (block
	// header) for every block but the first ref block in the file —
	// which is never an index block, so we do not need that branch
	// here.
	startOff := uint32(4)
	restartOff := blk.header.blockLen - 2 - 3*uint32(blk.header.restartCount)

	// Binary-search restart points for the largest restart key ≤ probe.
	// Restart records carry prefix_length=0, so each cmp() call decodes
	// a self-contained key; no running prevKey is needed.
	idx := blk.seekRestart(func(i int) int {
		off := blk.restartOffsets[i]
		key, _, _, err := decodeKey(blk.bytes[off:restartOff], nil, nil)
		if err != nil {
			// Returning +1 makes seekRestart skip this restart; the
			// chosen earlier restart still anchors the linear scan,
			// which re-reads the same bytes via decodeKey and returns
			// the format error to the caller.
			return +1
		}
		return bytes.Compare(key, probe)
	})

	// Linear scan starting at the chosen restart (or at startOff when
	// probe sorts before the first restart). Advance until the first
	// record with key ≥ probe; back up by one record to mirror
	// `block_iter_seek_key`, then read its block_position.
	scanFrom := startOff
	if idx >= 0 {
		scanFrom = blk.restartOffsets[idx]
	}

	cur := scanFrom
	var prevKey []byte
	var lastBlockPos uint64 // descent target if probe sorts after every record
	haveLast := false

	for cur < restartOff {
		key, _, n, err := decodeKey(blk.bytes[cur:restartOff], prevKey, nil)
		if err != nil {
			return 0, fmt.Errorf("reftable: decode index key at offset %d: %w", cur, err)
		}
		// After the (prefix-compressed) key comes varint(block_position)
		// for index records (reftable.adoc §"index record").
		blockPos, m, err := decodeVarint(blk.bytes[cur+uint32(n) : restartOff])
		if err != nil {
			return 0, fmt.Errorf("reftable: decode index block_position at offset %d: %w", cur, err)
		}

		cmp := bytes.Compare(key, probe)
		if cmp >= 0 {
			return blockPos, nil
		}
		lastBlockPos = blockPos
		haveLast = true

		prevKey = key
		cur += uint32(n) + uint32(m)
	}

	if haveLast {
		// Probe sorts after every index_record at this level. Descend
		// via the last record so the caller still gets a leaf block.
		return lastBlockPos, nil
	}
	// An empty record range under a non-empty restart_count would be a
	// malformed block; the caller is expected to surface this.
	return 0, fmt.Errorf("reftable: index block has no records: %w", ErrTruncatedBlock)
}

// seekLinear returns the leaf ref block for files without a ref index.
// It walks ref blocks left-to-right until the next block's first key
// sorts after probe, then returns the previous block. For files with a
// single ref block (the common case under "no index") the loop returns
// after one iteration.
//
// Reftable's spec only allows a missing ref index when the file has
// at most one ref block (very small files, or one-block aligned
// files); this fallback nonetheless walks all leading ref blocks for
// robustness.
//
// The block-walk shape (first-block firstByteOffset, blockSize round-up,
// stop-on-non-ref) matches [Reader.iterAllRefs]; see that function's
// note on the deliberate non-factoring.
func seekLinear(file []byte, h header, probe []byte, counter *blockProbeCounter) ([]byte, uint32, error) {
	headerSize := uint32(h.size())
	pos := uint32(0)
	first := true
	var lastBytes []byte
	var lastFirstByte uint32

	for pos < uint32(len(file)) {
		firstByteOffset := uint32(0)
		readPos := pos
		if first {
			firstByteOffset = headerSize
		}

		// `parseBlock` expects the slice to begin at position 0 in the
		// caller's frame; for the first block we hand over the file
		// from byte 0 and let firstByteOffset point at the block-type
		// byte. For every later block we slice forward to its start.
		var slice []byte
		if first {
			slice = file
		} else {
			slice = file[readPos:]
		}

		// A block of an unknown type ('o', 'g') marks the end of the
		// ref-block run; canonical Git stops the linear scan there.
		// Peek at the type byte before parsing so we don't surface a
		// misleading log/obj-block error.
		if uint32(len(slice)) <= firstByteOffset {
			break
		}
		typeByte := slice[firstByteOffset]
		if typeByte != blockTypeRef {
			break
		}

		blk, err := parseBlock(slice, firstByteOffset)
		if err != nil {
			return nil, 0, fmt.Errorf("reftable: parse ref block at %d: %w", readPos, err)
		}
		if counter != nil {
			counter.refBlocks++
		}

		// On the first iteration, or when the new block's first record
		// sorts ≤ probe, this block is a candidate. We compare against
		// the new block's first key to decide whether to keep walking.
		firstKeyOff := firstByteOffset + 4
		firstKey, _, _, err := decodeKey(blk.bytes[firstKeyOff:], nil, nil)
		if err != nil {
			return nil, 0, fmt.Errorf("reftable: decode first key in block at %d: %w", readPos, err)
		}

		if !first && bytes.Compare(firstKey, probe) > 0 {
			// The probe lives no later than the previous block.
			return lastBytes, lastFirstByte, nil
		}

		lastBytes = blk.bytes
		lastFirstByte = firstByteOffset
		first = false

		// Advance to the next block. block_len excludes padding; for
		// aligned files we round up to the file's block_size.
		nextPos := readPos + blk.header.blockLen
		if h.blockSize > 0 {
			nextPos = roundUp(nextPos, h.blockSize)
		}
		if nextPos <= readPos {
			return nil, 0, fmt.Errorf("reftable: block at %d does not advance: %w", readPos, ErrTruncatedBlock)
		}
		pos = nextPos
	}

	if lastBytes == nil {
		return nil, 0, errors.New("reftable: file has no ref blocks")
	}
	return lastBytes, lastFirstByte, nil
}

// roundUp rounds v up to the next multiple of m. m must be > 0.
func roundUp(v, m uint32) uint32 {
	return ((v + m - 1) / m) * m
}

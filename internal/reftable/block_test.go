package reftable

import (
	"encoding/binary"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// putBE24 writes v as a 3-byte big-endian uint into buf[0:3].
func putBE24(buf []byte, v uint32) {
	buf[0] = byte(v >> 16)
	buf[1] = byte(v >> 8)
	buf[2] = byte(v)
}

// buildBlock returns blockLen bytes containing a block of the given type
// with restart_offset entries equal to restarts. Bytes between offset 4
// and the start of the restart table are zero-filled (the test only
// inspects header and restart-table fields).
func buildBlock(blockType byte, blockLen int, restarts []uint32) []byte {
	if blockLen < 4+3*len(restarts)+2 {
		panic("buildBlock: blockLen too small for header + restart table")
	}
	buf := make([]byte, blockLen)
	buf[0] = blockType
	putBE24(buf[1:4], uint32(blockLen))

	tableStart := blockLen - 2 - 3*len(restarts)
	for i, off := range restarts {
		putBE24(buf[tableStart+3*i:tableStart+3*i+3], off)
	}
	binary.BigEndian.PutUint16(buf[blockLen-2:], uint16(len(restarts)))
	return buf
}

// buildFirstRefBlock returns blockLen bytes laid out as the first ref
// block of a file: a firstByteOffset-byte preamble (the file header,
// zero-filled here) followed by the block header at offset
// firstByteOffset. The stored block_len covers the preamble too, per
// [reftable.adoc § Ref block format]. restarts are written verbatim
// (i.e. relative to position 0, including the preamble).
//
// [reftable.adoc § Ref block format]: https://github.com/git/git/blob/v2.54.0/Documentation/technical/reftable.adoc#ref-block-format
func buildFirstRefBlock(blockLen, firstByteOffset int, restarts []uint32) []byte {
	if blockLen < firstByteOffset+4+3*len(restarts)+2 {
		panic("buildFirstRefBlock: blockLen too small")
	}
	buf := make([]byte, blockLen)
	buf[firstByteOffset] = 'r'
	putBE24(buf[firstByteOffset+1:firstByteOffset+4], uint32(blockLen))

	tableStart := blockLen - 2 - 3*len(restarts)
	for i, off := range restarts {
		putBE24(buf[tableStart+3*i:tableStart+3*i+3], off)
	}
	binary.BigEndian.PutUint16(buf[blockLen-2:], uint16(len(restarts)))
	return buf
}

func Test_parseBlock(t *testing.T) {
	t.Run("ref_block_basic", func(t *testing.T) {
		buf := buildBlock('r', 32, []uint32{4, 16})
		b, err := parseBlock(buf, 0)
		require.NoError(t, err)
		assert.Equal(t, byte('r'), b.header.blockType)
		assert.Equal(t, uint32(32), b.header.blockLen)
		assert.Equal(t, uint16(2), b.header.restartCount)
		assert.Equal(t, uint32(4), b.restart(0))
		assert.Equal(t, uint32(16), b.restart(1))
		// bytes is a view sliced to blockLen.
		assert.Len(t, b.bytes, 32)
	})

	t.Run("index_block", func(t *testing.T) {
		buf := buildBlock('i', 24, []uint32{4})
		b, err := parseBlock(buf, 0)
		require.NoError(t, err)
		assert.Equal(t, byte('i'), b.header.blockType)
		assert.Equal(t, uint32(24), b.header.blockLen)
		assert.Equal(t, uint16(1), b.header.restartCount)
		assert.Equal(t, uint32(4), b.restart(0))
	})

	t.Run("obj_block", func(t *testing.T) {
		buf := buildBlock('o', 24, []uint32{4})
		b, err := parseBlock(buf, 0)
		require.NoError(t, err)
		assert.Equal(t, byte('o'), b.header.blockType)
	})

	t.Run("buf_larger_than_block_len", func(t *testing.T) {
		// Padding zeros after blockLen should be ignored (aligned files
		// pad each block with zeros up to the file's block size).
		buf := buildBlock('r', 32, []uint32{4, 16})
		padded := append(buf, make([]byte, 32)...)
		b, err := parseBlock(padded, 0)
		require.NoError(t, err)
		assert.Equal(t, uint32(32), b.header.blockLen)
		assert.Len(t, b.bytes, 32)
	})

	t.Run("log_block_rejected", func(t *testing.T) {
		buf := buildBlock('g', 24, []uint32{4})
		_, err := parseBlock(buf, 0)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrLogBlockUnsupported), "want ErrLogBlockUnsupported, got %v", err)
	})

	t.Run("bad_type_rejected", func(t *testing.T) {
		buf := buildBlock('X', 24, []uint32{4})
		_, err := parseBlock(buf, 0)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrBadBlockType), "want ErrBadBlockType, got %v", err)
	})

	t.Run("truncated_too_short_for_header", func(t *testing.T) {
		// Only 3 bytes — cannot even read block_type + block_len.
		_, err := parseBlock([]byte{'r', 0, 0}, 0)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrTruncatedBlock), "want ErrTruncatedBlock, got %v", err)
	})

	t.Run("truncated_block_len_exceeds_buf", func(t *testing.T) {
		// Header claims blockLen=32, but buf is only 16 bytes.
		buf := make([]byte, 16)
		buf[0] = 'r'
		putBE24(buf[1:4], 32)
		_, err := parseBlock(buf, 0)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrTruncatedBlock), "want ErrTruncatedBlock, got %v", err)
	})

	t.Run("truncated_no_room_for_restart_table", func(t *testing.T) {
		// blockLen too small to fit header + restart_count + claimed
		// restart_offset entries.
		buf := make([]byte, 8)
		buf[0] = 'r'
		putBE24(buf[1:4], 8)
		// restart_count = 5 → would need 4+15+2 = 21 bytes.
		binary.BigEndian.PutUint16(buf[6:8], 5)
		_, err := parseBlock(buf, 0)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrTruncatedBlock), "want ErrTruncatedBlock, got %v", err)
	})

	t.Run("empty_restart_table", func(t *testing.T) {
		// [reftable.adoc § Ref block format]: "the restart_offset list,
		// which must not be empty".
		//
		// [reftable.adoc § Ref block format]: https://github.com/git/git/blob/v2.54.0/Documentation/technical/reftable.adoc#ref-block-format
		buf := make([]byte, 8)
		buf[0] = 'r'
		putBE24(buf[1:4], 8)
		binary.BigEndian.PutUint16(buf[6:8], 0)
		_, err := parseBlock(buf, 0)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrTruncatedBlock), "want ErrTruncatedBlock, got %v", err)
	})

	t.Run("first_ref_block_offset_subtraction", func(t *testing.T) {
		// First ref block carries restart_offsets relative to position
		// 0 (i.e. they include the 24-byte file header).
		// firstByteOffset=24 → on-disk offsets {28, 40} become
		// block-relative {4, 16}.
		//
		// [reftable.adoc § Ref block format]: "all restart_offset in the
		// first block are relative to the start of the file (position 0),
		// and include the file header. This forces the first
		// restart_offset to be `28`."
		// So we lay out a buffer with a 24-byte file-header preamble
		// followed by the block payload; the block's stored block_len
		// covers the preamble too.
		//
		// [reftable.adoc § Ref block format]: https://github.com/git/git/blob/v2.54.0/Documentation/technical/reftable.adoc#ref-block-format
		buf := buildFirstRefBlock(64, 24, []uint32{28, 40})
		b, err := parseBlock(buf, 24)
		require.NoError(t, err)
		assert.Equal(t, uint32(64), b.header.blockLen)
		assert.Equal(t, uint32(4), b.restart(0))
		assert.Equal(t, uint32(16), b.restart(1))
	})

	t.Run("first_ref_block_offset_below_first_byte_rejected", func(t *testing.T) {
		// A restart_offset smaller than firstByteOffset would
		// underflow the subtraction. Reject it as a malformed block.
		buf := buildFirstRefBlock(64, 24, []uint32{12, 28})
		_, err := parseBlock(buf, 24)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrTruncatedBlock), "want ErrTruncatedBlock, got %v", err)
	})
}

func Test_block_seekRestart(t *testing.T) {
	// Build a block whose restart-table layout we control. The cmp
	// function below maps each restart index to a synthetic key
	// without touching the bytes, so we can drive seekRestart without
	// depending on the record decoder.
	keys := []string{"a", "c", "e"}
	buf := buildBlock('r', 32, []uint32{4, 8, 12})
	b, err := parseBlock(buf, 0)
	require.NoError(t, err)
	require.Equal(t, uint16(3), b.header.restartCount)

	cmpFor := func(probe string) func(int) int {
		return func(i int) int {
			switch {
			case keys[i] < probe:
				return -1
			case keys[i] > probe:
				return +1
			default:
				return 0
			}
		}
	}

	t.Run("finds_largest_le", func(t *testing.T) {
		cases := []struct {
			probe string
			want  int
		}{
			{"a", 0}, // exact hit on first
			{"b", 0}, // between first and second → first
			{"c", 1}, // exact hit on second
			{"d", 1}, // between second and third → second
			{"e", 2}, // exact hit on third
			{"f", 2}, // after last → last
		}
		for _, tc := range cases {
			t.Run(tc.probe, func(t *testing.T) {
				got := b.seekRestart(cmpFor(tc.probe))
				assert.Equal(t, tc.want, got)
			})
		}
	})

	t.Run("probe_before_first_returns_minus_one", func(t *testing.T) {
		// Probe sorts before everything in the table.
		got := b.seekRestart(cmpFor("0"))
		assert.Equal(t, -1, got)
	})
}

package objfmt

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
)

// Midx is a `multi-pack-index` reader: a single chunk-based file that
// indexes objects across many `.pack`/`.idx` pairs in one
// `objects/pack/` directory.
//
// The format is documented in
// `Documentation/technical/multi-pack-index.adoc` and the canonical
// reader lives in `midx.c::load_multi_pack_index_one`. A 12-byte header
// is followed by a chunk lookup table; required chunks are `PNAM`
// (pack-name list), `OIDF` (256-entry fan-out), `OIDL` (sorted OID
// table), and `OOFF` (per-object pack id and offset). Optional chunks
// — `LOFF` (large-offset overflow), `RIDX` (reverse index), `BTMP`
// (bitmap pack metadata), and `BASE` (incremental-chain base) — are
// recognised in the table but only `LOFF` is consumed by this reader.
// Reachability bitmaps and incremental chains are out of scope.
//
// The file is read into memory at [OpenMidx] time and every lookup is
// then arithmetic over the in-memory slice; midx files for typical
// multi-gigabyte repositories are well under 10 MiB so the simpler
// model wins over mmap.
type Midx struct {
	path      string
	algo      Algo
	ver       uint32
	count     uint32
	numPacks  uint32
	data      []byte
	chunks    map[chunkID]chunkExtent
	packNames []string
}

// chunkID is the four-byte ASCII tag that identifies a chunk in the
// midx (and commit-graph) chunk-table format.
type chunkID [4]byte

// chunkExtent locates a chunk within [Midx.data]: an absolute byte
// offset plus a length in bytes. Both are derived from the chunk
// lookup table at parse time and assumed in-bounds by every reader.
type chunkExtent struct {
	off int64
	len int64
}

// Multi-pack-index file layout per `midx.h` (`MIDX_BYTE_*`) and
// `midx-write.c` (`write_midx_header`):
//
//	'MIDX'              4 bytes magic
//	uint8 version       1 (v1) or 2 (v2; reserves a hash byte)
//	uint8 hash_version  1 = SHA-1, 2 = SHA-256 (matches OID hash table)
//	uint8 num_chunks    number of chunks in the lookup table
//	uint8 unused        always written as 0
//	uint32 num_packs    number of packs referenced by PNAM (BE)
//	[chunk lookup table: (num_chunks + 1) * (4-byte ID + 8-byte offset)]
//	[chunk bodies, in declaration order]
//	hashLen-byte trailer over every preceding byte
//
// Canonical Git surfaces incremental "chained" midx files via a
// separate `multi-pack-index-chain` index in a sibling
// `multi-pack-index.d/` directory rather than a header field, so this
// reader rejects byte 7 ≠ 0 defensively (mirrors the explicit
// zero-write in `midx-write.c::write_midx_header`) and treats only
// standalone midx files as in scope.
//
// The chunk lookup table's terminator entry has an all-zero ID; its
// 8-byte offset is one past the last byte of the final chunk's body
// and so doubles as the final chunk's length.

const (
	midxHeaderSize    = 12
	midxChunkTOCEntry = 4 + 8

	// midxChunkAlignment is the byte alignment every chunk's start
	// offset must satisfy in the chunk-format file family. Mirrors
	// `MIDX_CHUNK_ALIGNMENT` in `midx.h` and the `expected_alignment`
	// argument passed to `read_table_of_contents` in
	// `chunk-format.c:127-130`.
	midxChunkAlignment = 4
)

// midxMagic is the four-byte ASCII signature that introduces every
// multi-pack-index file (`MIDX_SIGNATURE` in `midx.h`).
var midxMagic = [4]byte{'M', 'I', 'D', 'X'}

// Chunk IDs from `midx.h`. Only the four required chunks plus `LOFF`
// are consumed by this reader; the others are ignored if present.
var (
	chunkPNAM = chunkID{'P', 'N', 'A', 'M'}
	chunkOIDF = chunkID{'O', 'I', 'D', 'F'}
	chunkOIDL = chunkID{'O', 'I', 'D', 'L'}
	chunkOOFF = chunkID{'O', 'O', 'F', 'F'}
	chunkLOFF = chunkID{'L', 'O', 'F', 'F'}
)

// OpenMidx reads path into memory, validates the multi-pack-index
// header and chunk table, and returns a [Midx] ready for lookups.
//
// algo must match the file's `hash_version` byte: SHA1 corresponds to
// hash version 1, SHA256 to hash version 2. Mismatches are rejected
// rather than tolerated because every downstream lookup interprets the
// OID table at the algo's stride.
//
// Chained ("incremental") midx files — files with a non-zero
// `num_base` byte — are out of scope and rejected here. Bitmap-related
// chunks (`BTMP`, `RIDX`) are tolerated when present but never
// consulted.
func OpenMidx(path string, algo Algo) (*Midx, error) {
	if algo.Size() == 0 {
		return nil, fmt.Errorf("objfmt: unknown algo %v: %w", algo, ErrUnsupportedAlgo)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) < midxHeaderSize+algo.Size() {
		return nil, fmt.Errorf("objfmt: midx too short (%d bytes): %w", len(data), ErrShortFile)
	}
	if !bytes.Equal(data[0:4], midxMagic[:]) {
		return nil, fmt.Errorf("objfmt: not a midx (magic = %q): %w", data[0:4], ErrBadMagic)
	}

	m := &Midx{path: path, algo: algo, data: data}
	m.ver = uint32(data[4])
	if m.ver != 1 && m.ver != 2 {
		return nil, fmt.Errorf("objfmt: unsupported midx version %d (want 1 or 2): %w", m.ver, ErrUnsupportedVersion)
	}

	hashVer := data[5]
	wantHashVer, err := midxHashVersion(algo)
	if err != nil {
		return nil, err
	}
	if hashVer != wantHashVer {
		return nil, fmt.Errorf("objfmt: midx hash version %d does not match algo %v: %w",
			hashVer, algo, ErrAlgoMismatch)
	}

	numChunks := int(data[6])
	if data[7] != 0 {
		return nil, fmt.Errorf("objfmt: midx reserved byte non-zero (%d): %w", data[7], ErrCorrupt)
	}
	m.numPacks = binary.BigEndian.Uint32(data[8:12])

	if err := m.parseChunkTable(numChunks); err != nil {
		return nil, err
	}
	if err := m.parsePackNames(); err != nil {
		return nil, err
	}
	if err := m.parseFanout(); err != nil {
		return nil, err
	}
	if err := m.validateOIDLookup(); err != nil {
		return nil, err
	}
	return m, nil
}

// midxHashVersion maps an [Algo] to the `hash_version` byte the midx
// header is expected to carry. The mapping mirrors `oid_version` in
// canonical Git's `chunk-format.c`.
func midxHashVersion(a Algo) (byte, error) {
	switch a {
	case SHA1:
		return 1, nil
	case SHA256:
		return 2, nil
	default:
		return 0, fmt.Errorf("objfmt: unsupported algo %v: %w", a, ErrUnsupportedAlgo)
	}
}

// parseChunkTable reads the (`numChunks` + 1) entries of the chunk
// lookup table immediately after the 12-byte header. Each non-terminator
// entry is recorded in `m.chunks`; the terminator's offset doubles as
// the final chunk's end and is used to size the previous extent.
//
// Mirrors `read_table_of_contents` in `chunk-format.c`.
func (m *Midx) parseChunkTable(numChunks int) error {
	if numChunks < 0 {
		return fmt.Errorf("objfmt: negative midx chunk count: %w", ErrCorrupt)
	}
	end := midxHeaderSize + (numChunks+1)*midxChunkTOCEntry
	hashLen := m.algo.Size()
	if end > len(m.data)-hashLen {
		return fmt.Errorf("objfmt: midx chunk table overflows file (%d chunks): %w", numChunks, ErrTruncated)
	}
	bodyEnd := int64(len(m.data) - hashLen)

	m.chunks = make(map[chunkID]chunkExtent, numChunks)

	type entry struct {
		id  chunkID
		off int64
	}
	entries := make([]entry, 0, numChunks+1)
	for i := 0; i <= numChunks; i++ {
		base := midxHeaderSize + i*midxChunkTOCEntry
		var e entry
		copy(e.id[:], m.data[base:base+4])
		e.off = int64(binary.BigEndian.Uint64(m.data[base+4 : base+12]))
		entries = append(entries, e)
	}
	// The terminator entry must have id == 0; its offset is one past
	// the final chunk's body and so cannot exceed the body end.
	last := entries[len(entries)-1]
	if last.id != (chunkID{}) {
		return fmt.Errorf("objfmt: midx chunk table missing terminator (id %x): %w", last.id, ErrCorrupt)
	}
	if last.off > bodyEnd {
		return fmt.Errorf("objfmt: midx chunk table overruns body (offset %d > %d): %w",
			last.off, bodyEnd, ErrTruncated)
	}
	for i := range numChunks {
		e := entries[i]
		if e.id == (chunkID{}) {
			return fmt.Errorf("objfmt: premature midx chunk terminator at index %d: %w", i, ErrCorrupt)
		}
		// Each chunk's start offset must be `midxChunkAlignment`-byte
		// aligned; canonical Git rejects misaligned offsets at TOC
		// parse time (`chunk-format.c:127-130`).
		if e.off%midxChunkAlignment != 0 {
			return fmt.Errorf("objfmt: midx chunk %s offset %d not %d-byte aligned: %w",
				e.id, e.off, midxChunkAlignment, ErrCorrupt)
		}
		next := entries[i+1].off
		if next < e.off || next > bodyEnd {
			return fmt.Errorf("objfmt: midx chunk %s out of range (%d..%d): %w",
				e.id, e.off, next, ErrCorrupt)
		}
		if _, dup := m.chunks[e.id]; dup {
			return fmt.Errorf("objfmt: midx duplicate chunk %s: %w", e.id, ErrCorrupt)
		}
		m.chunks[e.id] = chunkExtent{off: e.off, len: next - e.off}
	}
	for _, required := range [...]chunkID{chunkPNAM, chunkOIDF, chunkOIDL, chunkOOFF} {
		if _, ok := m.chunks[required]; !ok {
			return fmt.Errorf("objfmt: midx missing required chunk %s: %w", required, ErrCorrupt)
		}
	}
	return nil
}

// String returns the four-byte chunk ID as ASCII so error messages can
// quote it directly.
func (id chunkID) String() string { return string(id[:]) }

// parsePackNames slices the PNAM chunk into NUL-separated entries and
// stores them on the Midx.
//
// For v1 files canonical Git requires the entries to be in
// strict-ascending lexicographic order, rejecting both inversions and
// duplicates ("multi-pack-index pack names out of order" in
// `midx.c:213-218`). v2 relaxes the ordering, so the check fires only
// when `m.ver == 1`.
func (m *Midx) parsePackNames() error {
	ext := m.chunks[chunkPNAM]
	body := m.data[ext.off : ext.off+ext.len]
	names := make([]string, 0, m.numPacks)
	for len(body) > 0 && uint32(len(names)) < m.numPacks {
		// Each name is NUL-terminated; the chunk is zero-padded to a
		// 4-byte boundary so a successful `IndexByte` is the only way
		// to reach the next entry.
		idx := bytes.IndexByte(body, 0)
		if idx < 0 {
			return fmt.Errorf("objfmt: midx pack-name chunk missing terminator: %w", ErrCorrupt)
		}
		names = append(names, string(body[:idx]))
		body = body[idx+1:]
	}
	if uint32(len(names)) != m.numPacks {
		return fmt.Errorf("objfmt: midx pack-name count %d != header num_packs %d: %w",
			len(names), m.numPacks, ErrCorrupt)
	}
	if m.ver == 1 {
		for i := 1; i < len(names); i++ {
			if names[i-1] >= names[i] {
				return fmt.Errorf("objfmt: midx v1 pack names out of order (%q before %q): %w",
					names[i-1], names[i], ErrCorrupt)
			}
		}
	}
	m.packNames = names
	return nil
}

// parseFanout validates that the OIDF chunk is the canonical 1024
// bytes (256 × uint32), checks that the entries are non-decreasing
// (`midx.c:62-71` rejects "oid fanout out of order"), and reads the
// count out of the last slot.
func (m *Midx) parseFanout() error {
	ext := m.chunks[chunkOIDF]
	if ext.len != 256*4 {
		return fmt.Errorf("objfmt: midx OIDF wrong size (%d, want 1024): %w", ext.len, ErrCorrupt)
	}
	fanout := m.data[ext.off : ext.off+256*4]
	var prev uint32
	for k := range 256 {
		n := binary.BigEndian.Uint32(fanout[k*4 : (k+1)*4])
		if n < prev {
			return fmt.Errorf("objfmt: midx non-monotonic fanout at %d: %d < %d: %w",
				k, n, prev, ErrCorrupt)
		}
		prev = n
	}
	m.count = prev

	// Cross-check OIDL while we are here: the lookup table must hold
	// exactly count × hashLen bytes.
	oidl := m.chunks[chunkOIDL]
	hashLen := int64(m.algo.Size())
	if oidl.len != int64(m.count)*hashLen {
		return fmt.Errorf("objfmt: midx OIDL size %d != count*%d: %w", oidl.len, hashLen, ErrCorrupt)
	}
	// And OOFF must hold count × 8 bytes (4-byte pack id + 4-byte
	// offset, per `MIDX_CHUNK_OFFSET_WIDTH` in `midx.h`).
	ooff := m.chunks[chunkOOFF]
	if ooff.len != int64(m.count)*8 {
		return fmt.Errorf("objfmt: midx OOFF size %d != count*8: %w", ooff.len, ErrCorrupt)
	}
	// Validate OOFF pack indices against `num_packs` at parse time.
	// A stray packIdx ≥ num_packs would otherwise reach
	// `midxBackend.Lookup` as a slice-bounds-out-of-range panic on
	// `coveredByMidxIndex[packIdx]`. Canonical Git also surfaces this
	// at lookup time via `nth_midxed_pack_int_id`; checking it once
	// at open keeps the lookup hot path free of the bounds test.
	for k := uint32(0); k < m.count; k++ {
		base := ooff.off + int64(k)*midxOOFFRecordSize
		packIdx := binary.BigEndian.Uint32(m.data[base : base+4])
		if packIdx >= m.numPacks {
			return fmt.Errorf("objfmt: midx OOFF pack index %d >= num_packs %d at entry %d: %w",
				packIdx, m.numPacks, k, ErrCorrupt)
		}
	}
	return nil
}

// validateOIDLookup walks the OIDL chunk and confirms its entries are
// in strict-ascending order. The fanout-bounded binary search in
// `Midx.Find` (mirroring `bsearch_one_midx` in `midx.c`) silently
// returns wrong answers if OIDL is unsorted: an OID that is present
// but at a bisection-incompatible position is reported absent. Equal
// consecutive entries are likewise corruption — a midx records each
// object once, so duplicate OIDs indicate either a writer bug or a
// malicious file.
//
// Canonical Git's `midx_read_oid_lookup` (`midx.c:76-84`) only checks
// that the chunk size matches `count * hash_len`; ordering is assumed
// because `midx-write.c` writes a sorted table. v0 is deliberately
// stricter here as defense-in-depth: a one-pass walk at parse time
// costs nothing and acts as a tripwire for both malformed external
// files and any future v0 write path that produces a corrupt OIDL.
func (m *Midx) validateOIDLookup() error {
	if m.count < 2 {
		return nil
	}
	oidl := m.chunks[chunkOIDL]
	hashLen := int64(m.algo.Size())
	body := m.data[oidl.off : oidl.off+oidl.len]
	for i := int64(1); i < int64(m.count); i++ {
		prev := body[(i-1)*hashLen : i*hashLen]
		cur := body[i*hashLen : (i+1)*hashLen]
		if bytes.Compare(prev, cur) >= 0 {
			return fmt.Errorf(
				"objfmt: midx OIDL not strictly ascending at entry %d (%x >= %x): %w",
				i, prev, cur, ErrCorrupt)
		}
	}
	return nil
}

// Close releases the in-memory body for garbage collection. Subsequent
// lookup calls observe an empty body and report misses; Close is safe
// to call more than once.
func (m *Midx) Close() error {
	m.data = nil
	m.chunks = nil
	m.packNames = nil
	m.count = 0
	m.numPacks = 0
	return nil
}

// Algo returns the hash algorithm asserted by the caller at
// [OpenMidx] time. The midx header's `hash_version` byte was checked
// against this algo.
func (m *Midx) Algo() Algo { return m.algo }

// Version returns the midx format version recorded in the header
// (1 or 2; canonical Git emits both).
func (m *Midx) Version() uint32 { return m.ver }

// Count returns the number of object entries indexed by the midx,
// read from the last fan-out slot (`fanout[255]`).
func (m *Midx) Count() uint32 { return m.count }

// PackNames returns the `.idx` basenames listed in the PNAM chunk in
// chunk order. The midx's `OOFF` chunk encodes pack indices into this
// slice. The caller receives a copy and may mutate it freely.
func (m *Midx) PackNames() []string {
	out := make([]string, len(m.packNames))
	copy(out, m.packNames)
	return out
}

// Path returns the filesystem path passed to [OpenMidx].
func (m *Midx) Path() string { return m.path }

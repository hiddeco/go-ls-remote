package objfmt

import (
	"encoding/binary"
	"fmt"
	"os"
)

// Idx is a `.idx` file mapping object ids to pack offsets and CRC32s.
//
// Both pack-index v1 (no magic, fan-out at offset 0) and v2 (magic
// `\xfftOc` + version uint32) are supported. The whole file is read
// into memory at [OpenIdx] time — typical idx files are well under
// 10 MiB even for large repositories — and every lookup is then pure
// arithmetic over the in-memory slice.
//
// The hash algorithm is supplied by the caller and asserted against
// the file's layout where possible. Idx v1 only ever stored SHA-1; an
// idx opened as v1 with [SHA256] will load successfully but
// [Idx.FindOffset] will report misses for every lookup.
type Idx struct {
	path  string
	algo  Algo
	ver   uint32
	count uint32
	data  []byte
}

// Pack-index v2 layout per `Documentation/gitformat-pack.adoc`
// (lines 285-319):
//
//	'\xfftOc'           4 bytes magic
//	uint32 version (BE) currently always 2
//	256 × uint32        first-level fan-out
//	N × hashLen         sorted object names
//	N × uint32          CRC32 of each packed object's data
//	N × uint32          pack offsets (bit 31 set ⇒ overflow index)
//	K × uint64          large-offset overflow (empty if pack ≤ 2 GiB)
//	hashLen             pack-trailer copy
//	hashLen             idx self-checksum
//
// Pack-index v1 has no magic; the file begins directly with the
// fan-out table. The layout is documented in the same file at
// lines 196-218.

const idxV2HeaderLen = 8

// idxV2Magic is the four-byte magic that distinguishes a v2 index from
// a v1 index, where the first four bytes are the fan-out's first
// entry (always small for any plausible repository).
var idxV2Magic = [4]byte{0xff, 't', 'O', 'c'}

// OpenIdx reads path into memory, identifies it as pack-index v1 or v2,
// and returns an [Idx] ready for offset lookups.
//
// The hash algorithm is supplied by the caller — idx files do not
// carry an algorithm byte — and must match the algo used to write the
// paired `.pack`. Idx v1 only stored SHA-1 ids: opening a v1 file with
// [SHA256] succeeds but every lookup will miss.
func OpenIdx(path string, algo Algo) (*Idx, error) {
	if algo.Size() == 0 {
		return nil, fmt.Errorf("objfmt: unknown algo %v: %w", algo, ErrUnsupportedAlgo)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	idx := &Idx{path: path, algo: algo, data: data}
	if hasIdxV2MagicPrefix(data) {
		if len(data) < idxV2HeaderLen {
			return nil, fmt.Errorf("objfmt: idx truncated before version field: %w", ErrTruncated)
		}
		ver := binary.BigEndian.Uint32(data[4:8])
		if ver != 2 {
			return nil, fmt.Errorf("objfmt: unsupported idx version %d (want 1 or 2): %w", ver, ErrUnsupportedVersion)
		}
		idx.ver = 2
	} else {
		idx.ver = 1
	}
	if err := idx.parseHeader(); err != nil {
		return nil, err
	}
	return idx, nil
}

// hasIdxV2MagicPrefix reports whether data starts with the four-byte
// `\xfftOc` magic introduced by pack-index v2. The check ignores the
// version word so an unsupported v2-shaped file still routes to the
// v2 error path rather than being silently mis-classified as v1.
func hasIdxV2MagicPrefix(data []byte) bool {
	if len(data) < 4 {
		return false
	}
	return data[0] == idxV2Magic[0] && data[1] == idxV2Magic[1] &&
		data[2] == idxV2Magic[2] && data[3] == idxV2Magic[3]
}

// parseHeader populates [Idx.count] from the fan-out's last entry and
// validates that the file is long enough to hold the rest of the
// expected layout. Lookups can then index into [Idx.data] without
// re-checking bounds on every call.
func (i *Idx) parseHeader() error {
	hashLen := i.algo.Size()
	switch i.ver {
	case 1:
		// fan-out: 256 × uint32, then N × (uint32 offset + hashLen oid),
		// then a 20-byte pack-trailer SHA-1, then a 20-byte idx-trailer
		// SHA-1. v1 is SHA-1 only — see `gitformat-pack.adoc` lines
		// 196-218.
		if len(i.data) < 256*4 {
			return fmt.Errorf("objfmt: idx v1 truncated before fan-out: %w", ErrTruncated)
		}
		i.count = binary.BigEndian.Uint32(i.data[255*4 : 256*4])
		// 20 is hard-coded because v1 never stored SHA-256.
		want := 256*4 + int(i.count)*(4+20) + 20 + 20
		if len(i.data) < want {
			return fmt.Errorf("objfmt: idx v1 truncated: have %d, want %d: %w", len(i.data), want, ErrTruncated)
		}
	case 2:
		fanoutEnd := idxV2HeaderLen + 256*4
		if len(i.data) < fanoutEnd {
			return fmt.Errorf("objfmt: idx v2 truncated before fan-out: %w", ErrTruncated)
		}
		i.count = binary.BigEndian.Uint32(i.data[fanoutEnd-4 : fanoutEnd])
		// Minimum size with no large-offset overflow.
		want := fanoutEnd + int(i.count)*hashLen + int(i.count)*4 + int(i.count)*4 + 2*hashLen
		if len(i.data) < want {
			return fmt.Errorf("objfmt: idx v2 truncated: have %d, want >= %d: %w", len(i.data), want, ErrTruncated)
		}
	default:
		return fmt.Errorf("objfmt: unsupported idx version %d: %w", i.ver, ErrUnsupportedVersion)
	}
	return nil
}

// Close releases the in-memory body for garbage collection. Subsequent
// lookup calls observe an empty body and report misses; Close is safe
// to call more than once.
func (i *Idx) Close() error {
	i.data = nil
	i.count = 0
	return nil
}

// Algo returns the hash algorithm asserted by the caller at
// [OpenIdx] time.
func (i *Idx) Algo() Algo { return i.algo }

// Version returns the pack-index format version recorded in the file:
// 1 or 2.
func (i *Idx) Version() uint32 { return i.ver }

// Count returns the number of object entries in the index, read from
// the last fan-out slot (`fanout[255]`).
func (i *Idx) Count() uint32 { return i.count }

// Path returns the filesystem path passed to [OpenIdx].
func (i *Idx) Path() string { return i.path }

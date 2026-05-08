package objfmt

import (
	"encoding/binary"
	"fmt"
)

// Pack is a thin random-access view over a single `.pack` file.
//
// Pack does not decompress whole objects. It exposes per-object
// header reads (type plus declared size plus delta base) and the
// leading bytes of delta payloads (source and target sizes); full
// delta resolution is the responsibility of `internal/objstore`.
//
// A Pack is safe for concurrent reads from multiple goroutines once
// constructed: every method is keyed on an offset and the underlying
// `packReader` permits concurrent [io.ReaderAt.ReadAt] calls.
type Pack struct {
	r    packReader
	algo Algo
	ver  uint32
	nr   uint32
}

// Pack header layout per `Documentation/gitformat-pack.adoc`:
//
//	'PACK'                  4 bytes magic
//	uint32 version (BE)     2 (currently emitted) or 3 (reserved)
//	uint32 nr_objects (BE)
//	... object entries ...
//	hashLen-byte trailer    SHA-1 (20) or SHA-256 (32) of all preceding bytes
//
// Canonical Git emits version 2 for both SHA-1 and SHA-256 packs; the
// hash algorithm is determined out of band from the repository's
// `extensions.objectFormat` config and asserted by the caller via the
// `algo` argument to [OpenPack].

// OpenPack opens path and parses the 12-byte pack header.
//
// Both pack version 2 (the only version emitted by canonical Git) and
// version 3 (reserved by `gitformat-pack`) are accepted; the hash
// algorithm is supplied by the caller and is not derived from the
// header. If the file is shorter than `12 + algo.Size()` (the
// minimum: header plus trailer, zero objects) OpenPack returns an
// error rather than constructing an obviously-invalid Pack.
func OpenPack(path string, algo Algo) (*Pack, error) {
	if algo.Size() == 0 {
		return nil, fmt.Errorf("objfmt: unknown algo %v", algo)
	}
	r, err := openPackReader(path)
	if err != nil {
		return nil, err
	}
	if int64(r.Len()) < int64(12+algo.Size()) {
		_ = r.Close()
		return nil, fmt.Errorf("objfmt: pack file too short (%d bytes)", r.Len())
	}
	hdr := make([]byte, 12)
	if _, err := r.ReadAt(hdr, 0); err != nil {
		_ = r.Close()
		return nil, fmt.Errorf("objfmt: read pack header: %w", err)
	}
	if string(hdr[:4]) != "PACK" {
		_ = r.Close()
		return nil, fmt.Errorf("objfmt: not a pack file (magic = %q)", hdr[:4])
	}
	ver := binary.BigEndian.Uint32(hdr[4:8])
	if ver != 2 && ver != 3 {
		_ = r.Close()
		return nil, fmt.Errorf("objfmt: unsupported pack version %d (want 2 or 3)", ver)
	}
	nr := binary.BigEndian.Uint32(hdr[8:12])
	return &Pack{r: r, algo: algo, ver: ver, nr: nr}, nil
}

// Close releases the underlying reader. Close is idempotent only to
// the extent the underlying reader is.
func (p *Pack) Close() error { return p.r.Close() }

// Algo returns the hash algorithm asserted by the caller at
// [OpenPack] time.
func (p *Pack) Algo() Algo { return p.algo }

// Version returns the pack format version recorded in the header
// (always 2 for packs emitted by current Git).
func (p *Pack) Version() uint32 { return p.ver }

// Count returns the number of object entries advertised in the pack
// header. Trusting this value lets readers loop without re-parsing
// the index, but verification of the trailer ([Pack.VerifyChecksum])
// is what proves the pack body is intact.
func (p *Pack) Count() uint32 { return p.nr }

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
// The type parameter `H` carries the pack's object-id type — either
// [SHA1Hash] or [SHA256Hash]. Methods that read OIDs (notably the
// REF_DELTA base in [Pack.ReadHeader]) return values of type `H`, so
// callers obtain typed ids without reaching back through the [Algo]
// interface.
//
// A Pack is safe for concurrent reads from multiple goroutines once
// constructed: every method is keyed on an offset and the underlying
// `packReader` permits concurrent [io.ReaderAt.ReadAt] calls.
type Pack[H Hash] struct {
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
// algorithm is supplied by the caller through the type parameter `H`
// (the `algo` argument echoes that choice and is used for the
// [Pack.Algo] getter and for trailer-size arithmetic in
// [Pack.VerifyChecksum]). If the file is shorter than `12 + algo.Size()`
// (the minimum: header plus trailer, zero objects) OpenPack returns an
// error rather than constructing an obviously-invalid Pack.
//
// `algo` must agree with `H` — SHA1 with [SHA1Hash], SHA256 with
// [SHA256Hash]. The caller is responsible for the pairing; mismatched
// instantiations are not detected here because callsites that go
// through `objstore` already gate on the discovered repo algo.
func OpenPack[H Hash](path string, algo Algo) (*Pack[H], error) {
	if algo == nil {
		return nil, fmt.Errorf("objfmt: nil algo: %w", ErrUnsupportedAlgo)
	}
	r, err := openPackReader(path)
	if err != nil {
		return nil, err
	}
	if r.Len() < int64(12+algo.Size()) {
		_ = r.Close()
		return nil, fmt.Errorf("objfmt: pack file too short (%d bytes): %w", r.Len(), ErrShortFile)
	}
	hdr := make([]byte, 12)
	if _, err := r.ReadAt(hdr, 0); err != nil {
		_ = r.Close()
		return nil, fmt.Errorf("objfmt: read pack header: %w", err)
	}
	if string(hdr[:4]) != "PACK" {
		_ = r.Close()
		return nil, fmt.Errorf("objfmt: not a pack file (magic = %q): %w", hdr[:4], ErrBadMagic)
	}
	ver := binary.BigEndian.Uint32(hdr[4:8])
	if ver != 2 && ver != 3 {
		_ = r.Close()
		return nil, fmt.Errorf("objfmt: unsupported pack version %d (want 2 or 3): %w", ver, ErrUnsupportedVersion)
	}
	nr := binary.BigEndian.Uint32(hdr[8:12])
	return &Pack[H]{r: r, algo: algo, ver: ver, nr: nr}, nil
}

// Close releases the underlying reader. Close is idempotent only to
// the extent the underlying reader is.
func (p *Pack[H]) Close() error { return p.r.Close() }

// Algo returns the hash algorithm asserted by the caller at
// [OpenPack] time. Returned as the [Algo] interface value so callers
// that need only the identity (capability emission, error messages)
// avoid carrying the type parameter forward.
func (p *Pack[H]) Algo() Algo { return p.algo }

// Version returns the pack format version recorded in the header
// (always 2 for packs emitted by current Git).
func (p *Pack[H]) Version() uint32 { return p.ver }

// Count returns the number of object entries advertised in the pack
// header. Trusting this value lets readers loop without re-parsing
// the index, but verification of the trailer ([Pack.VerifyChecksum])
// is what proves the pack body is intact.
func (p *Pack[H]) Count() uint32 { return p.nr }

// Len returns the size of the underlying pack file in bytes, including
// the 12-byte header and the `algo.Size()`-byte trailer. Callers that
// need to bound a per-object byte range (notably the CRC32 verifier
// in `internal/objstore`, which hashes the compressed bytes between
// one object's start and the next object's offset) use Len to land on
// the trailer when an object is the last entry in the pack.
func (p *Pack[H]) Len() int64 { return p.r.Len() }

// ReadAt copies bytes from absolute offset off into b, returning the
// number of bytes read and any read error. It is a thin pass-through
// to the underlying [io.ReaderAt]; concurrent calls are safe.
//
// Exposed so callers in `internal/objstore` can hash the on-disk
// compressed bytes of a packed object without re-opening the file or
// reaching through unexported state. The contract matches
// [io.ReaderAt]: a short read at end-of-file returns the bytes that
// were available alongside an [io.EOF].
func (p *Pack[H]) ReadAt(b []byte, off int64) (int, error) { return p.r.ReadAt(b, off) }

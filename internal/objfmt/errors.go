package objfmt

import "errors"

// Sentinel errors returned by the parsers in this package. Callers
// match against these with [errors.Is]; the wrapping
// `fmt.Errorf("...: %w", ..., sentinel)` adds the offending value
// (offsets, declared sizes, magic bytes, version numbers) for
// diagnostics.
//
// I/O errors from the underlying file or [io.ReaderAt] propagate
// unwrapped. The errors below cover only the format-level violations
// surfaced by [OpenPack], [OpenIdx], [OpenMidx], the per-object
// header readers, and the trailer-checksum verifiers.
var (
	// ErrUnsupportedAlgo is returned when the caller-supplied [Algo]
	// has zero hash size — i.e. it is neither [SHA1] nor [SHA256] —
	// or when a midx's `hash_version` byte cannot be mapped to one of
	// those algos.
	ErrUnsupportedAlgo = errors.New("objfmt: unsupported algo")

	// ErrShortFile is returned when a pack, idx, or midx file is too
	// small to contain the fixed header plus the algo-sized trailer.
	// It signals truncation discovered before any field is parsed.
	ErrShortFile = errors.New("objfmt: file too short for header and trailer")

	// ErrBadMagic is returned when the four-byte magic at the start of
	// a pack (`PACK`), v2 idx (`\xfftOc`), or midx (`MIDX`) does not
	// match the expected signature.
	ErrBadMagic = errors.New("objfmt: bad magic")

	// ErrUnsupportedVersion is returned when a pack, idx, or midx
	// declares a format version this reader does not implement.
	ErrUnsupportedVersion = errors.New("objfmt: unsupported version")

	// ErrAlgoMismatch is returned when the caller asserts an [Algo]
	// that disagrees with the value declared by the file — currently
	// only the midx header's `hash_version` byte. Pack and idx files
	// carry no algo byte, so callers there are trusted blindly.
	ErrAlgoMismatch = errors.New("objfmt: algo does not match file")

	// ErrTruncated is returned when a structural region — header,
	// chunk table, fan-out, name table, varint — ends before its
	// declared length. Distinct from [ErrShortFile], which fires
	// before any structure is parsed.
	ErrTruncated = errors.New("objfmt: truncated")

	// ErrCorrupt is returned for self-inconsistent files: an
	// OFS_DELTA whose base lands inside or past the delta itself, an
	// unknown pack object type, a midx with a missing required chunk
	// or duplicate chunk id, a chained midx (out of scope for this
	// reader), and similar invariant violations that survive the
	// truncation checks.
	ErrCorrupt = errors.New("objfmt: corrupt")

	// ErrChecksumMismatch is returned by [Pack.VerifyChecksum],
	// [Idx.VerifyChecksum], and [Midx.VerifyChecksum] when the
	// trailing hash does not match a fresh hash of the preceding
	// bytes.
	ErrChecksumMismatch = errors.New("objfmt: trailer checksum mismatch")
)

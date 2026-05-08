// Package objfmt models Git's on-disk object formats: the hash
// algorithm in use, the 32-byte hash buffer that fits both SHA-1 and
// SHA-256 object ids, and the pack object type enum.
//
// The package is dependency-free so any reader, writer, or transport
// in the library can import it without pulling unrelated code.
package objfmt

// Algo identifies a Git object hash algorithm. The zero value is not a
// valid algorithm; constructors return [SHA1] or [SHA256] explicitly so
// the caller is forced to pick one.
type Algo uint8

const (
	// SHA1 is the original 20-byte object id algorithm.
	SHA1 Algo = iota + 1

	// SHA256 is the 32-byte object id algorithm introduced by the
	// SHA-256 transition. Selected per-repository via the
	// `extensions.objectFormat` config and advertised on the wire by
	// the v2 `object-format=sha256` capability.
	SHA256
)

// Size returns the raw hash size in bytes for a, or 0 if a is not a
// known algorithm.
func (a Algo) Size() int {
	switch a {
	case SHA1:
		return 20
	case SHA256:
		return 32
	default:
		return 0
	}
}

// String returns the canonical lowercase name for a — `sha1` or
// `sha256` — as used in `extensions.objectFormat` and the v2
// `object-format` capability. Unknown values return `unknown`.
func (a Algo) String() string {
	switch a {
	case SHA1:
		return "sha1"
	case SHA256:
		return "sha256"
	default:
		return "unknown"
	}
}

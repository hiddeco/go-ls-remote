// Package objfmt models Git's on-disk object formats: the hash
// algorithms in use, the typed object id values for each, and the
// pack object type enum.
//
// The package is dependency-free so any reader, writer, or transport
// in the library can import it without pulling unrelated code.
//
// # Layering
//
// objfmt reads on-disk structures and parses individual records:
// pack object headers including the OFS_DELTA / REF_DELTA pointers,
// idx and midx fanout/lookup chunks, and loose object framing. It
// does NOT walk delta chains, follow REF_DELTA bases across packs,
// or otherwise interpret records relative to one another. Chain
// depth is therefore unbounded at this layer — a maliciously
// crafted pack can declare an arbitrarily deep REF_DELTA / OFS_DELTA
// chain and objfmt will report each individual header faithfully.
//
// The higher-layer object store (`internal/objstore`) is the
// enforcement point: it must cap chain depth before resolution to
// prevent denial-of-service from untrusted packs. objfmt deliberately
// stays out of that policy so the same parser can serve trusted
// on-disk packs (no cap needed) and wire-received packs (cap
// aggressively) without conditional knobs.
package objfmt

import (
	"encoding/hex"
	"fmt"
)

// Algo identifies a Git object hash algorithm.
//
// Algo is a sealed interface: the only implementations are the
// package-level [SHA1] and [SHA256] values, both backed by zero-size
// unexported structs. The unexported [Algo.sealed] method blocks
// third-party additions, and the two exported values are comparable so
// callers can switch on them or test equality (`algo == SHA1`).
//
// Generic code that operates on a concrete object id uses
// [Hash]-constrained type parameters and skips the interface entirely;
// Algo is reserved for the runtime-discovery boundary (config parsing,
// capability emission, error messages).
type Algo interface {
	// Size returns the raw hash size in bytes — 20 for [SHA1], 32 for
	// [SHA256].
	Size() int

	// String returns the canonical lowercase name, `sha1` or `sha256`,
	// as used in `extensions.objectFormat` and the v2 `object-format`
	// capability.
	String() string

	// sealed prevents third-party implementations of Algo. It is
	// unexported so only types declared in this package can satisfy
	// the interface.
	sealed()
}

// sha1Algo implements [Algo] for the SHA-1 algorithm. It is a
// zero-size struct so [SHA1] is comparable and free to copy.
type sha1Algo struct{}

// sha256Algo implements [Algo] for the SHA-256 algorithm. It is a
// zero-size struct so [SHA256] is comparable and free to copy.
type sha256Algo struct{}

// SHA1 is the original 20-byte object id algorithm.
var SHA1 Algo = sha1Algo{}

// SHA256 is the 32-byte object id algorithm introduced by the SHA-256
// transition. Selected per-repository via the `extensions.objectFormat`
// config and advertised on the wire by the v2 `object-format=sha256`
// capability.
var SHA256 Algo = sha256Algo{}

func (sha1Algo) Size() int      { return 20 }
func (sha1Algo) String() string { return "sha1" }
func (sha1Algo) sealed()        {}

func (sha256Algo) Size() int      { return 32 }
func (sha256Algo) String() string { return "sha256" }
func (sha256Algo) sealed()        {}

// ParseHex decodes s into a [SHA1Hash], requiring exactly 40 hex
// characters. Returns an error if s has the wrong length or contains
// non-hex characters.
func (sha1Algo) ParseHex(s string) (SHA1Hash, error) {
	if len(s) != 40 {
		return SHA1Hash{}, fmt.Errorf("objfmt: sha1 hex %q has length %d, want 40", s, len(s))
	}
	var h SHA1Hash
	if _, err := hex.Decode(h[:], []byte(s)); err != nil {
		return SHA1Hash{}, fmt.Errorf("objfmt: invalid sha1 hex: %w", err)
	}
	return h, nil
}

// ParseHex decodes s into a [SHA256Hash], requiring exactly 64 hex
// characters. Returns an error if s has the wrong length or contains
// non-hex characters.
func (sha256Algo) ParseHex(s string) (SHA256Hash, error) {
	if len(s) != 64 {
		return SHA256Hash{}, fmt.Errorf("objfmt: sha256 hex %q has length %d, want 64", s, len(s))
	}
	var h SHA256Hash
	if _, err := hex.Decode(h[:], []byte(s)); err != nil {
		return SHA256Hash{}, fmt.Errorf("objfmt: invalid sha256 hex: %w", err)
	}
	return h, nil
}

// ParseSHA1Hex is a package-level convenience wrapping
// `SHA1.(sha1Algo).ParseHex` so callers that already know they want a
// [SHA1Hash] can skip the type assertion. Equivalent to calling
// [sha1Algo.ParseHex] directly.
func ParseSHA1Hex(s string) (SHA1Hash, error) { return sha1Algo{}.ParseHex(s) }

// ParseSHA256Hex is the [SHA256Hash] sibling of [ParseSHA1Hex].
func ParseSHA256Hex(s string) (SHA256Hash, error) { return sha256Algo{}.ParseHex(s) }

// ParseHexAs parses s as a hex-encoded object id of the type parameter
// `H`, returning the typed value. Generic callers that only know `H`
// abstractly use this helper to dispatch to the correct concrete
// parser without a per-callsite type switch.
//
// The dispatch is bounded by the [Hash] type set: H is statically
// one of [SHA1Hash] or [SHA256Hash], so the switch covers every
// possible instantiation. The default arm exists only as defence in
// depth — it cannot be reached at runtime — and returns an error
// rather than panicking so a future widening of the type set surfaces
// at a callsite boundary instead of mid-loop.
func ParseHexAs[H Hash](s string) (H, error) {
	var zero H
	switch any(zero).(type) {
	case SHA1Hash:
		h, err := ParseSHA1Hex(s)
		return any(h).(H), err
	case SHA256Hash:
		h, err := ParseSHA256Hex(s)
		return any(h).(H), err
	}
	return zero, fmt.Errorf("objfmt: unreachable: H is sealed to SHA1Hash or SHA256Hash")
}

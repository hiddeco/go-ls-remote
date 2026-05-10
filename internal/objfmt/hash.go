package objfmt

import (
	"encoding/hex"
	"unsafe"
)

// SHA1Hash is the 20-byte object id under the SHA-1 algorithm.
//
// SHA1Hash carries its size in its type; methods take no [Algo]
// argument because the type is the algorithm. Defined as a fixed-size
// array so SHA1Hash is comparable and usable directly as a Go map key
// — the [Hash] constraint relies on this for generic ref and peel
// caches keyed by object id.
type SHA1Hash [20]byte

// SHA256Hash is the 32-byte object id under the SHA-256 algorithm.
// See [SHA1Hash] for the design rationale; SHA256Hash differs only in
// its byte length.
type SHA256Hash [32]byte

// Hash is the type-set constraint that admits both concrete hash
// types. Generic code that operates on either uses `[H Hash]`.
//
// The `comparable` bound is required so generic code can use H as a
// Go map key — a type set of comparable types does not auto-satisfy
// `comparable` in Go's generics, so the bound must be explicit.
//
// The method set ([Hex], [AppendHex], [IsZero], [Bytes]) is declared
// alongside the type union so generic callers can invoke the typed
// hex / zero-check methods on `H` without a per-callsite type switch.
// Both [SHA1Hash] and [SHA256Hash] satisfy this method set.
type Hash interface {
	comparable
	SHA1Hash | SHA256Hash

	Hex() string
	AppendHex(dst []byte) []byte
	IsZero() bool
	Bytes() []byte
}

// Hex returns the lowercase 40-character hex encoding of h.
func (h SHA1Hash) Hex() string { return hex.EncodeToString(h[:]) }

// AppendHex appends the lowercase 40-character hex encoding of h to
// dst and returns the extended slice. Pairs with [SHA1Hash.Hex] the
// same way `strconv.AppendInt` pairs with `strconv.Itoa`: callers on
// a hot loop pass a scratch buffer to avoid the per-call string
// allocation that Hex's return type forces.
func (h SHA1Hash) AppendHex(dst []byte) []byte { return hex.AppendEncode(dst, h[:]) }

// IsZero reports whether h is the all-zero hash. Canonical Git uses
// the all-zero hash as the null-OID sentinel — for example as the
// `old` value of a receive-pack ref create or the `new` value of a
// delete — and the library follows the same convention.
func (h SHA1Hash) IsZero() bool { return h == SHA1Hash{} }

// Bytes returns a slice over a fresh copy of the receiver's bytes.
// The returned slice has length and capacity 20. Mutating the returned
// slice does not affect the receiver — the value-receiver method takes
// `h` by copy, and the slice references that copy's storage.
//
// Callers on a hot path that need to avoid the per-call array copy
// should use [SHA1Hash.AppendHex] or take the slice directly with
// `h[:]` instead.
func (h SHA1Hash) Bytes() []byte { return h[:] }

// Hex returns the lowercase 64-character hex encoding of h. See
// [SHA1Hash.Hex] for the SHA-1 sibling.
func (h SHA256Hash) Hex() string { return hex.EncodeToString(h[:]) }

// AppendHex appends the lowercase 64-character hex encoding of h to
// dst and returns the extended slice. See [SHA1Hash.AppendHex] for
// the SHA-1 sibling.
func (h SHA256Hash) AppendHex(dst []byte) []byte { return hex.AppendEncode(dst, h[:]) }

// IsZero reports whether h is the all-zero hash. See [SHA1Hash.IsZero]
// for the SHA-1 sibling.
func (h SHA256Hash) IsZero() bool { return h == SHA256Hash{} }

// Bytes returns a slice over a fresh copy of the receiver's bytes.
// The returned slice has length and capacity 32. See [SHA1Hash.Bytes]
// for the copy contract.
func (h SHA256Hash) Bytes() []byte { return h[:] }

// hashBytes returns a slice over the in-place storage of h. Used by
// generic readers ([Idx.searchV2], [Midx.searchOID], etc.) that need a
// `[]byte` view of the OID without per-call allocation. The
// type-parameter `H` cannot be sliced directly with `h[:]` because
// the union of [SHA1Hash] and [SHA256Hash] does not have a single core
// type — Go's generic slicing rules require one. The
// [unsafe.Slice] form is the minimum-surface workaround; the slice
// references the local-copy `h`, so callers must not retain it past
// the end of `h`'s scope.
//
// For the typed-public APIs ([SHA1Hash.Bytes], [SHA256Hash.Bytes])
// continue to use the by-copy form: their `[:]` form is inherently
// safer because the copy lives on the caller's stack.
func hashBytes[H Hash](h *H) []byte {
	return unsafe.Slice((*byte)(unsafe.Pointer(h)), len(*h))
}

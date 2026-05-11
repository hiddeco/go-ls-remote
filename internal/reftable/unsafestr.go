package reftable

import "unsafe"

// asReadOnlyBytes returns a []byte view of s's underlying bytes. The
// caller must not modify the result and must not let it outlive s. It
// is the dual of `string([]byte)` (which copies) for the case where
// shared, read-only memory is what the caller actually needs.
//
// Mirrors stdlib's `unsafe.Slice(unsafe.StringData(s), len(s))` idiom
// behind a named helper so call sites read intent rather than pointer
// arithmetic. See [objfmt.HashBytes] for the analogous minimum-surface
// unsafe wrapper used by the ref-record decoder.
func asReadOnlyBytes(s string) []byte {
	return unsafe.Slice(unsafe.StringData(s), len(s))
}

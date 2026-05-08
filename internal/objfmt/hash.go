package objfmt

import (
	"encoding/hex"
	"fmt"
)

// Hash is a 32-byte buffer holding a Git object id.
//
// The interpretation is contextual: the owning store knows whether a
// given Hash carries a SHA-1 or a SHA-256 id. SHA-1 ids occupy the
// low 20 bytes and leave the high 12 bytes zero; SHA-256 ids fill all
// 32. Defined as a fixed-size array so Hash is comparable and usable
// directly as a Go map key — two SHA-1 ids with the same low 20 bytes
// compare equal because the cleared high bytes match too.
type Hash [32]byte

// IsZero reports whether h is the all-zero hash.
//
// Canonical Git uses the all-zero hash as the null-OID sentinel — for
// example as the `old` value of a receive-pack ref create or the `new`
// value of a delete — and the library follows the same convention.
func (h Hash) IsZero() bool {
	for _, b := range h {
		if b != 0 {
			return false
		}
	}
	return true
}

// Hex returns the lowercase hex encoding of h interpreted under a:
// 40 chars of the low 20 bytes for [SHA1], 64 chars of all 32 bytes
// for [SHA256]. An unknown algorithm returns the empty string.
func (h Hash) Hex(a Algo) string {
	n := a.Size()
	if n == 0 {
		return ""
	}
	return hex.EncodeToString(h[:n])
}

// ParseHex decodes s into a Hash, requiring exactly `a.Size()*2` hex
// characters. Bytes beyond `a.Size()` are left zero so SHA-1 ids parsed
// this way compare equal to other SHA-1 ids with the same low 20 bytes.
//
// Returns an error if a is not a known [Algo], if s has the wrong
// length, or if s contains non-hex characters.
func ParseHex(s string, a Algo) (Hash, error) {
	n := a.Size()
	if n == 0 {
		return Hash{}, fmt.Errorf("objfmt: unknown algo %v", a)
	}
	want := n * 2
	if len(s) != want {
		return Hash{}, fmt.Errorf("objfmt: hex %q has length %d, want %d", s, len(s), want)
	}
	var h Hash
	if _, err := hex.Decode(h[:n], []byte(s)); err != nil {
		return Hash{}, fmt.Errorf("objfmt: invalid hex: %w", err)
	}
	return h, nil
}

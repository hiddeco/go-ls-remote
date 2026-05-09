package wire

import "strings"

// ParseCapabilities parses a whitespace-separated capability list and
// returns the tokens in input order. The input is the raw byte string
// that follows the NUL on a v0/v1 first ref line, or the payload of a
// single v2 capability pkt-line — both shapes reduce to "one or more
// whitespace-separated tokens" once the framing is stripped.
//
// Each token is either `name` (a boolean capability) or `name=value`
// (a value capability). Only the first `=` separates name from value;
// later `=` bytes belong to the value. Values never contain whitespace
// — canonical Git stops the value at the next space, tab, or newline
// in `connect.c::parse_feature_value` (see lines 614-659), and this
// implementation matches that behaviour.
//
// The parser is total over byte input: it never reports an error.
// Canonical Git's `parse_feature_value` returns NULL only when a
// requested feature is absent, never to signal a malformed list, so
// matching that contract keeps the API simple. Empty input or
// whitespace-only input returns an empty slice.
//
// The substring-match guard at `connect.c:629`
// (`feature_list == found || isspace(found[-1])`) prevents `multi`
// from matching the prefix of `multi_ack`. Tokenising up front makes
// that guard automatic: distinct tokens never alias one another.
func ParseCapabilities(s string) RawCapabilities {
	if len(s) == 0 {
		return nil
	}
	var caps RawCapabilities
	for _, tok := range strings.FieldsFunc(s, isWireSpace) {
		name, value, hasEq := strings.Cut(tok, "=")
		rc := RawCapability{Name: name}
		if hasEq {
			rc.Value = value
		}
		caps = append(caps, rc)
	}
	return caps
}

// isWireSpace reports whether r is whitespace per canonical Git's
// `isspace` usage in `connect.c::parse_feature_value`: space, tab,
// or newline. Carriage return is not in the canonical set, but it
// is bracketed by the same guards in practice — the wire never emits
// CR inside a capability list.
func isWireSpace(r rune) bool {
	switch r {
	case ' ', '\t', '\n':
		return true
	}
	return false
}

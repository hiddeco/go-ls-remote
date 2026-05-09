// Package wire encodes and decodes the Git smart-protocol wire formats
// consumed by the root `lsremote` package. It is intentionally narrow:
// it covers reference-discovery (v0/v1 advertisements, v2 ls-refs and
// object-info), the capability lists those flows exchange, and the
// HTTP/SSH framing details that ride alongside them. Object transfer
// and the wider fetch state machine live elsewhere.
//
// The package owns four raw value types — [RawCapability],
// [RawCapabilities], [RawRef], and [RawObjectInfo] — plus the
// umbrella [Advertisement] that pairs them with the negotiated
// [transport.ProtocolVersion]. Each "Raw" type holds bytes as the
// server emitted them; higher layers translate to richer Go values.
package wire

import "github.com/hiddeco/go-ls-remote/transport"

// RawCapability is a single capability token exactly as it appeared
// on the wire. A boolean capability has [RawCapability.Value] set to
// the empty string; a `name=value` capability stores the substring
// after the first `=` byte. Values never contain whitespace.
type RawCapability struct {
	Name, Value string
}

// RawCapabilities is an ordered list of capability tokens as they
// appeared on the wire. Order is preserved so that callers comparing
// against canonical Git's parsing — which scans left-to-right and
// returns the first hit for a given name — reach the same answer.
//
// The same name may appear more than once; `symref` is the canonical
// example. [RawCapabilities.Get] returns the first occurrence,
// [RawCapabilities.All] returns every occurrence, and
// [RawCapabilities.Names] surfaces the encounter order including
// duplicates so callers can reconstruct the wire ordering.
type RawCapabilities []RawCapability

// Get returns the value of the first capability named name and a
// boolean reporting whether it was found. A boolean capability
// (`name` with no `=value`) returns the empty string with ok=true.
func (c RawCapabilities) Get(name string) (string, bool) {
	for _, rc := range c {
		if rc.Name == name {
			return rc.Value, true
		}
	}
	return "", false
}

// All returns every value recorded for capabilities named name, in
// encounter order. It returns nil when name is absent. Boolean
// capabilities contribute the empty string.
func (c RawCapabilities) All(name string) []string {
	var out []string
	for _, rc := range c {
		if rc.Name == name {
			out = append(out, rc.Value)
		}
	}
	return out
}

// Has reports whether at least one capability named name is present.
func (c RawCapabilities) Has(name string) bool {
	for _, rc := range c {
		if rc.Name == name {
			return true
		}
	}
	return false
}

// Names returns the capability names in encounter order, including
// duplicates. The result mirrors the wire ordering exactly: a list
// with `symref` listed twice came from a server that emitted two
// `symref=...` tokens.
func (c RawCapabilities) Names() []string {
	if c == nil {
		return nil
	}
	out := make([]string, len(c))
	for i, rc := range c {
		out[i] = rc.Name
	}
	return out
}

// RawRef is a single advertised reference exactly as it appeared on
// the wire. [RawRef.OID] is the lowercase hex object id; [RawRef.Name]
// is the full refname (e.g. `refs/heads/main`); [RawRef.Peeled] is
// the dereferenced object id for an annotated tag, populated when the
// server emits a `^{}` companion line; [RawRef.Symref] is the target
// refname for a symbolic ref, populated from a matching `symref=`
// capability after the v0/v1 ref list is parsed.
//
// [RawRef.Unborn] is set when a v2 `ls-refs` response uses the literal
// `unborn` token in place of an object id, signalling a ref (typically
// `HEAD`) that points at an unborn branch on a freshly-initialised
// repository. [RawRef.OID] is then empty. The flag distinguishes an
// unborn ref from a malformed line missing its OID.
type RawRef struct {
	OID, Name, Peeled, Symref string
	Unborn                    bool
}

// RawObjectInfo is one row of a v2 `object-info` response: an object
// id paired with its canonical size. Size is signed because callers
// surface it through Go's stdlib I/O types, but the wire value is
// always non-negative.
type RawObjectInfo struct {
	OID  string
	Size int64
}

// Advertisement is the discovery-time view a transport hands back
// after the initial handshake. [Advertisement.Version] records the
// negotiated wire protocol; [Advertisement.Caps] holds the server's
// capability list (sourced from the v0/v1 first ref line or the v2
// `capabilities` advertisement); [Advertisement.Refs] holds the
// advertised references for v0 and v1 only — v2 callers fetch refs
// on demand via the `ls-refs` command, so the field is left nil.
type Advertisement struct {
	Version transport.ProtocolVersion
	Caps    RawCapabilities
	Refs    []RawRef
}

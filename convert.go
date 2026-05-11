package lsremote

import (
	"slices"
	"strings"

	"github.com/hiddeco/go-ls-remote/internal/wire"
)

// v2CommandNames is the library-curated set of v2 commands that
// [Session] methods know how to issue. Per `gitprotocol-v2.adoc`
// §"capability-advertisement", a v2 server advertises each command it
// supports as a top-level capability whose name is the command name;
// the canonical set today is `ls-refs`, `fetch`, `object-info`, and
// `bundle-uri`. Intersecting the advertised names against this set
// keeps metadata capabilities (`agent`, `object-format`,
// `server-option`, `session-id`, ...) out of [Capabilities.Commands]
// without having to enumerate them. The set grows when the library
// learns to issue a new command. Callers who need the verbatim wire
// view — every advertised capability name, including commands this
// library does not yet implement — can read [Capabilities.Raw].
var v2CommandNames = []string{
	"ls-refs",
	"fetch",
	"object-info",
	"bundle-uri",
}

// convertCaps builds a public [Capabilities] from raw and the
// concretely-negotiated protocol version v. The result is safe to hand
// to callers as-is: every map and slice the function populates is
// freshly allocated and never aliases raw.
//
// Population rules per field:
//
//   - [Capabilities.Version] is v verbatim — the library never returns
//     an auto sentinel here.
//   - [Capabilities.Agent] comes from the first `agent=` capability,
//     or the empty string when absent.
//   - [Capabilities.ObjectFormat] is the literal `object-format=` value
//     when present (the constants [ObjectFormatSHA1] /
//     [ObjectFormatSHA256] cover the two known formats). An unknown
//     value is stored raw so callers can detect future hash algorithms.
//     When the server does not advertise `object-format` and the
//     negotiated version is v0 or v1, the field is normalised to
//     [ObjectFormatSHA1] — omitting the capability on those protocol
//     versions always implies SHA-1 per the capability contract.
//     For v2 servers that omit `object-format` (a protocol violation),
//     the field is left at its zero value so callers can detect the
//     misbehaviour.
//   - [Capabilities.Commands] is populated only for v2 by intersecting
//     [v2CommandNames] with the advertised capability names. v0/v1
//     servers never populate this field — they do not advertise
//     commands at the capability level.
//   - [Capabilities.LSRefsArgs] and [Capabilities.ObjectInfoArgs] are
//     the whitespace-split values of the `ls-refs=` and `object-info=`
//     capabilities. A boolean advertisement (capability present with no
//     value) yields an empty non-nil slice; absence yields nil.
//     Splitting uses [strings.Fields], which matches the v2 grammar's
//     tokenisation ("one or more whitespace-separated arguments").
//     The `fetch=` capability value is not split into a typed field;
//     it is available verbatim in [Capabilities.Raw].
//   - [Capabilities.Symrefs] is populated only for v0/v1 by walking
//     every `symref=NAME:TARGET` capability and splitting on the first
//     `:` per `connect.c::parse_one_symref_info`. Malformed entries —
//     no colon, empty name, or empty target — are skipped silently,
//     matching canonical Git's behaviour. v2 servers convey symrefs on
//     the per-ref response from `ls-refs` instead, so this slice stays
//     empty for v2.
//   - [Capabilities.Raw] is the verbatim fallback keyed by capability
//     name. Repeated names accumulate values in encounter order;
//     boolean capabilities contribute the empty string. The returned
//     map is always non-nil — callers can read missing keys without a
//     prior nil check.
func convertCaps(raw wire.RawCapabilities, v ProtocolVersion) Capabilities {
	c := Capabilities{Version: v}

	if agent, ok := raw.Get("agent"); ok {
		c.Agent = agent
	}
	if of, ok := raw.Get("object-format"); ok {
		c.ObjectFormat = ObjectFormat(of)
	} else if v != ProtocolV2 {
		// v0/v1 servers that omit `object-format` always use SHA-1;
		// see gitprotocol-capabilities.adoc §"object-format":
		// "If this capability is not provided, it is assumed that the
		// only supported algorithm is SHA-1."
		// v2 servers that omit `object-format` violate the v2 wire
		// protocol; leave the field empty so callers can detect it.
		c.ObjectFormat = ObjectFormatSHA1
	}

	if v == ProtocolV2 {
		// Iterate `raw` directly rather than via `raw.Names()`, which
		// would allocate an intermediate `[]string`. The `Commands`
		// slice is lazily made on first match and pre-sized to the
		// upper bound — at most one element per curated v2 command —
		// so it never grows. When no recognised commands are
		// advertised, `c.Commands` stays nil, matching the documented
		// contract for the v0/v1 path.
		for _, rc := range raw {
			if !slices.Contains(v2CommandNames, rc.Name) || slices.Contains(c.Commands, rc.Name) {
				continue
			}
			if c.Commands == nil {
				c.Commands = make([]string, 0, len(v2CommandNames))
			}
			c.Commands = append(c.Commands, rc.Name)
		}
	}

	c.LSRefsArgs = splitArgs(raw, "ls-refs")
	c.ObjectInfoArgs = splitArgs(raw, "object-info")

	if v != ProtocolV2 {
		// Iterate `raw` directly rather than calling `raw.All("symref")`,
		// which would allocate an intermediate `[]string` of values.
		for _, rc := range raw {
			if rc.Name != "symref" {
				continue
			}
			name, target, ok := strings.Cut(rc.Value, ":")
			if !ok || name == "" || target == "" {
				continue
			}
			c.Symrefs = append(c.Symrefs, Symref{Name: name, Target: target})
		}
	}

	rawMap := make(map[string][]string, len(raw))
	for _, rc := range raw {
		rawMap[rc.Name] = append(rawMap[rc.Name], rc.Value)
	}
	c.Raw = rawMap

	return c
}

// splitArgs returns the whitespace-split arguments of the first
// capability named name, an empty non-nil slice for a boolean
// advertisement (`name` with no value), or nil when name is absent.
func splitArgs(raw wire.RawCapabilities, name string) []string {
	val, ok := raw.Get(name)
	if !ok {
		return nil
	}
	if val == "" {
		return []string{}
	}
	return strings.Fields(val)
}

// convertRef builds a public [Ref] from a single raw wire ref. The
// only structural translation is the `OID`→`Hash` field rename; an
// unborn ref keeps an empty [Ref.Hash], which is the public encoding
// for the unborn-`HEAD` case.
func convertRef(rr wire.RawRef) Ref {
	return Ref{
		Name:   rr.Name,
		Hash:   rr.OID,
		Peeled: rr.Peeled,
		Symref: rr.Symref,
	}
}

// convertRefs is the slice form of [convertRef]. A nil input returns
// nil; an empty non-nil input returns an empty non-nil slice.
func convertRefs(rrs []wire.RawRef) []Ref {
	if rrs == nil {
		return nil
	}
	out := make([]Ref, len(rrs))
	for i, rr := range rrs {
		out[i] = convertRef(rr)
	}
	return out
}

// convertObjectInfo builds a public [ObjectInfo] from a single raw
// row. [ObjectInfo.Size] is propagated verbatim from
// [wire.RawObjectInfo.Size]; translation of "size not requested" to the
// public `-1` sentinel is the caller's responsibility because the wire
// layer carries no signal distinguishing an unrequested size from a
// legitimate zero-byte blob.
func convertObjectInfo(roi wire.RawObjectInfo) ObjectInfo {
	return ObjectInfo{Hash: roi.OID, Size: roi.Size}
}

// convertObjectInfos is the slice form of [convertObjectInfo]. A nil
// input returns nil; an empty non-nil input returns an empty non-nil
// slice.
func convertObjectInfos(rois []wire.RawObjectInfo) []ObjectInfo {
	if rois == nil {
		return nil
	}
	out := make([]ObjectInfo, len(rois))
	for i, roi := range rois {
		out[i] = convertObjectInfo(roi)
	}
	return out
}

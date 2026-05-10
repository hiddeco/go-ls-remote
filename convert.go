package lsremote

import (
	"strings"

	"github.com/hiddeco/go-ls-remote/internal/wire"
)

// v2CommandNames is the allow-list of v2 commands recognised when
// translating a server's capability advertisement into
// [Capabilities.Commands]. Per `gitprotocol-v2.adoc`
// §"capability-advertisement", a v2 server advertises each command it
// supports as a top-level capability whose name is the command name;
// the canonical set today is `ls-refs`, `fetch`, `object-info`, and
// `bundle-uri`. Intersecting the advertised names against this fixed
// list keeps metadata capabilities (`agent`, `object-format`,
// `server-option`, `session-id`, ...) out of the command slice without
// having to enumerate them. Future v2 commands not in this list will
// not show up in [Capabilities.Commands]; callers needing exhaustive
// information can inspect [Capabilities.Raw] instead.
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
//     value is stored raw so callers can detect future hash algorithms;
//     absence leaves the field at its zero value rather than defaulting
//     to `sha1`.
//   - [Capabilities.Commands] is populated only for v2 by intersecting
//     [v2CommandNames] with the advertised capability names. v0/v1
//     servers never populate this field — they do not advertise
//     commands at the capability level.
//   - [Capabilities.LSRefsArgs], [Capabilities.ObjectInfoArgs] and
//     [Capabilities.FetchArgs] are the whitespace-split values of the
//     `ls-refs=`, `object-info=` and `fetch=` capabilities. A boolean
//     advertisement (capability present with no value) yields an empty
//     non-nil slice; absence yields nil. Splitting uses
//     [strings.Fields], which matches the v2 grammar's tokenisation
//     ("one or more whitespace-separated arguments").
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
	}

	if v == ProtocolV2 {
		names := raw.Names()
		seen := make(map[string]struct{}, len(v2CommandNames))
		for _, n := range names {
			if _, dup := seen[n]; dup {
				continue
			}
			for _, cmd := range v2CommandNames {
				if n == cmd {
					c.Commands = append(c.Commands, n)
					seen[n] = struct{}{}
					break
				}
			}
		}
	}

	c.LSRefsArgs = splitArgs(raw, "ls-refs")
	c.ObjectInfoArgs = splitArgs(raw, "object-info")
	c.FetchArgs = splitArgs(raw, "fetch")

	if v != ProtocolV2 {
		for _, val := range raw.All("symref") {
			name, target, ok := strings.Cut(val, ":")
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

package lsremote

import "github.com/hiddeco/go-ls-remote/transport"

// Ref names a single Git reference as it appears in the remote's
// discovery response.
//
// The exported fields mirror the wire representation directly so a
// caller can read whatever it needs without indirection:
//
//   - [Ref.Name] is the canonical ref name — `HEAD`, `refs/heads/main`,
//     `refs/tags/v1.0.0`, and so on.
//   - [Ref.Hash] is the hexadecimal object id the ref points at — 40
//     characters for `sha1` repositories and 64 for `sha256`. It is
//     the empty string for the unborn-`HEAD` case where a server
//     advertises `HEAD` with no object yet.
//   - [Ref.Peeled] is the hexadecimal object id of the underlying
//     commit when [Ref.Name] is an annotated tag and the server
//     supplied a peeled value (either inline via `^{}` on the v0/v1
//     wire or via the v2 `peel` argument to `ls-refs`). It is empty
//     when the ref is not a peeled annotated tag.
//   - [Ref.Symref] is the target ref name when this entry is a
//     symbolic reference and the server disclosed the target. It is
//     empty otherwise.
//
// Ref has no methods; callers read the exported fields directly.
type Ref struct {
	// Name is the ref name, e.g. `HEAD`, `refs/heads/main`,
	// `refs/tags/v1.0.0`.
	Name string

	// Hash is the hexadecimal object id the ref points at. It is
	// 40 characters for `sha1` and 64 characters for `sha256`,
	// or the empty string for an unborn `HEAD`.
	Hash string

	// Peeled is the hexadecimal object id of the peeled commit
	// for an annotated tag, or the empty string when [Ref.Name]
	// is not a peeled annotated tag.
	Peeled string

	// Symref is the target ref name when [Ref.Name] is a symbolic
	// reference and the server disclosed the target, or the empty
	// string otherwise.
	Symref string
}

// ObjectInfo carries the per-object metadata returned by the v2
// `object-info` command.
//
// [ObjectInfo.Hash] is the queried object id in lower-case
// hexadecimal. [ObjectInfo.Size] is the object's payload size in
// bytes when the caller requested it and the server returned a
// value, or `-1` when the size was not requested by the caller or
// not returned by the server. A real on-disk object can legitimately
// have a size of zero (an empty blob), so the negative sentinel is
// the only unambiguous "absent" marker.
//
// ObjectInfo has no methods; callers read the exported fields
// directly.
type ObjectInfo struct {
	// Hash is the queried object id in lower-case hexadecimal.
	Hash string

	// Size is the object's payload size in bytes, or `-1` when
	// the size was not requested or not returned by the server.
	Size int64
}

// Symref names a single symbolic-reference mapping that a v0/v1
// server advertised in its capability list — for example
// `HEAD → refs/heads/main`.
//
// v2 servers do not advertise symrefs at the capability level;
// they expose the same information inline on each [Ref] via
// [Ref.Symref] when the caller passes the `symrefs` argument to
// `ls-refs`. As a result [Capabilities.Symrefs] is populated only
// for v0/v1 handshakes and is left empty for v2.
//
// Symref has no methods; callers read the exported fields directly.
type Symref struct {
	// Name is the symbolic ref's own name, e.g. `HEAD`.
	Name string

	// Target is the ref name the symref points at, e.g.
	// `refs/heads/main`.
	Target string
}

// ObjectFormat identifies the cryptographic hash function a remote
// repository uses to name objects. The string values are the literal
// tokens Git puts on the wire (`sha1` for the historical default and
// `sha256` for the newer object format), so callers can compare
// against the constants without case folding.
type ObjectFormat string

const (
	// ObjectFormatSHA1 is the historical Git object format whose
	// object ids are 40-character hexadecimal SHA-1 digests.
	ObjectFormatSHA1 ObjectFormat = "sha1"

	// ObjectFormatSHA256 is the newer Git object format whose
	// object ids are 64-character hexadecimal SHA-256 digests.
	ObjectFormatSHA256 ObjectFormat = "sha256"
)

// Capabilities records what a remote advertised during the
// discovery handshake: the negotiated [ProtocolVersion], the server
// agent string, the repository's [ObjectFormat], and (for v2) the
// commands and per-command argument lists the server claims to
// support.
//
// # Field semantics
//
//   - [Capabilities.Version] is the protocol version actually
//     negotiated, not the one the caller requested.
//   - [Capabilities.Agent] is the `agent=` capability value the
//     server advertised, or the empty string when the server did
//     not send one.
//   - [Capabilities.ObjectFormat] is the negotiated repository
//     object format. v0/v1 servers commonly omit the `object-format`
//     capability; the field is then the empty string. Callers
//     comparing against [ObjectFormatSHA1] should treat the empty
//     string as equivalent — v0/v1 servers that do not advertise
//     `object-format` always speak `sha1`. Unknown values are
//     preserved verbatim so callers can detect future formats.
//   - [Capabilities.Commands] lists the v2 commands the server
//     advertised under the `command=` capability. It is empty for
//     v0/v1 handshakes, where command-style negotiation does not
//     exist.
//   - [Capabilities.LSRefsArgs], [Capabilities.ObjectInfoArgs], and
//     [Capabilities.FetchArgs] list the per-command arguments the
//     server claims to accept for `ls-refs`, `object-info`, and
//     `fetch` respectively. [Capabilities.FetchArgs] is recorded for
//     completeness; the library never issues a `fetch`.
//   - [Capabilities.Symrefs] lists `HEAD → refs/heads/...` style
//     mappings advertised in the v0/v1 capability list. v2 servers
//     surface symrefs inline on each [Ref] instead, so this slice
//     is empty for v2 handshakes.
//   - [Capabilities.Raw] holds every capability the server
//     advertised, keyed by capability name with each value's
//     argument(s) preserved verbatim. It is a `map[string][]string`
//     because the same capability name can appear multiple times in
//     a single advertisement — `symref=HEAD:refs/heads/main` and
//     `symref=refs/remotes/origin/HEAD:refs/heads/main`, for
//     example — and the slice keeps every occurrence in advertised
//     order. Capabilities advertised without a value (a bare
//     `multi_ack` token) appear as a one-element slice containing
//     the empty string.
//
// # Caller contract
//
// Callers must treat a returned Capabilities and any slice or map
// it contains as read-only. The library deep-copies the value
// before handing it back so mutation by the caller cannot corrupt
// internal state, but mutating the returned value is undefined and
// reserved for the library's own use.
//
// Capabilities has no methods; callers read the exported fields
// directly.
type Capabilities struct {
	// Version is the negotiated protocol version.
	Version ProtocolVersion

	// Agent is the server's `agent=` advertisement, or the empty
	// string when none was sent.
	Agent string

	// ObjectFormat is the negotiated repository object format.
	ObjectFormat ObjectFormat

	// Commands lists the v2 commands the server advertised. Empty
	// for v0/v1 handshakes.
	Commands []string

	// LSRefsArgs lists the per-command arguments the server
	// accepts on `ls-refs`.
	LSRefsArgs []string

	// ObjectInfoArgs lists the per-command arguments the server
	// accepts on `object-info`.
	ObjectInfoArgs []string

	// FetchArgs lists the per-command arguments the server
	// accepts on `fetch`. Recorded for completeness; the library
	// does not implement `fetch`.
	FetchArgs []string

	// Symrefs lists v0/v1 capability-level symref advertisements.
	// Empty for v2 handshakes.
	Symrefs []Symref

	// Raw is every advertised capability, verbatim, keyed by
	// capability name. The value slice preserves both repeated
	// advertisements and original order.
	Raw map[string][]string
}

// ProtocolVersion is a Go type alias of [transport.ProtocolVersion].
// The alias means the two are interchangeable without conversion:
// a `transport.ProtocolVersion` value is also a
// `lsremote.ProtocolVersion`, and the [ProtocolV0], [ProtocolV1],
// and [ProtocolV2] constants below resolve to the same
// package-level constants the transport package defines.
type ProtocolVersion = transport.ProtocolVersion

// Re-exported [ProtocolVersion] constants. Because the type is an
// alias, `lsremote.ProtocolV2 == transport.ProtocolV2` and the two
// names refer to the same constant value.
const (
	ProtocolV0 = transport.ProtocolV0
	ProtocolV1 = transport.ProtocolV1
	ProtocolV2 = transport.ProtocolV2
)

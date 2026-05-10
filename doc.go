// Package lsremote implements the discovery-time half of Git's wire
// protocols — the surface a caller exercises when they run
// `git ls-remote` — as a self-contained Go library with no shell-out
// to a `git` binary.
//
// The package speaks all three protocol versions (v0, v1, v2) over
// the transports Git itself supports: HTTP(S), SSH, the anonymous
// `git://` daemon, and local `file://` repositories. Version
// negotiation matches canonical Git: a caller may pin a preferred
// version via the [Option] helpers, but absent an explicit
// preference the library asks for v2 and gracefully falls back to
// v0 when the server only speaks the original protocol.
//
// The library exposes two layers. One-shot helpers — see [Refs],
// [ListRefs], and [ObjectInfos] — open a connection, run a single
// discovery command, and close it; they fit the common case where
// the caller only wants a ref list or a few object sizes. The
// session layer — see [Dial] and [Session] — keeps a connection
// open across multiple v2 commands so callers can issue [Session.Refs]
// followed by [Session.ObjectInfo] without re-handshaking.
//
// # Primary types
//
// [Ref] models a single entry in a discovery response: the ref name,
// its object id, an optional peeled object id for annotated tags, and
// an optional symref target when the entry is a symbolic reference.
//
// [Capabilities] records what the remote advertised during the
// handshake: the negotiated [ProtocolVersion], the server agent
// string, the [ObjectFormat] in use (`sha1` or `sha256`), and the
// per-command argument lists a v2 server claims to accept.
//
// [ObjectInfo] is the response shape for the v2 `object-info`
// command, pairing an object id with metadata such as its `Size`.
//
// [Symref] names a single `HEAD → refs/heads/...` style mapping as
// advertised by v0/v1 servers in their capability list. v2 servers
// surface the same information inline on each [Ref] via [Ref.Symref]
// instead, so v2 [Capabilities] leaves [Capabilities.Symrefs] empty.
//
// The [ProtocolVersion] identifier is a Go type alias of
// [transport.ProtocolVersion]; the two are interchangeable without
// conversion. The constants [ProtocolV0], [ProtocolV1], and
// [ProtocolV2] resolve to the same package-level constants the
// `transport` package defines.
package lsremote

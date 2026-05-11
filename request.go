package lsremote

// RefsRequest collects the per-command arguments for an `ls-refs`
// invocation — the inputs a caller passes to [Session.Refs],
// [Session.ListRefs], and the corresponding top-level helpers.
//
// RefsRequest is a plain data carrier with no methods. The zero value
// is valid and asks the server for every ref it would normally
// advertise, without peeled object ids, symref targets, or an unborn
// `HEAD`.
//
// # Field semantics
//
//   - [RefsRequest.Prefixes] restricts the returned refs to those whose
//     names begin with one of the listed strings. On the v2 wire the
//     filter is applied server-side: the library forwards each entry
//     verbatim as a `ref-prefix <value>` argument to `ls-refs`. On the
//     v0/v1 wire there is no equivalent capability, so the library
//     fetches the full advertisement and applies the filter client
//     side. Either way, an empty or nil [RefsRequest.Prefixes] means
//     "do not filter".
//   - [RefsRequest.Peel] asks the server to include the peeled object
//     id for annotated tags. On v2 it maps to the `peel` argument to
//     `ls-refs`; on v0/v1 the peeled value rides inline on the
//     advertisement (`<oid> <name>^{}`) regardless of this flag, and
//     the library keeps the flag's behaviour consistent by surfacing
//     the value on [Ref.Peeled] only when the caller asked for it.
//   - [RefsRequest.Symrefs] asks the server to disclose symref targets
//     alongside each [Ref]. On v2 it maps to the `symrefs` argument
//     to `ls-refs` and surfaces inline on [Ref.Symref]. On v0/v1 the
//     flag has no wire effect (the symref info already rides on the
//     capability list), but the library post-fills [Ref.Symref] from
//     [Capabilities.Symrefs] when the flag is set, unifying the
//     call-site experience with v2. When the flag is not set on v0/v1,
//     [Ref.Symref] is always empty; the raw mapping remains available
//     on [Capabilities.Symrefs] regardless.
//   - [RefsRequest.Unborn] asks a v2 server to advertise an unborn
//     `HEAD` — a `HEAD` whose target ref exists but holds no commit
//     yet. The flag maps to the `unborn` argument to `ls-refs` and is
//     honoured only when the server advertises `ls-refs=unborn` in
//     its capability list. On v0/v1 the wire has no concept of an
//     unborn `HEAD`, so the flag is ignored.
//
// See `gitprotocol-v2.adoc §"ls-refs"` for the v2 wire contract.
type RefsRequest struct {
	// Prefixes restricts the returned refs to those whose names
	// begin with one of the listed strings. Applied server-side on
	// v2 and client-side on v0/v1. An empty or nil slice means no
	// filtering.
	Prefixes []string

	// Peel asks the server to include peeled object ids for
	// annotated tags. On v2 this maps to the `peel` argument; on
	// v0/v1 peeled values ride inline on the advertisement.
	Peel bool

	// Symrefs asks the server to disclose symref targets alongside
	// each [Ref]. On v2 this maps to the `symrefs` argument and
	// surfaces on [Ref.Symref]. On v0/v1 the flag has no wire effect,
	// but the library post-fills [Ref.Symref] from
	// [Capabilities.Symrefs] when the flag is set.
	Symrefs bool

	// Unborn asks a v2 server to advertise an unborn `HEAD`. The
	// flag is honoured only when the server advertises
	// `ls-refs=unborn`. On v0/v1 the flag has no effect.
	Unborn bool
}

// ObjectInfoRequest collects the per-command arguments for an
// `object-info` invocation — the inputs a caller passes to
// [Session.ObjectInfo] and the corresponding top-level helper.
//
// ObjectInfoRequest is a plain data carrier with no methods. The zero
// value is valid and queries the listed object ids without asking for
// any per-object attributes.
//
// # Field semantics
//
//   - [ObjectInfoRequest.Size] asks the server to return the payload
//     size in bytes for each queried object id. It maps to the `size`
//     argument of the v2 `object-info` command and surfaces on
//     [ObjectInfo.Size]. `object-info` is a v2-only command; on
//     v0/v1 handshakes the library never issues it and this flag is
//     unused.
//
// See `gitprotocol-v2.adoc §"object-info"` for the v2 wire contract.
type ObjectInfoRequest struct {
	// Size asks the server to return the payload size in bytes for
	// each queried object id. Maps to the `size` argument of v2
	// `object-info` and surfaces on [ObjectInfo.Size].
	Size bool
}

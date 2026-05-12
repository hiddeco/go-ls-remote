package wire

// RefsArgs are the per-command arguments to a v2 `ls-refs` request.
// The fields mirror the body grammar in [gitprotocol-v2.adoc lines 197-228]:
// `peel` and `symrefs` are unconditional booleans, `unborn`
// is gated by the server advertising `ls-refs=unborn`, and `Prefixes`
// becomes one `ref-prefix <p>` argument per element in slice order.
//
// The struct lives in `internal/wire` for the moment; the eventual
// public `lsremote.RefsArgs` will be a thin alias once the root
// package exists.
//
// [gitprotocol-v2.adoc lines 197-228]: https://github.com/git/git/blob/v2.54.0/Documentation/gitprotocol-v2.adoc?plain=1#L197-L228
type RefsArgs struct {
	// Prefixes is the list of refname prefixes the server filters by;
	// each element becomes one `ref-prefix <p>` argument on the wire.
	Prefixes []string

	// Peel requests peeled-tag information for annotated tags
	// (`peel` argument).
	Peel bool

	// Symrefs requests symref-target information on `HEAD` and
	// branches (`symrefs` argument).
	Symrefs bool

	// Unborn requests unborn-`HEAD` reporting on empty repositories.
	// The encoder drops this argument silently when the server has
	// not advertised `ls-refs=unborn`; see `EncodeLSRefs`.
	Unborn bool
}

// ObjectInfoArgs are the per-command arguments to a v2 `object-info`
// request. Per [gitprotocol-v2.adoc §"object-info" (lines 556-585)],
// `size` is currently the only defined attribute; the server emits a
// matching `size` line per OID in the response when set.
//
// [gitprotocol-v2.adoc §"object-info" (lines 556-585)]: https://github.com/git/git/blob/v2.54.0/Documentation/gitprotocol-v2.adoc?plain=1#L556-L585
type ObjectInfoArgs struct {
	// Size requests `size` information for each OID. When false the
	// server still returns one row per OID, but with the size column
	// omitted.
	Size bool
}

// DefaultUserAgent is the agent string the encoder advertises when
// the caller passes an empty userAgent. It follows Git's
// `<name>/<major>` agent convention (see [version.c::git_user_agent]).
//
// [version.c::git_user_agent]: https://github.com/git/git/blob/v2.54.0/version.c#L28
const DefaultUserAgent = "lsremote/0"

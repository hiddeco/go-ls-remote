package wire

// RefsArgs are the per-command arguments to a v2 `ls-refs` request.
// The fields mirror the body grammar in `gitprotocol-v2.adoc` lines
// 197-228: `peel` and `symrefs` are unconditional booleans, `unborn`
// is gated by the server advertising `ls-refs=unborn`, and `Prefixes`
// becomes one `ref-prefix <p>` argument per element in slice order.
//
// The struct lives in `internal/wire` for the moment; the eventual
// public `lsremote.RefsArgs` will be a thin alias once the root
// package exists.
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

// DefaultUserAgent is the agent string the encoder advertises when
// the caller passes an empty userAgent. The version suffix is not
// appended yet; the root package will override this at session-build
// time once it exists.
const DefaultUserAgent = "go-ls-remote"

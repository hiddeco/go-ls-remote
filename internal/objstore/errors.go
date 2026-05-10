// Package objstore is the read-only on-disk Git object store. It
// resolves a working tree or gitdir to its canonical layout
// (`gitDir`, `commonDir`), opens the backends that hold refs and
// objects, and exposes the lookup primitives used by the higher-layer
// transport and protocol code.
//
// The package treats the on-disk repository as a black box of bytes:
// no commits are written, no refs are mutated, and no `git` binary is
// invoked. All format parsing is delegated to `internal/objfmt` and
// `internal/reftable`; this package layers naming, indirection, and
// caching on top.
package objstore

import "errors"

// Sentinel errors returned by this package. Callers match against
// these with [errors.Is]; the wrapping `fmt.Errorf("...: %w", ...,
// sentinel)` carries the offending path or value for diagnostics.
var (
	// ErrNotARepo is returned by [resolveGitDir] when the supplied
	// path is neither a gitdir, a working tree containing a `.git`
	// directory, nor a working tree containing a `.git` file with a
	// well-formed `gitdir:` directive.
	ErrNotARepo = errors.New("objstore: not a git repository")

	// ErrUnsupportedFormat is returned by [readGitConfig] when an
	// `extensions.*` value names a format this implementation does
	// not handle. It surfaces in three situations:
	//
	//  - `extensions.objectFormat` is neither `sha1` nor `sha256`.
	//  - `extensions.refStorage` is a bare format name other than
	//    `files` or `reftable`.
	//  - `extensions.refStorage` is a `<format>://<payload>` URI
	//    whose format prefix is not `files` or `reftable`.
	//
	// The wrapping `fmt.Errorf` carries the offending value so
	// diagnostics can include both the key and the rejected token.
	ErrUnsupportedFormat = errors.New("objstore: unsupported format")

	// ErrCorruptObject is returned by object-lookup paths when an
	// object is structurally unresolvable: missing from every backend,
	// or present but failing CRC32 / hash verification, or part of a
	// delta chain whose base cannot be found. Callers match against it
	// with [errors.Is]; the wrapping `fmt.Errorf` carries the offending
	// hash and pack location for diagnostics.
	ErrCorruptObject = errors.New("objstore: object corrupt or unresolvable")

	// ErrAlgoMismatch is returned by [Open] when the type-parameter
	// `H` of the requested store does not match the on-disk
	// `extensions.objectFormat`. This guards programmer error at
	// callsites that hard-code `H` rather than dispatching on
	// [DiscoverAlgo]; the transport layer in `transport/{file,http}`
	// goes through that dispatch and so never produces the mismatch
	// in production.
	ErrAlgoMismatch = errors.New("objstore: hash type does not match repository format")
)

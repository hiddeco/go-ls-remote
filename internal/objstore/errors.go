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
)

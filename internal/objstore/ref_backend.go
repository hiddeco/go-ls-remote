package objstore

import (
	"iter"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
)

// refBackend is the contract every ref-storage implementation honours.
// The two concrete backends are the loose-files reader (`refs/`,
// `packed-refs`) and the reftable reader (`reftable/tables.list`); the
// store opener picks one or the other based on `extensions.refStorage`
// and treats the rest of the lookup pipeline as backend-agnostic.
//
// All methods are safe for concurrent use by multiple goroutines once
// the backend has been constructed; mutation of the underlying on-disk
// state is out of scope for this library.
type refBackend interface {
	// Head returns the resolved [Head] of the repository.
	Head() (Head, error)

	// IterRefs yields every fully-qualified ref the backend exposes,
	// in an unspecified order. The iterator stops on the first error.
	IterRefs() iter.Seq2[RefEntry, error]

	// Lookup resolves name to a single [RefEntry]. The found return is
	// false when the backend has no entry by that name; the error slot
	// is reserved for backend faults (decode errors, transient I/O) and
	// is not produced by a missing name.
	//
	// Implementations must populate [RefEntry.Peeled] and
	// [RefEntry.PeelKnown] using the same rules as [refBackend.IterRefs].
	Lookup(name string) (RefEntry, bool, error)

	// Close releases any resources held by the backend (file handles,
	// memory mappings). It must be safe to call exactly once; idempotency
	// is the [Store]'s responsibility, not the backend's.
	Close() error
}

// Head describes the resolved value of `HEAD`.
//
// Either Symref or OID is set depending on whether HEAD is a symbolic
// reference (`ref: refs/heads/<branch>`) or detached (a literal OID).
// Unborn signals the canonical "symbolic ref points at a branch with
// no commits yet" shape — the only state where neither field is
// meaningful on its own and the caller must special-case the response.
type Head struct {
	// Symref is the fully-qualified target of a symbolic HEAD, e.g.
	// `refs/heads/main`. Empty for a detached HEAD.
	Symref string

	// OID is the resolved object id. Zero for an [Head.Unborn] symbolic
	// HEAD; populated otherwise.
	OID objfmt.Hash

	// Unborn is true when HEAD is a symbolic ref pointing at a branch
	// that does not yet exist in the ref backend. Canonical Git treats
	// this as `ref: refs/heads/<x>` with no `refs/heads/<x>` file or
	// reftable entry.
	Unborn bool
}

// RefEntry pairs a fully-qualified ref name with its resolved object
// id. Symbolic refs other than `HEAD` are dereferenced to their
// terminal OID before being yielded; the iterator never surfaces
// `ref: ...` payloads.
type RefEntry struct {
	// Name is the fully-qualified ref name, e.g. `refs/heads/main`,
	// `refs/tags/v1.0`, `refs/remotes/origin/main`.
	Name string

	// OID is the resolved object id of the ref's terminal target.
	OID objfmt.Hash

	// Peeled is the dereferenced commit id when the backend can answer
	// without performing object I/O; the zero hash otherwise. A non-zero
	// Peeled is only meaningful when [RefEntry.PeelKnown] is true.
	Peeled objfmt.Hash

	// PeelKnown reports whether the backend definitively knows the
	// peel state for this ref without reading the object body. The
	// three observable shapes are:
	//
	//   - PeelKnown && !Peeled.IsZero(): peelable annotated tag whose
	//     terminal target is Peeled.
	//   - PeelKnown && Peeled.IsZero(): the ref has no peel (commit,
	//     tree, blob, lightweight tag).
	//   - !PeelKnown: the backend cannot answer cheaply; callers that
	//     need the peel must reach for [Store.Peel] (or
	//     [Store.PeelRef], which folds the lookup and the fall-through
	//     into one call).
	//
	// The loose-refs backend sets PeelKnown when a `^<oid>` line follows
	// the entry in `packed-refs` OR when the file's `# pack-refs with:`
	// header advertises the `fully-peeled` trait — under that trait the
	// absence of `^<oid>` is itself authoritative. The reftable backend
	// always sets PeelKnown=true: every reftable ref record carries its
	// peel slot (zero or set), so the merged-view lookup is definitive.
	PeelKnown bool
}

// Head returns the resolved [Head] of the repository by delegating to
// the configured ref backend.
func (s *Store) Head() (Head, error) {
	return s.refs.Head()
}

// IterRefs yields every ref the configured backend exposes. The
// iteration order is backend-defined; callers that need a stable order
// must sort the result themselves.
func (s *Store) IterRefs() iter.Seq2[RefEntry, error] {
	return s.refs.IterRefs()
}

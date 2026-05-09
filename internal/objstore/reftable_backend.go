package objstore

import (
	"fmt"
	"iter"
	"path/filepath"

	"github.com/hiddeco/go-ls-remote/internal/reftable"
)

// reftableBackend reads refs from the reftable on-disk format,
// selected when `extensions.refStorage` is `reftable` (with or without
// a `<format>://<payload>` URI). Canonical Git stores the index of
// active tables in `<location>/tables.list`; the actual table files
// live alongside it. Decoding is delegated to [reftable.Stack]; this
// type only owns the gitDir/commonDir resolution and the lift from
// [reftable.RefRecord] to [RefEntry] and [Head].
//
// HEAD is resolved eagerly at construction so [reftableBackend.Head]
// is I/O-free, mirroring the discipline [looseRefs] established. The
// stack itself is held open for [reftableBackend.IterRefs] (which
// re-walks the cached merged view on every call) and released by
// [reftableBackend.Close].
type reftableBackend struct {
	gitDir, commonDir, location string
	stack                       *reftable.Stack
	head                        Head
}

// openReftableBackend constructs a [reftableBackend] rooted at the
// repository's reftable directory.
//
// The reftable directory is selected by the location payload:
//
//   - When location is empty, the canonical
//     `<commonDir>/reftable/` layout is used.
//   - When location is absolute, it is consumed verbatim.
//   - When location is relative, it is resolved against gitDir
//     (not commonDir), per canonical Git's
//     `Documentation/config/extensions.adoc` § `extensions.refStorage`:
//     "if relative, it is interpreted relative to `$GIT_DIR`".
//
// The stack's hash algorithm is intentionally not validated against
// the repository's `extensions.objectFormat` here — the constructor
// does not receive the algo (the existing skeleton fixed that shape),
// and a mismatch only manifests when objects are looked up against
// pack/loose storage. Surfacing it later, with the offending OID in
// hand, gives a more useful diagnostic than rejecting the open.
func openReftableBackend(gitDir, commonDir, location string) (*reftableBackend, error) {
	reftableDir := resolveReftableDir(gitDir, commonDir, location)

	stack, err := reftable.OpenStack(reftableDir)
	if err != nil {
		return nil, fmt.Errorf("objstore: open reftable %s: %w", reftableDir, err)
	}

	b := &reftableBackend{
		gitDir:    gitDir,
		commonDir: commonDir,
		location:  location,
		stack:     stack,
	}

	head, err := resolveReftableHead(stack)
	if err != nil {
		_ = stack.Close()
		return nil, err
	}
	b.head = head

	return b, nil
}

// resolveReftableDir applies the location-resolution rules documented
// on [openReftableBackend]. Kept separate so the (gitDir, commonDir,
// location) → reftableDir mapping is testable in isolation if a future
// regression makes that worthwhile.
func resolveReftableDir(gitDir, commonDir, location string) string {
	if location == "" {
		return filepath.Join(commonDir, "reftable")
	}
	if filepath.IsAbs(location) {
		return filepath.Clean(location)
	}
	return filepath.Clean(filepath.Join(gitDir, location))
}

// resolveReftableHead reads the HEAD record from stack and lifts it
// into a [Head]. Reftable repos always carry a HEAD record (canonical
// Git writes one at `git init`, before any commit lands); a missing
// HEAD is therefore corruption rather than the unborn shape — that
// state is encoded as `HEAD = symref refs/heads/<name>` with the
// target absent from the stack.
//
// The accepted record shapes mirror canonical Git's reftable backend:
//
//   - `TargetRef != ""` — symbolic HEAD. The target is looked up in
//     the stack; a hit yields Symref + OID, a miss yields Symref +
//     Unborn.
//   - `TargetRef == "" && Value != zero` — detached HEAD. Symref empty,
//     OID populated.
//   - both empty — corruption (an on-disk record that is neither a
//     symref nor a value record cannot be interpreted).
func resolveReftableHead(stack *reftable.Stack) (Head, error) {
	rec, found, err := stack.FindRef("HEAD")
	if err != nil {
		return Head{}, fmt.Errorf("objstore: read HEAD: %w", err)
	}
	if !found {
		return Head{}, fmt.Errorf(
			"objstore: HEAD record absent from reftable stack: %w", ErrCorruptObject)
	}

	if rec.TargetRef != "" {
		target, ok, err := stack.FindRef(rec.TargetRef)
		if err != nil {
			return Head{}, fmt.Errorf(
				"objstore: read HEAD target %q: %w", rec.TargetRef, err)
		}
		if !ok {
			return Head{Symref: rec.TargetRef, Unborn: true}, nil
		}
		return Head{Symref: rec.TargetRef, OID: target.Value}, nil
	}

	if !rec.Value.IsZero() {
		return Head{OID: rec.Value}, nil
	}

	return Head{}, fmt.Errorf(
		"objstore: HEAD record present but empty: %w", ErrCorruptObject)
}

// Head returns the cached [Head] resolved at construction. No I/O.
func (b *reftableBackend) Head() (Head, error) { return b.head, nil }

// IterRefs yields every observable ref from the reftable stack in
// lexical order. The iterator skips HEAD and any other symref records:
// HEAD is exposed through [reftableBackend.Head], and surfacing other
// symrefs through [RefEntry] would require the type to carry a target
// — out of scope for v0, and consistent with [looseRefs]'s precedent
// of dropping non-HEAD symrefs from `refs/`.
//
// The error slot mirrors the upstream [reftable.Stack.IterRefs]
// contract: a decode failure short-circuits the walk and is forwarded
// to the consumer. Today the upstream iterator never errors at walk
// time (every reader's bytes are validated at OpenStack), but the
// slot is preserved for forward compatibility.
func (b *reftableBackend) IterRefs() iter.Seq2[RefEntry, error] {
	return func(yield func(RefEntry, error) bool) {
		for rec, err := range b.stack.IterRefs() {
			if err != nil {
				yield(RefEntry{}, err)
				return
			}
			if rec.Name == "HEAD" {
				continue
			}
			// Symrefs other than HEAD are dropped; see the doc comment.
			if rec.TargetRef != "" {
				continue
			}
			if !yield(RefEntry{Name: rec.Name, OID: rec.Value}, nil) {
				return
			}
		}
	}
}

// Close releases the wrapped [reftable.Stack]. Close errors propagate
// from the stack unchanged; the wrapping read-only contract leaves no
// objstore-level resources to release here.
func (b *reftableBackend) Close() error {
	if b.stack == nil {
		return nil
	}
	return b.stack.Close()
}

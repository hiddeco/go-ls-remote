package objstore

import (
	"iter"
)

// looseRefs reads refs from the canonical files-backed layout:
// `<commonDir>/refs/...` plus `<commonDir>/packed-refs`. The backend
// is selected when `extensions.refStorage` is `files` (the default).
//
// This file carries the type and constructor only. Iteration, peeling,
// and packed-refs parsing land in a follow-up — keeping the skeleton
// here lets the store opener compile and lets tests exercise the
// backend selector without touching ref-reading semantics.
type looseRefs struct {
	gitDir    string
	commonDir string
}

// openLooseRefs constructs a [looseRefs] backed by the given dirs.
// It must succeed on a well-formed empty repository; per-ref errors
// surface from [looseRefs.Head] and [looseRefs.IterRefs] later.
func openLooseRefs(gitDir, commonDir string) (*looseRefs, error) {
	return &looseRefs{gitDir: gitDir, commonDir: commonDir}, nil
}

// Head reports an unborn HEAD by default. The real implementation
// reads `<gitDir>/HEAD` and resolves any symbolic chain.
func (r *looseRefs) Head() (Head, error) {
	return Head{Unborn: true}, nil
}

// IterRefs yields nothing by default. The real implementation walks
// `<commonDir>/refs/` and merges `<commonDir>/packed-refs`.
func (r *looseRefs) IterRefs() iter.Seq2[RefEntry, error] {
	return func(yield func(RefEntry, error) bool) {}
}

// Close releases the backend. The skeleton holds no resources.
func (r *looseRefs) Close() error { return nil }

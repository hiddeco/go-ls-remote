package objstore

import (
	"iter"
)

// reftableBackend reads refs from the reftable on-disk format,
// selected when `extensions.refStorage` is `reftable` (with or without
// a `<format>://<payload>` URI). Canonical Git stores the index of
// active tables in `<location>/tables.list`; the actual table files
// live alongside it.
//
// This file carries the type and constructor only. Stack reading,
// reverse-index lookup, and ref enumeration land in a follow-up.
type reftableBackend struct {
	gitDir    string
	commonDir string
	location  string // raw payload from `extensions.refStorage`
}

// openReftableBackend constructs a [reftableBackend]. The location
// payload is taken verbatim — relative paths are resolved against the
// commonDir by the real implementation, but the skeleton just stores
// the raw string so the opener has a place to thread the value through.
func openReftableBackend(gitDir, commonDir, location string) (*reftableBackend, error) {
	return &reftableBackend{
		gitDir:    gitDir,
		commonDir: commonDir,
		location:  location,
	}, nil
}

// Head reports an unborn HEAD by default. The real implementation
// reads `HEAD` (still a loose file even with reftable refs) and
// dereferences through the reftable stack.
func (b *reftableBackend) Head() (Head, error) {
	return Head{Unborn: true}, nil
}

// IterRefs yields nothing by default. The real implementation walks
// the reftable stack with tombstone-aware semantics.
func (b *reftableBackend) IterRefs() iter.Seq2[RefEntry, error] {
	return func(yield func(RefEntry, error) bool) {}
}

// Close releases the backend. The skeleton holds no resources.
func (b *reftableBackend) Close() error { return nil }

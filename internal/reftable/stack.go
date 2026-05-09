package reftable

import (
	"bytes"
	"errors"
	"fmt"
	"iter"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
)

// Sentinel errors returned by the stack reader. Callers match against
// these with [errors.Is]; the wrapping `fmt.Errorf("...: %w", ..., sentinel)`
// adds context (offending line, mismatching algos) for diagnostics.
var (
	// ErrMixedHashAlgo is returned by [OpenStack] when the readers it
	// composes do not all declare the same [objfmt.Algo]. A reftable
	// stack is, by construction, single-algorithm: a SHA-1 repository's
	// reftables are all SHA-1, a SHA-256 repository's all SHA-256.
	// Mixed bytes on disk indicate either repository corruption or a
	// developer mistake; either way, refusing to open is safer than
	// letting hash-size mismatches propagate into record decoding.
	ErrMixedHashAlgo = errors.New("reftable: stack has mixed hash algorithms")

	// ErrInvalidTablesList is returned when `tables.list` cannot be
	// parsed as a sequence of newline-separated basenames. The most
	// common cause is an empty middle line (a corrupted manifest);
	// canonical Git's `reftable/stack.c::read_lines` likewise rejects
	// this shape.
	ErrInvalidTablesList = errors.New("reftable: invalid tables.list")
)

// Stack is a read-only view over the ordered set of reftable files
// listed by a single `tables.list` manifest.
//
// At [OpenStack] time, every reader is opened and the merged ref view
// is materialized into an in-memory map: walking each table in stack
// order (oldest first, newest last) and either inserting a record or,
// for tombstones, deleting the corresponding name. Lookups thereafter
// hit the map directly. This trades canonical Git's streaming
// priority-queue iterator for O(1) [Stack.FindRef] and constant work
// per record at open; ls-remote consumers want the merged answer for
// every ref, so we pay the merge cost once.
//
// Stack does not parse log records, OID-to-ref indexes, or anything
// outside the ref namespace: those live outside the public surface
// described by [Stack.IterRefs] and [Stack.FindRef].
//
// # Concurrency
//
// After [OpenStack] returns, the read methods ([Stack.HashAlgo],
// [Stack.IterRefs], [Stack.FindRef]) are safe for concurrent use by
// any number of goroutines: the merged map and the underlying readers
// are populated once at construction and never written again.
// [Stack.Close] is NOT safe to call concurrently with read methods or
// with itself; callers must drain in-flight reads before closing and
// serialize Close calls. Once drained, Close is idempotent — a second
// call returns nil without touching the OS.
type Stack struct {
	readers []*Reader            // [0] = oldest table, [n-1] = newest
	merged  map[string]RefRecord // pre-computed merged view
	algo    objfmt.Algo          // taken from the first reader; Algo(0) when empty
}

// OpenStack reads `<reftableDir>/tables.list`, opens each listed
// reftable as a [Reader], and pre-computes the merged ref view.
//
// `tables.list` is a sequence of newline-separated basenames; a single
// trailing newline is tolerated. An empty file is a valid empty stack.
// Empty middle lines indicate a corrupted manifest and surface as
// [ErrInvalidTablesList]. See canonical Git's
// `reftable/stack.c::read_lines` for the wire-compatible parser.
//
// All readers must share the same [objfmt.Algo]; mismatches surface as
// [ErrMixedHashAlgo] after every reader has been closed. Errors from
// individual [OpenReader] calls propagate unwrapped (e.g. fs.ErrNotExist
// when a basename in the manifest does not exist on disk); already
// opened readers are closed before the error returns.
//
// On successful return the caller owns the [Stack] and must release it
// with [Stack.Close].
func OpenStack(reftableDir string) (*Stack, error) {
	manifest := filepath.Join(reftableDir, "tables.list")
	raw, err := os.ReadFile(manifest)
	if err != nil {
		return nil, fmt.Errorf("reftable: read %s: %w", manifest, err)
	}

	names, err := parseTablesList(raw)
	if err != nil {
		return nil, err
	}

	// Empty stack: no readers, no merged entries, Algo(0).
	if len(names) == 0 {
		return &Stack{merged: map[string]RefRecord{}}, nil
	}

	readers := make([]*Reader, 0, len(names))
	closeAll := func() {
		for _, r := range readers {
			_ = r.Close()
		}
	}

	for _, basename := range names {
		r, err := OpenReader(filepath.Join(reftableDir, basename))
		if err != nil {
			closeAll()
			return nil, err
		}
		readers = append(readers, r)
	}

	algo := readers[0].HashAlgo()
	for _, r := range readers[1:] {
		if r.HashAlgo() != algo {
			closeAll()
			return nil, fmt.Errorf("reftable: %s vs %s: %w", algo, r.HashAlgo(), ErrMixedHashAlgo)
		}
	}

	merged := make(map[string]RefRecord)
	for _, r := range readers {
		for rec, err := range r.iterAllRefs() {
			if err != nil {
				closeAll()
				return nil, err
			}
			if rec.ValueType == refValueDeletion {
				delete(merged, rec.Name)
				continue
			}
			merged[rec.Name] = liftRefRecord(rec)
		}
	}

	return &Stack{readers: readers, merged: merged, algo: algo}, nil
}

// parseTablesList splits a `tables.list` payload into basenames.
//
// The format is a strict sequence of `<basename>\n` records, with a
// single trailing newline tolerated. Empty middle elements signal a
// corrupted manifest and yield [ErrInvalidTablesList]; canonical Git
// rejects the same shape in `reftable/stack.c::read_lines`.
func parseTablesList(raw []byte) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	// Trim a single trailing newline so `a\nb\n` and `a\nb` parse the
	// same. Anything beyond one trailing newline (e.g. `a\nb\n\n`)
	// surfaces as an empty middle line and fails the loop below.
	trimmed := raw
	if trimmed[len(trimmed)-1] == '\n' {
		trimmed = trimmed[:len(trimmed)-1]
	}

	parts := bytes.Split(trimmed, []byte{'\n'})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if len(p) == 0 {
			return nil, fmt.Errorf("reftable: empty line in tables.list: %w", ErrInvalidTablesList)
		}
		out = append(out, string(p))
	}
	return out, nil
}

// Close releases every underlying [Reader].
//
// Close is idempotent: a second call on an already-closed Stack
// returns nil without touching the OS. The first non-nil reader error
// is returned; subsequent reader closes still run so no mapping is
// leaked. Close is NOT safe to call concurrently with read methods or
// with itself; see the [Stack] concurrency contract.
func (s *Stack) Close() error {
	if s.readers == nil {
		return nil
	}
	rs := s.readers
	s.readers = nil
	s.merged = nil
	var firstErr error
	for _, r := range rs {
		if err := r.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// HashAlgo returns the hash algorithm shared by every reader in the
// stack.
//
// For an empty stack (no entries in `tables.list`), HashAlgo returns
// [objfmt.Algo](0): the zero value, which is invalid per
// [objfmt.Algo.Size]. Callers that need to handle the empty case can
// check `s.HashAlgo().Size() == 0` or count [Stack.IterRefs] yields.
func (s *Stack) HashAlgo() objfmt.Algo {
	return s.algo
}

// IterRefs returns an iterator that yields every observable ref in the
// merged stack view, sorted lexicographically by name.
//
// The merge runs once at [OpenStack] time; iteration walks the
// pre-built map and is therefore O(N log N) for the sort plus O(N) for
// the yield. Sorting trades a small per-call cost for deterministic
// output that matches reftable's own on-disk ordering and keeps tests
// stable across Go runtime map-iteration changes.
//
// Errors short-circuit the walk: on any decode failure produced during
// iteration (currently never — all errors surface at OpenStack time),
// the iterator yields one (RefRecord{}, err) pair and stops.
//
// Breaking out of the range loop is supported.
func (s *Stack) IterRefs() iter.Seq2[RefRecord, error] {
	return func(yield func(RefRecord, error) bool) {
		for _, name := range slices.Sorted(maps.Keys(s.merged)) {
			if !yield(s.merged[name], nil) {
				return
			}
		}
	}
}

// FindRef looks up name in the merged stack view.
//
// The boolean return distinguishes "no match" (false, nil) from
// "match" (true, nil) and "lookup error" (false, non-nil err). A name
// shadowed by a tombstone in a later table is reported as no-match:
// the merge step removed it from the view at OpenStack time. The error
// return is reserved for future use; today this method does not fail.
func (s *Stack) FindRef(name string) (RefRecord, bool, error) {
	rec, ok := s.merged[name]
	return rec, ok, nil
}

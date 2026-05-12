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
	// ErrMixedHashAlgo is returned when the on-disk hash algorithm of a
	// reftable file does not match the [objfmt.Hash] the reader was
	// instantiated for. A reftable stack is, by construction, single-
	// algorithm: a SHA-1 repository's reftables are all SHA-1, a SHA-256
	// repository's all SHA-256. Mixed bytes on disk indicate either
	// repository corruption or a developer mistake; either way, refusing
	// to open is safer than letting hash-size mismatches propagate into
	// record decoding.
	ErrMixedHashAlgo = errors.New("reftable: stack has mixed hash algorithms")

	// ErrInvalidTablesList is returned when `tables.list` cannot be
	// parsed as a sequence of newline-separated basenames. The most
	// common cause is an empty middle line (a corrupted manifest);
	// canonical Git's [reftable/stack.c::read_lines] likewise rejects
	// this shape.
	//
	// [reftable/stack.c::read_lines]: https://github.com/git/git/blob/v2.54.0/reftable/stack.c#L111
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
type Stack[H objfmt.Hash] struct {
	readers []*Reader[H]            // [0] = oldest table, [n-1] = newest
	merged  map[string]RefRecord[H] // pre-computed merged view
	sorted  []string                // ref names sorted lexicographically; cached for IterRefs
	algo    objfmt.Algo             // taken from the first reader; nil when empty
}

// OpenStack reads `<reftableDir>/tables.list`, opens each listed
// reftable as a [Reader], and pre-computes the merged ref view.
//
// `tables.list` is a sequence of newline-separated basenames; a single
// trailing newline is tolerated. An empty file is a valid empty stack.
// Empty middle lines indicate a corrupted manifest and surface as
// [ErrInvalidTablesList]. See canonical Git's
// [reftable/stack.c::read_lines] for the wire-compatible parser.
//
// Every reader is opened under the stack's static hash type `H`; a file
// whose on-disk hash algorithm does not match surfaces as
// [ErrMixedHashAlgo] from [OpenReader] before its bytes feed the merge.
// Errors from individual [OpenReader] calls propagate unwrapped
// otherwise (e.g. fs.ErrNotExist when a basename in the manifest does
// not exist on disk); already opened readers are closed before the
// error returns.
//
// On successful return the caller owns the [Stack] and must release it
// with [Stack.Close].
//
// [reftable/stack.c::read_lines]: https://github.com/git/git/blob/v2.54.0/reftable/stack.c#L111
func OpenStack[H objfmt.Hash](reftableDir string) (*Stack[H], error) {
	manifest := filepath.Join(reftableDir, "tables.list")
	raw, err := os.ReadFile(manifest)
	if err != nil {
		return nil, fmt.Errorf("reftable: read %s: %w", manifest, err)
	}

	names, err := parseTablesList(raw)
	if err != nil {
		return nil, err
	}

	// Empty stack: no readers, no merged entries, nil algo. The type
	// parameter `H` is statically fixed by the call site, but with no
	// reader to read it from there is no identity-only [objfmt.Algo]
	// value to return either; callers compare `HashAlgo() == nil` (or
	// use [Stack.Len]) for the empty case.
	if len(names) == 0 {
		return &Stack[H]{merged: map[string]RefRecord[H]{}, sorted: []string{}}, nil
	}

	readers := make([]*Reader[H], 0, len(names))
	closeAll := func() {
		for _, r := range readers {
			_ = r.Close()
		}
	}

	for _, basename := range names {
		r, err := OpenReader[H](filepath.Join(reftableDir, basename))
		if err != nil {
			closeAll()
			return nil, err
		}
		readers = append(readers, r)
	}

	algo := readers[0].HashAlgo()

	merged := make(map[string]RefRecord[H])
	for _, r := range readers {
		for rec, err := range r.iterAllRefs() {
			if err != nil {
				closeAll()
				return nil, err
			}
			if rec.ValueType == refValueDeletion {
				delete(merged, string(rec.Name))
				continue
			}
			// Take ownership of Name's bytes via a single allocation
			// that backs both the merged-map key and the cached []byte
			// Name. The conversion `string(rec.Name)` clones the
			// scratch-aliased bytes onto the heap; `asReadOnlyBytes`
			// then exposes that same memory as a []byte view for the
			// cached Name field. The Name slice and the map key alias
			// the same string-backed memory and are valid for the
			// Stack's lifetime, read-only.
			//
			// Target aliases the Reader's underlying file (retained by
			// Stack), so no clone is needed there.
			name := string(rec.Name)
			rec.Name = asReadOnlyBytes(name)
			merged[name] = liftRefRecord(rec)
		}
	}

	// Sort once at construction; [Stack.IterRefs] yields straight from
	// this slice on every call. The merged map is immutable after Open,
	// so callers never observe a stale order.
	sorted := slices.Sorted(maps.Keys(merged))

	return &Stack[H]{readers: readers, merged: merged, sorted: sorted, algo: algo}, nil
}

// parseTablesList splits a `tables.list` payload into basenames.
//
// The format is a strict sequence of `<basename>\n` records, with a
// single trailing newline tolerated. Empty middle elements signal a
// corrupted manifest and yield [ErrInvalidTablesList]; canonical Git
// rejects the same shape in [reftable/stack.c::read_lines].
//
// [reftable/stack.c::read_lines]: https://github.com/git/git/blob/v2.54.0/reftable/stack.c#L111
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
func (s *Stack[H]) Close() error {
	if s.readers == nil {
		return nil
	}
	rs := s.readers
	s.readers = nil
	s.merged = nil
	s.sorted = nil
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
// nil: the [objfmt.Algo] interface's zero value. Callers handling the
// empty case should use [Stack.Len] rather than comparing the result
// against a known algo.
func (s *Stack[H]) HashAlgo() objfmt.Algo {
	return s.algo
}

// Len returns the number of observable refs in the merged stack view.
//
// Len returns zero both for the empty-stack case (no entries in
// `tables.list`) and for a non-empty stack whose every record was
// shadowed by a tombstone: ls-remote consumers care only whether any
// refs are observable, and Len answers that without forcing a walk.
// Callers that specifically need to detect the empty-stack case can
// check `s.HashAlgo() == nil` alongside Len.
func (s *Stack[H]) Len() int {
	return len(s.merged)
}

// IterRefs returns an iterator that yields every observable ref in the
// merged stack view, sorted lexicographically by name.
//
// The merge and the sort both run once at [OpenStack] time; iteration
// walks the cached order and is therefore O(N) per call. Deterministic
// output matches reftable's own on-disk ordering and keeps tests stable
// across Go runtime map-iteration changes.
//
// Errors short-circuit the walk: on any decode failure produced during
// iteration (currently never — all errors surface at OpenStack time),
// the iterator yields one (RefRecord{}, err) pair and stops.
//
// Breaking out of the range loop is supported.
func (s *Stack[H]) IterRefs() iter.Seq2[RefRecord[H], error] {
	return func(yield func(RefRecord[H], error) bool) {
		for _, name := range s.sorted {
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
func (s *Stack[H]) FindRef(name string) (RefRecord[H], bool, error) {
	rec, ok := s.merged[name]
	return rec, ok, nil
}

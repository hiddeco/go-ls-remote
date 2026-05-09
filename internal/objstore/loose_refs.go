package objstore

import (
	"errors"
	"fmt"
	"io/fs"
	"iter"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
)

// looseRefs reads refs from the canonical files-backed layout:
// `<commonDir>/refs/...` plus `<commonDir>/packed-refs`. The backend
// is selected when `extensions.refStorage` is `files` (the default).
//
// Everything is read once at construction and cached. The constructor
// parses `packed-refs`, walks `refs/` for loose entries, lets the loose
// set override packed entries of the same name, resolves HEAD against
// the resulting map, and stores the materialized state on the struct.
// The eager build avoids the consistency hazards a streaming reader
// would face (a ref disappearing between `IterRefs` and a subsequent
// `Head` call) and keeps every post-Open access I/O-free, which matters
// for the read-fanout patterns the upper layers favour.
//
// Loose-overrides-packed mirrors canonical Git's
// `refs/files-backend.c::loose_fill_ref_dir` precedence: when the same
// name exists in both places the loose copy is authoritative. The peel
// hint from `packed-refs` is intentionally dropped on override because
// loose ref files do not encode peel state and we have no way to verify
// the packed peel still matches the loose OID.
type looseRefs struct {
	gitDir    string                 // for HEAD reading
	commonDir string                 // for refs/ + packed-refs reading
	algo      objfmt.Algo            // hash algorithm bound to the store
	refs      map[string]packedEntry // built once at construction
	head      Head                   // resolved at construction
	traits    packedTraits           // copied from packed-refs header
	sorted    []string               // ref names in lexical order
}

// openLooseRefs constructs a [looseRefs] backed by the given dirs. The
// constructor reads `<commonDir>/packed-refs` (silent when absent),
// walks `<commonDir>/refs/` for loose entries, and resolves HEAD. Any
// malformed input surfaces as an error wrapping [ErrCorruptObject];
// the constructor has no way to recover from a half-listed ref set so
// errors propagate up rather than being recorded for later iteration.
//
// algo selects the hex width for ref OIDs (40 chars for SHA-1, 64 for
// SHA-256) and is propagated to the packed-refs parser. The returned
// `*looseRefs` satisfies [refBackend].
func openLooseRefs(gitDir, commonDir string, algo objfmt.Algo) (*looseRefs, error) {
	r := &looseRefs{
		gitDir:    gitDir,
		commonDir: commonDir,
		algo:      algo,
	}

	packed, err := readPackedRefsFile(commonDir, algo)
	if err != nil {
		return nil, err
	}
	r.traits = packed.traits
	r.refs = packed.refs

	if err := r.walkLooseRefs(); err != nil {
		return nil, err
	}

	r.sorted = make([]string, 0, len(r.refs))
	for name := range r.refs {
		r.sorted = append(r.sorted, name)
	}
	slices.Sort(r.sorted)

	head, err := r.resolveHead()
	if err != nil {
		return nil, err
	}
	r.head = head

	return r, nil
}

// readPackedRefsFile opens `<commonDir>/packed-refs` and returns the
// parsed [packedRefs]. A missing file is not an error — it yields an
// empty map and zero traits, the canonical "no packed refs yet" shape.
func readPackedRefsFile(commonDir string, algo objfmt.Algo) (packedRefs, error) {
	path := filepath.Join(commonDir, "packed-refs")
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return packedRefs{refs: make(map[string]packedEntry)}, nil
		}
		return packedRefs{}, fmt.Errorf("objstore: open %s: %w", path, err)
	}
	defer f.Close()

	parsed, err := parsePackedRefs(f, algo)
	if err != nil {
		return packedRefs{}, fmt.Errorf("objstore: parse %s: %w", path, err)
	}
	return parsed, nil
}

// walkLooseRefs descends `<commonDir>/refs/` and registers every regular
// file as a ref entry, overriding any packed entry of the same name.
// Symbolic loose refs (`ref: <other-name>` content) are skipped: the
// only symref consumers care about is HEAD itself, and `Head()` reads
// it directly. Surfacing other symrefs through [RefEntry] would require
// the type to carry a target, which is out of scope for v0.
//
// A missing `refs/` directory is not an error — the empty-repo fixtures
// ship without one — but any other read failure aborts construction so
// a half-listed ref set does not silently mislead callers.
func (r *looseRefs) walkLooseRefs() error {
	refsDir := filepath.Join(r.commonDir, "refs")
	err := filepath.WalkDir(refsDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		// `.gitkeep` placeholders preserve empty `refs/` subdirectories
		// in the test fixtures; they are not refs.
		if d.Name() == ".gitkeep" {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}

		raw, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("objstore: read %s: %w", path, ErrCorruptObject)
		}
		content := strings.TrimRight(string(raw), " \t\r\n")
		if content == "" {
			return fmt.Errorf("objstore: ref %s empty: %w", path, ErrCorruptObject)
		}
		// Symbolic loose refs are rare in `refs/` (canonical Git keeps
		// them in `packed-refs` as direct OIDs); skip rather than
		// surface a half-modeled entry.
		if strings.HasPrefix(content, "ref:") {
			return nil
		}

		oid, err := objfmt.ParseHex(content, r.algo)
		if err != nil {
			return fmt.Errorf("objstore: ref %s: %w: %w", path, err, ErrCorruptObject)
		}

		rel, err := filepath.Rel(r.commonDir, path)
		if err != nil {
			return fmt.Errorf("objstore: rel %s: %w", path, err)
		}
		name := filepath.ToSlash(rel)

		// Loose overrides packed: drop any prior packed peel hint, since
		// a loose ref carries no peel information and we cannot trust a
		// stale peel against a possibly-rewritten OID. fromPacked stays
		// at its zero value (false) so the file-wide `fully-peeled`
		// trait does not bleed onto an OID that never lived in the file.
		r.refs[name] = packedEntry{oid: oid}
		return nil
	})
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return nil
}

// resolveHead reads `<gitDir>/HEAD` and returns the resolved [Head].
// The three accepted shapes are:
//
//   - `ref: <fully-qualified-name>\n` — symbolic HEAD. The OID is
//     looked up in the cached refs map; an unresolvable target yields
//     [Head.Unborn] = true with a zero OID.
//   - A bare hex OID (40 chars for SHA-1, 64 for SHA-256, with optional
//     trailing newline) — detached HEAD.
//   - Anything else — corruption.
//
// A missing HEAD is also corruption: the gitdir resolver would not have
// classified the directory as a repo without one. The check is defensive
// in case the file vanishes between resolver and constructor.
func (r *looseRefs) resolveHead() (Head, error) {
	path := filepath.Join(r.gitDir, "HEAD")
	raw, err := os.ReadFile(path)
	if err != nil {
		return Head{}, fmt.Errorf("objstore: read %s: %w", path, ErrCorruptObject)
	}
	content := strings.TrimRight(string(raw), " \t\r\n")
	if content == "" {
		return Head{}, fmt.Errorf("objstore: %s empty: %w", path, ErrCorruptObject)
	}

	if rest, ok := strings.CutPrefix(content, "ref:"); ok {
		target := strings.TrimSpace(rest)
		if target == "" {
			return Head{}, fmt.Errorf(
				"objstore: %s: empty symref target: %w", path, ErrCorruptObject)
		}
		entry, found := r.refs[target]
		if !found {
			return Head{Symref: target, Unborn: true}, nil
		}
		return Head{Symref: target, OID: entry.oid}, nil
	}

	oid, err := objfmt.ParseHex(content, r.algo)
	if err != nil {
		return Head{}, fmt.Errorf("objstore: %s: %w: %w", path, err, ErrCorruptObject)
	}
	return Head{OID: oid}, nil
}

// Head returns the cached [Head] resolved at construction. No I/O.
func (r *looseRefs) Head() (Head, error) { return r.head, nil }

// IterRefs yields every cached ref in lexical order. The iterator is
// I/O-free and never produces an error — every entry was validated at
// construction — but the [iter.Seq2] error slot stays in the contract
// so other backends can surface streaming failures without changing
// the interface.
func (r *looseRefs) IterRefs() iter.Seq2[RefEntry, error] {
	return func(yield func(RefEntry, error) bool) {
		for _, name := range r.sorted {
			entry := r.refs[name]
			if !yield(r.toRefEntry(name, entry), nil) {
				return
			}
		}
	}
}

// Lookup resolves name through the cached map. The map lookup is O(1)
// and I/O-free; every ref the constructor saw is materialized eagerly
// at [openLooseRefs] time. The error slot stays in the contract so the
// reftable backend can surface its decode-time failures uniformly, but
// this implementation never errors at lookup time.
func (r *looseRefs) Lookup(name string) (RefEntry, bool, error) {
	entry, found := r.refs[name]
	if !found {
		return RefEntry{}, false, nil
	}
	return r.toRefEntry(name, entry), true, nil
}

// toRefEntry lifts a cached [packedEntry] into the public [RefEntry].
// PeelKnown captures three signals:
//
//   - The entry's own `peelKnown` bit, set when a `^<oid>` line followed
//     the ref in `packed-refs`. The peel is recorded directly.
//   - The file-wide `fully-peeled` trait, but only for entries that came
//     from `packed-refs` (`fromPacked` true). The trait is a statement
//     about the file's contents — under it, the absence of a `^<oid>`
//     line is authoritative anywhere in the file, so a packed entry
//     without one definitively has no peel.
//   - The file-wide `peeled` trait, scoped to refs whose name has the
//     `refs/tags/` prefix and (again) only for `fromPacked` entries.
//     Canonical `next_record` (`refs/packed-backend.c:945`) sets
//     `REF_KNOWS_PEELED` for tags under either trait; a missing
//     `^<oid>` line on a tag means the tag is non-peelable
//     (commit-target lightweight tag).
//
// In every trait-derived case the `fromPacked` gate matters: a
// loose-override entry's OID never sat in the file, so the file-wide
// traits say nothing about it; PeelKnown stays false and
// [Store.PeelRef] falls through to a full peel.
func (r *looseRefs) toRefEntry(name string, entry packedEntry) RefEntry {
	peelKnown := entry.peelKnown ||
		(entry.fromPacked && r.traits.fullyPeeled) ||
		(entry.fromPacked && r.traits.peeled && strings.HasPrefix(name, "refs/tags/"))
	return RefEntry{
		Name:      name,
		OID:       entry.oid,
		Peeled:    entry.peeled,
		PeelKnown: peelKnown,
	}
}

// Close releases the backend. The eager-load constructor holds no file
// handles or memory mappings beyond the lifetime of [openLooseRefs].
func (r *looseRefs) Close() error { return nil }

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

// maxRefDepth caps symref-chain resolution at the same depth canonical
// Git uses (`SYMREF_MAXDEPTH = 5` in `refs/refs-internal.h:246`, applied
// by `refs.c::resolve_ref_unsafe` at `refs.c:2109`). A chain longer than
// this — or any cycle — surfaces as a corruption error.
const maxRefDepth = 5

// resolveHead reads `<gitDir>/HEAD` and returns the resolved [Head].
// The three accepted shapes are:
//
//   - `ref: <fully-qualified-name>\n` — symbolic HEAD. The target is
//     followed through any further symref hops up to [maxRefDepth];
//     an unresolvable terminal yields [Head.Unborn] = true with a zero
//     OID and the last symref name in [Head.Symref].
//   - A bare hex OID (40 chars for SHA-1, 64 for SHA-256, with optional
//     trailing newline) — detached HEAD.
//   - Anything else — corruption.
//
// Missing-HEAD handling: a missing HEAD file surfaces here as a
// corruption error rather than an unborn-repo signal. Canonical Git
// distinguishes `ENOENT` from other I/O errors in
// `refs/files-backend.c:562-570` (the open-error retry path) and from
// the lstat path at `refs/files-backend.c:504-512`, treating ENOENT as
// the "missing/unborn" case. We do not, because `git init` writes HEAD
// atomically as part of repo creation; a directory that passed this
// project's gitdir resolver but has no HEAD is in practice unreachable.
// Revisit if v0 ever needs to operate on partially-initialised
// repositories.
//
// Packed-refs HEAD fallback: canonical Git also falls back to
// `packed-refs` when the loose `HEAD` file is missing
// (`refs/files-backend.c:504-512`), a legacy compatibility path for
// repositories produced by very old Git versions. Modern Git keeps
// HEAD loose unconditionally, so we omit that fallback. If a fixture
// ever surfaces with HEAD only in `packed-refs`, this is the place to
// add the lookup.
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

	if target, ok, err := parseSymrefTarget(content); err != nil {
		return Head{}, fmt.Errorf("objstore: %s: %w", path, err)
	} else if ok {
		return r.followSymrefChain(target)
	}

	oid, err := objfmt.ParseHex(content, r.algo)
	if err != nil {
		return Head{}, fmt.Errorf("objstore: %s: %w: %w", path, err, ErrCorruptObject)
	}
	return Head{OID: oid}, nil
}

// followSymrefChain walks symref hops starting at target until either
// a terminal OID is found, a hop is missing (unborn), or [maxRefDepth]
// is exceeded. It detects cycles via a visited set keyed by the
// fully-qualified symref name. Intermediate symrefs are loose ref files
// (the eager [walkLooseRefs] pass deliberately skips them, since
// non-HEAD symrefs are not surfaced through [RefEntry]); chain hops
// therefore read those files on demand.
//
// The returned [Head.Symref] is the TERMINAL symref name — the final
// hop, whether it resolved to an OID (then `Symref` is the symref that
// pointed at that OID) or was missing (then `Symref` is the
// unresolvable name and `Unborn` is true). This mirrors canonical
// `refs.c::resolve_ref_unsafe` (`refs.c:2075`), which returns the loop's
// last `refname` whether the resolution succeeded or terminated at a
// missing target.
func (r *looseRefs) followSymrefChain(target string) (Head, error) {
	seen := make(map[string]struct{}, maxRefDepth)
	current := target
	for depth := 0; depth < maxRefDepth; depth++ {
		if _, ok := seen[current]; ok {
			return Head{}, fmt.Errorf(
				"objstore: symref cycle at %s: %w", current, ErrCorruptObject)
		}
		seen[current] = struct{}{}

		// Try the OID-bearing refs map first: it covers loose refs that
		// hold an OID and packed-refs entries. A hit terminates the
		// chain with the symref that named it.
		if entry, found := r.refs[current]; found {
			return Head{Symref: current, OID: entry.oid}, nil
		}

		// No OID at this name. Either it is itself a symref-loose file,
		// or it is missing entirely (unborn terminal).
		next, ok, err := r.readLooseSymref(current)
		if err != nil {
			return Head{}, err
		}
		if !ok {
			return Head{Symref: current, Unborn: true}, nil
		}
		current = next
	}
	return Head{}, fmt.Errorf(
		"objstore: symref chain exceeds depth %d: %w", maxRefDepth, ErrCorruptObject)
}

// readLooseSymref reads `<commonDir>/<name>` and returns the symref
// target if the file exists and is a symref. A missing file returns
// `ok=false` (the caller treats that as an unborn terminal). A file
// whose contents are not a `ref: ...` line is corruption: by the time
// the chain walker reaches it, the OID-bearing refs map has already
// been consulted and missed, so any non-symref content is a malformed
// loose ref file we declined to register at construction.
func (r *looseRefs) readLooseSymref(name string) (string, bool, error) {
	path := filepath.Join(r.commonDir, filepath.FromSlash(name))
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("objstore: read %s: %w", path, ErrCorruptObject)
	}
	content := strings.TrimRight(string(raw), " \t\r\n")
	target, ok, err := parseSymrefTarget(content)
	if err != nil {
		return "", false, fmt.Errorf("objstore: %s: %w", path, err)
	}
	if !ok {
		return "", false, fmt.Errorf(
			"objstore: %s: expected symref, got OID-shaped content: %w",
			path, ErrCorruptObject)
	}
	return target, true, nil
}

// parseSymrefTarget recognises the `ref: <name>` shape canonical Git
// writes for symbolic refs (`refs/files-backend.c::parse_loose_ref_contents`
// at `refs/files-backend.c:621`). It returns `ok=false` when the input
// is not a symref so callers can fall through to OID parsing, and an
// error when the input is a symref with an empty target.
func parseSymrefTarget(content string) (string, bool, error) {
	rest, ok := strings.CutPrefix(content, "ref:")
	if !ok {
		return "", false, nil
	}
	target := strings.TrimSpace(rest)
	if target == "" {
		return "", false, fmt.Errorf("empty symref target: %w", ErrCorruptObject)
	}
	return target, true, nil
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

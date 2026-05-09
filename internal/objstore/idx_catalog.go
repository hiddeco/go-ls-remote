package objstore

import (
	"errors"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
)

// idxCatalog opens every `<commonDir>/objects/pack/*.idx`, pairs each
// with its `.pack` sibling, and answers lookups by consulting the
// per-pack indexes in turn. Selected by [openPackBackend] when no
// `multi-pack-index` file is present.
//
// All wrapped state is established at construction. The wrapped
// [objfmt.Idx] and [objfmt.Pack] are documented as concurrent-read
// safe (see their godoc), so [idxCatalog.Lookup], [idxCatalog.AllPacks],
// and [idxCatalog.packByChecksum] are likewise safe for concurrent use
// from multiple goroutines without further synchronisation. [Close]
// is guarded by a [sync.Once] so the cascade runs exactly once.
type idxCatalog struct {
	commonDir string
	algo      objfmt.Algo

	// packs holds every (idx, pack) pair, sorted by idx basename so
	// [idxCatalog.Lookup] and [idxCatalog.AllPacks] iterate in a
	// platform-independent order.
	packs []packEntry

	// byChecksum maps each pack's trailer hash (as recorded by the
	// paired idx; see [objfmt.Idx.PackChecksum]) to the open pack. Used
	// by the cross-pack REF_DELTA resolver in a follow-up; intentionally
	// unused today.
	byChecksum map[objfmt.Hash]*objfmt.Pack

	closeOnce sync.Once
	closeErr  error
}

// packEntry pairs an opened idx with its `.pack` sibling. The pairing
// is built once at construction; callers consume `idx` for offset
// lookups and `pack` to read object bytes.
type packEntry struct {
	idx  *objfmt.Idx
	pack *objfmt.Pack
}

// openIdxCatalog opens every `*.idx` under `<commonDir>/objects/pack/`,
// pairs each with its `.pack` sibling, and returns a catalog ready for
// concurrent reads.
//
// A missing `objects/pack/` directory is not an error: a brand-new
// repository may not have materialised it yet, and the catalog exists
// alongside the always-present [looseObjects] backend. Other stat
// errors propagate wrapped with the offending path.
//
// On any per-pack failure (idx open, pack open, missing `.pack`
// sibling) every already-opened idx and pack is closed before the
// error is returned, so a partially-constructed catalog never leaks
// file handles.
func openIdxCatalog(commonDir string, algo objfmt.Algo) (*idxCatalog, error) {
	packDir := filepath.Join(commonDir, "objects", "pack")

	entries, err := os.ReadDir(packDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Brand-new repos may not have materialised `objects/pack/`
			// yet. Surface a usable empty catalog rather than refusing.
			return &idxCatalog{
				commonDir:  commonDir,
				algo:       algo,
				byChecksum: map[objfmt.Hash]*objfmt.Pack{},
			}, nil
		}
		return nil, fmt.Errorf("objstore: stat %s: %w", packDir, err)
	}

	c := &idxCatalog{
		commonDir:  commonDir,
		algo:       algo,
		byChecksum: map[objfmt.Hash]*objfmt.Pack{},
	}

	// closeOpened tears down everything established so far. Used on
	// every per-pack failure path so a partial open never leaks.
	closeOpened := func() {
		for _, e := range c.packs {
			if e.idx != nil {
				_ = e.idx.Close()
			}
			if e.pack != nil {
				_ = e.pack.Close()
			}
		}
	}

	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		name := ent.Name()
		if !strings.HasSuffix(name, ".idx") {
			// Skip `.keep`, `.bitmap`, `.rev`, `multi-pack-index` (in
			// the rare case it slipped past the selector), and any
			// other sibling files Git may park here.
			continue
		}

		idxPath := filepath.Join(packDir, name)
		idx, err := objfmt.OpenIdx(idxPath, algo)
		if err != nil {
			closeOpened()
			return nil, fmt.Errorf("objstore: open idx %s: %w", idxPath, err)
		}

		// Paired pack lives next to the idx with the same basename.
		packPath := strings.TrimSuffix(idxPath, ".idx") + ".pack"
		pack, err := objfmt.OpenPack(packPath, algo)
		if err != nil {
			_ = idx.Close()
			closeOpened()
			return nil, fmt.Errorf("objstore: open pack %s for idx %s: %w",
				packPath, idxPath, err)
		}

		c.packs = append(c.packs, packEntry{idx: idx, pack: pack})
		c.byChecksum[idx.PackChecksum()] = pack
	}

	// Sort by idx basename so `Lookup` iteration order — and the
	// `AllPacks` enumeration the cross-pack REF_DELTA scan walks — is
	// stable across filesystems whose `ReadDir` ordering differs.
	slices.SortFunc(c.packs, func(a, b packEntry) int {
		return strings.Compare(filepath.Base(a.idx.Path()),
			filepath.Base(b.idx.Path()))
	})

	return c, nil
}

// Lookup walks the catalog's packs in basename-sorted order and returns
// the first (Pack, offset) pair whose idx records h. A miss surfaces as
// `(nil, 0, false, nil)` so the upper layer can fall through to loose
// objects, alternates, or [ErrCorruptObject] without reinterpreting
// the error slot.
//
// [objfmt.Idx.FindOffset] reports only a boolean today; the error slot
// here is preserved for forward compatibility with a future shape that
// may surface per-idx failures (e.g. a lazily-mapped read error).
func (c *idxCatalog) Lookup(h objfmt.Hash) (*objfmt.Pack, int64, bool, error) {
	for _, e := range c.packs {
		if off, ok := e.idx.FindOffset(h); ok {
			return e.pack, off, true, nil
		}
	}
	return nil, 0, false, nil
}

// AllPacks yields every open `*objfmt.Pack` in the catalog's
// deterministic basename-sorted order. Used by the cross-pack
// REF_DELTA scan and any external integrity walk that needs to see
// each pack exactly once.
func (c *idxCatalog) AllPacks() iter.Seq[*objfmt.Pack] {
	return func(yield func(*objfmt.Pack) bool) {
		for _, e := range c.packs {
			if !yield(e.pack) {
				return
			}
		}
	}
}

// packByChecksum returns the open pack whose trailer hash matches h, as
// recorded by the paired idx ([objfmt.Idx.PackChecksum]).
//
// Intentionally unused today; pre-installed for the cross-pack
// REF_DELTA resolver in a follow-up. A REF_DELTA encodes its base by
// OID without naming the pack the base lives in, so resolving across
// packs needs an OID-keyed scan; this accessor exists so that scan can
// short-circuit when the base's pack-checksum hint is known up front.
func (c *idxCatalog) packByChecksum(h objfmt.Hash) (*objfmt.Pack, bool) {
	p, ok := c.byChecksum[h]
	return p, ok
}

// Close releases every opened idx and pack. Errors from each are joined
// via [errors.Join] so a single failure does not mask the rest. Close
// is idempotent: subsequent calls return the same joined error without
// re-running the cascade.
func (c *idxCatalog) Close() error {
	c.closeOnce.Do(func() {
		var errs []error
		for _, e := range c.packs {
			if err := e.idx.Close(); err != nil {
				errs = append(errs, err)
			}
			if err := e.pack.Close(); err != nil {
				errs = append(errs, err)
			}
		}
		c.closeErr = errors.Join(errs...)
	})
	return c.closeErr
}

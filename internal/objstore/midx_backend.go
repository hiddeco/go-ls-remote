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

// midxBackend reads `<commonDir>/objects/pack/multi-pack-index` and
// answers lookups across the entire pack set in one shot. Selected by
// [openPackBackend] when the midx file exists, regardless of how many
// `.idx` files sit alongside it: canonical Git keeps both formats so
// older tooling can still resolve objects via per-pack idxs while the
// midx accelerates aggregate lookups (`midx.c` and
// `Documentation/technical/multi-pack-index.adoc`).
//
// The midx is authoritative for the packs it lists — it carries a
// fanout / OID table / per-object pack id and offset for every covered
// object — but a directory may also hold packs added after midx
// generation. Canonical Git treats those as a fallback set: lookups
// consult the midx first and only walk per-pack idxs for the packs the
// midx does not cover. This backend mirrors that contract.
//
// All wrapped state is established at construction. The wrapped
// [objfmt.Midx], [objfmt.Idx], and [objfmt.Pack] are documented as
// concurrent-read safe (see their godoc), so [midxBackend.Lookup],
// [midxBackend.AllPacks], and [midxBackend.packByChecksum] are likewise
// safe for concurrent use from multiple goroutines without further
// synchronisation. [Close] is guarded by a [sync.Once] so the cascade
// runs exactly once.
type midxBackend struct {
	commonDir string

	// midx is the parsed `multi-pack-index`. The midx reader owns the
	// in-memory body and is closed alongside the wrapped packs.
	midx *objfmt.Midx

	// packNames is a one-time cached copy of `midx.PackNames()`.
	// `Midx.PackNames` allocates a fresh slice on every call; caching
	// here keeps the hot lookup path allocation-free.
	packNames []string

	// coveredByMidxIndex maps each index into [packNames] to its
	// open pack. The midx's `OOFF` chunk encodes pack indices into
	// `PackNames()`, so a hit on [Midx.Find] translates to a `*Pack`
	// with a single positional slice access — no map probe, no string
	// indirection.
	coveredByMidxIndex []*objfmt.Pack

	// coveredIdxs is the `*objfmt.Idx` parallel to
	// [coveredByMidxIndex]. Lookups never consult these — the midx
	// itself owns the pack-id / offset table — but [Close] needs them
	// to release the underlying file handles, and `idx.PackChecksum()`
	// is read at open time to populate [packsByChecksum].
	coveredIdxs []*objfmt.Idx

	// packsByChecksum maps every opened pack — midx-covered AND
	// sibling — by its trailer hash (as recorded by the paired idx;
	// see [objfmt.Idx.PackChecksum]). Used by the cross-pack REF_DELTA
	// resolver in a follow-up; intentionally unused today.
	packsByChecksum map[objfmt.Hash]*objfmt.Pack

	// siblings carries every (idx, pack) pair from the directory that
	// is NOT covered by the midx. [midxBackend.Lookup] falls through to
	// these on a midx miss — matching canonical Git's "midx is
	// authoritative for its packs; siblings exist for newly-added
	// packs." Sorted by idx basename so the fallback walk and
	// [midxBackend.AllPacks] iterate in a platform-independent order.
	siblings []packEntry

	// ordered enumerates every opened pack in the order
	// [midxBackend.AllPacks] yields: midx-covered packs first in
	// `midx.PackNames()` order, then sibling packs in basename-sorted
	// order. Built once at construction.
	ordered []*objfmt.Pack

	closeOnce sync.Once
	closeErr  error
}

// openMidxBackend opens `<commonDir>/objects/pack/multi-pack-index`
// alongside every `.idx` in the same directory and returns a backend
// ready for concurrent reads.
//
// The midx is opened first; on failure the constructor returns without
// touching the rest of the directory. After the midx parses cleanly
// every `.idx` is opened (regardless of midx coverage) and paired with
// its `.pack` sibling, exactly as `idxCatalog` does. Each pair is then
// classified: an idx whose basename appears in `midx.PackNames()` is a
// midx-covered pack and goes into `packsByName`; everything else
// becomes a sibling for the fallback scan. Both classifications also
// land in `packsByChecksum` so the cross-pack REF_DELTA scaffolding
// works through one uniform map.
//
// The midx's pack-name list is required to be a subset of the on-disk
// `.idx` set: a midx that names a pack the directory no longer
// contains is corrupt (the midx and the pack-set must agree). Such
// catalogs are rejected with an error wrapping [ErrCorruptObject].
//
// On any per-pack failure (idx open, pack open, missing `.pack`
// sibling, midx referencing a missing pack) every already-opened
// resource — the midx and every (idx, pack) pair — is closed before
// the error is returned, so a partially-constructed backend never
// leaks file handles.
func openMidxBackend(commonDir string, algo objfmt.Algo) (*midxBackend, error) {
	packDir := filepath.Join(commonDir, "objects", "pack")
	midxPath := filepath.Join(packDir, "multi-pack-index")

	midx, err := objfmt.OpenMidx(midxPath, algo)
	if err != nil {
		return nil, fmt.Errorf("objstore: open midx %s: %w", midxPath, err)
	}

	// Cache `Midx.PackNames()` once: each call allocates a fresh copy,
	// and the lookup hot path consults it on every midx hit.
	packNames := midx.PackNames()

	b := &midxBackend{
		commonDir:          commonDir,
		midx:               midx,
		packNames:          packNames,
		coveredByMidxIndex: make([]*objfmt.Pack, len(packNames)),
		coveredIdxs:        make([]*objfmt.Idx, len(packNames)),
		packsByChecksum:    map[objfmt.Hash]*objfmt.Pack{},
	}

	// nameIndex maps each PackNames entry to its slot in
	// `coveredByMidxIndex` / `coveredIdxs`. Built once so directory
	// classification is O(1) per `.idx`.
	nameIndex := make(map[string]int, len(packNames))
	for i, n := range packNames {
		nameIndex[n] = i
	}

	// closeOpened tears down everything established so far. Used on
	// every per-pack failure path so a partial open never leaks. The
	// midx is closed unconditionally; partially-classified slots may
	// be nil (slot reserved but pack not yet placed) and are skipped.
	closeOpened := func() {
		_ = b.midx.Close()
		for _, p := range b.coveredByMidxIndex {
			if p != nil {
				_ = p.Close()
			}
		}
		for _, idx := range b.coveredIdxs {
			if idx != nil {
				_ = idx.Close()
			}
		}
		for _, e := range b.siblings {
			if e.idx != nil {
				_ = e.idx.Close()
			}
			if e.pack != nil {
				_ = e.pack.Close()
			}
		}
	}

	entries, err := os.ReadDir(packDir)
	if err != nil {
		// The midx exists but the directory walk failed: surface the
		// underlying error after closing the midx. ENOENT here would
		// mean the pack dir was deleted between the midx-stat and the
		// walk — unusual but worth a clean error.
		_ = midx.Close()
		return nil, fmt.Errorf("objstore: stat %s: %w", packDir, err)
	}

	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		name := ent.Name()
		if !strings.HasSuffix(name, ".idx") {
			// Skip `.keep`, `.bitmap`, `.rev`, the midx itself, and
			// any other sibling files Git may park here.
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

		b.packsByChecksum[idx.PackChecksum()] = pack
		if i, covered := nameIndex[name]; covered {
			b.coveredByMidxIndex[i] = pack
			b.coveredIdxs[i] = idx
		} else {
			b.siblings = append(b.siblings, packEntry{idx: idx, pack: pack})
		}
	}

	// Validate that every pack the midx claims is actually present:
	// the midx and the pack-set must agree (canonical Git rejects this
	// shape too — see `midx.c::prepare_midx_pack`). Missing entries
	// surface as [ErrCorruptObject] naming the offending pack so
	// operators can find it.
	for i, pack := range b.coveredByMidxIndex {
		if pack == nil {
			closeOpened()
			return nil, fmt.Errorf("objstore: midx references missing pack %q: %w",
				b.packNames[i], ErrCorruptObject)
		}
	}

	// Sort siblings by idx basename so the fallback scan iterates in a
	// stable order across filesystems whose `ReadDir` ordering differs.
	slices.SortFunc(b.siblings, func(a, c packEntry) int {
		return strings.Compare(filepath.Base(a.idx.Path()),
			filepath.Base(c.idx.Path()))
	})

	// Build `ordered`: midx-covered packs in `PackNames` order, then
	// siblings in their already-sorted order. `coveredByMidxIndex` is
	// already the PackNames-order slice, so it splats directly.
	b.ordered = make([]*objfmt.Pack, 0, len(b.coveredByMidxIndex)+len(b.siblings))
	b.ordered = append(b.ordered, b.coveredByMidxIndex...)
	for _, e := range b.siblings {
		b.ordered = append(b.ordered, e.pack)
	}

	return b, nil
}

// Lookup answers h by consulting the midx first and falling back to a
// per-idx scan over [midxBackend.siblings] on a midx miss. The fallback
// matches canonical Git's "midx is authoritative for its packs;
// siblings exist for newly-added packs" rule (`midx.c::nth_midxed_*`
// vs `find_pack_entry`).
//
// A miss surfaces as `(nil, 0, false, nil)` so the upper layer can fall
// through to loose objects, alternates, or [ErrCorruptObject] without
// reinterpreting the error slot. The error slot is preserved for
// forward compatibility with a future shape that may surface per-pack
// failures (e.g. a lazily-mapped read error).
func (b *midxBackend) Lookup(h objfmt.Hash) (*objfmt.Pack, int64, bool, error) {
	if packIdx, off, ok := b.midx.Find(h); ok {
		// `coveredByMidxIndex` is sized to `PackNames()` at open
		// (see [openMidxBackend]) and `Midx.Find` returns the same
		// `PackNames` index, so this access is in-bounds by
		// construction. A truly corrupt midx surfaces as a runtime
		// slice-bounds panic, which is the right signal for a state
		// the open-time validation could not see.
		return b.coveredByMidxIndex[packIdx], off, true, nil
	}

	// Midx miss: scan the sibling packs in deterministic order. First
	// hit wins; sibling packs were sorted by basename at construction.
	for _, e := range b.siblings {
		if off, ok := e.idx.FindOffset(h); ok {
			return e.pack, off, true, nil
		}
	}
	return nil, 0, false, nil
}

// AllPacks yields every open `*objfmt.Pack` the backend holds in
// deterministic order: midx-covered packs first in
// `Midx.PackNames()` order, then sibling packs in basename-sorted
// order. Used by the cross-pack REF_DELTA scan and any external
// integrity walk that needs to see each pack exactly once.
func (b *midxBackend) AllPacks() iter.Seq[*objfmt.Pack] {
	return func(yield func(*objfmt.Pack) bool) {
		for _, p := range b.ordered {
			if !yield(p) {
				return
			}
		}
	}
}

// packByChecksum returns the open pack whose trailer hash matches h, as
// recorded by the paired idx ([objfmt.Idx.PackChecksum]). The index
// covers every opened pack — midx-covered AND sibling — so the
// cross-pack REF_DELTA resolver can probe through one uniform map
// regardless of which backend is in use.
//
// Intentionally unused today; pre-installed for the cross-pack
// REF_DELTA resolver in a follow-up. A REF_DELTA encodes its base by
// OID without naming the pack the base lives in, so resolving across
// packs needs an OID-keyed scan; this accessor exists so that scan can
// short-circuit when the base's pack-checksum hint is known up front.
func (b *midxBackend) packByChecksum(h objfmt.Hash) (*objfmt.Pack, bool) {
	p, ok := b.packsByChecksum[h]
	return p, ok
}

// Close releases the midx, every covered idx, and every (idx, pack)
// pair the backend opened. Errors from each are joined via
// [errors.Join] so a single failure does not mask the rest. Close is
// idempotent: subsequent calls return the same joined error without
// re-running the cascade.
func (b *midxBackend) Close() error {
	b.closeOnce.Do(func() {
		var errs []error
		if b.midx != nil {
			if err := b.midx.Close(); err != nil {
				errs = append(errs, err)
			}
		}
		for _, idx := range b.coveredIdxs {
			if err := idx.Close(); err != nil {
				errs = append(errs, err)
			}
		}
		for _, p := range b.coveredByMidxIndex {
			if err := p.Close(); err != nil {
				errs = append(errs, err)
			}
		}
		for _, e := range b.siblings {
			if err := e.idx.Close(); err != nil {
				errs = append(errs, err)
			}
			if err := e.pack.Close(); err != nil {
				errs = append(errs, err)
			}
		}
		b.closeErr = errors.Join(errs...)
	})
	return b.closeErr
}

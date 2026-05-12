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
	"time"

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
// concurrent-read safe (see their godoc), so [midxBackend.Lookup] and
// [midxBackend.AllPacks] are likewise safe for concurrent use from
// multiple goroutines without further synchronisation. [Close] is
// guarded by a [sync.Once] so the cascade runs exactly once.
type midxBackend[H objfmt.Hash] struct {
	commonDir string

	// midx is the parsed `multi-pack-index`. The midx reader owns the
	// in-memory body and is closed alongside the wrapped packs.
	midx *objfmt.Midx[H]

	// packNames is a one-time cached copy of `midx.PackNames()`.
	// `Midx.PackNames` allocates a fresh slice on every call; caching
	// here keeps the hot lookup path allocation-free.
	packNames []string

	// coveredByMidxIndex maps each index into [packNames] to its
	// open pack. The midx's `OOFF` chunk encodes pack indices into
	// `PackNames()`, so a hit on [Midx.Find] translates to a `*Pack`
	// with a single positional slice access — no map probe, no string
	// indirection.
	coveredByMidxIndex []*objfmt.Pack[H]

	// coveredIdxs is the `*objfmt.Idx` parallel to
	// [coveredByMidxIndex]. Lookups never consult these — the midx
	// itself owns the pack-id / offset table — but [Close] needs them
	// to release the underlying file handles.
	coveredIdxs []*objfmt.Idx[H]

	// idxByPack maps every opened pack — midx-covered AND sibling — to
	// its paired idx. The CRC verification path on [Store.ObjectInfo]
	// reaches for an idx by pack pointer, and a map lookup keeps that
	// hot path O(1) regardless of how many packs the backend tracks.
	idxByPack map[*objfmt.Pack[H]]*objfmt.Idx[H]

	// siblings carries every (idx, pack) pair from the directory that
	// is NOT covered by the midx. [midxBackend.Lookup] falls through to
	// these on a midx miss — matching canonical Git's "midx is
	// authoritative for its packs; siblings exist for newly-added
	// packs." Sorted by pack mtime (younger first) with idx basename
	// as a stable tiebreaker so the fallback walk consults the pack
	// most likely to satisfy the next lookup first, matching
	// canonical Git's [packfile.c::sort_pack] heuristic.
	//
	// [packfile.c::sort_pack]: https://github.com/git/git/blob/v2.54.0/packfile.c#L1042
	siblings []packEntry[H]

	// ordered enumerates every opened pack in the order
	// [midxBackend.AllPacks] yields. Unlike `Lookup` — which keeps the
	// midx-first / siblings-fallback two-phase shape because the midx
	// is the fast index — `AllPacks` is a single mtime-descending
	// list across both buckets, basename as the tiebreaker. Consumers
	// (the cross-pack REF_DELTA scan, integrity walks) want the same
	// "younger first" heuristic Git applies in `sort_pack`; the
	// midx-insertion order is an implementation detail and would
	// otherwise leak into them.
	ordered []*objfmt.Pack[H]

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
// becomes a sibling for the fallback scan.
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
func openMidxBackend[H objfmt.Hash](commonDir string, algo objfmt.Algo) (*midxBackend[H], error) {
	packDir := filepath.Join(commonDir, "objects", "pack")
	midxPath := filepath.Join(packDir, "multi-pack-index")

	midx, err := objfmt.OpenMidx[H](midxPath, algo)
	if err != nil {
		return nil, fmt.Errorf("objstore: open midx %s: %w", midxPath, err)
	}

	// Cache `Midx.PackNames()` once: each call allocates a fresh copy,
	// and the lookup hot path consults it on every midx hit.
	packNames := midx.PackNames()

	b := &midxBackend[H]{
		commonDir:          commonDir,
		midx:               midx,
		packNames:          packNames,
		coveredByMidxIndex: make([]*objfmt.Pack[H], len(packNames)),
		coveredIdxs:        make([]*objfmt.Idx[H], len(packNames)),
		idxByPack:          map[*objfmt.Pack[H]]*objfmt.Idx[H]{},
	}

	// coveredMtimes parallels `coveredByMidxIndex`, recording each
	// midx-covered pack's mtime so the `AllPacks` enumeration can
	// merge covered and sibling packs into a single mtime-desc list
	// after construction. The midx slot itself stays in `PackNames`
	// order — `Lookup` indexes into it directly via the midx-reported
	// pack id and must not be reshuffled.
	coveredMtimes := make([]time.Time, len(packNames))

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
		idx, err := objfmt.OpenIdx[H](idxPath, algo)
		if err != nil {
			closeOpened()
			return nil, fmt.Errorf("objstore: open idx %s: %w", idxPath, err)
		}

		// Paired pack lives next to the idx with the same basename.
		packPath := strings.TrimSuffix(idxPath, ".idx") + ".pack"
		pack, err := objfmt.OpenPack[H](packPath, algo)
		if err != nil {
			_ = idx.Close()
			closeOpened()
			return nil, fmt.Errorf("objstore: open pack %s for idx %s: %w",
				packPath, idxPath, err)
		}

		// Capture pack mtime once: it feeds the open-time sort that
		// matches canonical Git's [packfile.c::sort_pack] heuristic
		// (younger first). Stat failures here are unusual — the pack
		// was just opened — but surface as a clean error rather than
		// silently substituting a zero time.
		//
		// [packfile.c::sort_pack]: https://github.com/git/git/blob/v2.54.0/packfile.c#L1042
		st, err := os.Stat(packPath)
		if err != nil {
			_ = pack.Close()
			_ = idx.Close()
			closeOpened()
			return nil, fmt.Errorf("objstore: stat pack %s: %w", packPath, err)
		}
		mtime := st.ModTime()

		b.idxByPack[pack] = idx
		if i, covered := nameIndex[name]; covered {
			b.coveredByMidxIndex[i] = pack
			b.coveredIdxs[i] = idx
			coveredMtimes[i] = mtime
		} else {
			b.siblings = append(b.siblings, packEntry[H]{
				idx:   idx,
				pack:  pack,
				mtime: mtime,
			})
		}
	}

	// Validate that every pack the midx claims is actually present:
	// the midx and the pack-set must agree (canonical Git rejects this
	// shape too — see [midx.c::prepare_midx_pack]). Missing entries
	// surface as [ErrCorruptObject] naming the offending pack so
	// operators can find it.
	//
	// [midx.c::prepare_midx_pack]: https://github.com/git/git/blob/v2.54.0/midx.c#L456
	for i, pack := range b.coveredByMidxIndex {
		if pack == nil {
			closeOpened()
			return nil, fmt.Errorf("objstore: midx references missing pack %q: %w",
				b.packNames[i], ErrCorruptObject)
		}
	}

	// Sort siblings by pack mtime (younger first) with idx basename
	// as a stable tiebreaker so the midx-miss fallback scan consults
	// the pack most likely to satisfy the next lookup first
	// ([packfile.c::sort_pack]).
	//
	// [packfile.c::sort_pack]: https://github.com/git/git/blob/v2.54.0/packfile.c#L1042
	slices.SortFunc(b.siblings, func(a, c packEntry[H]) int {
		if d := c.mtime.Compare(a.mtime); d != 0 {
			return d
		}
		return strings.Compare(filepath.Base(a.idx.Path()),
			filepath.Base(c.idx.Path()))
	})

	// Build `ordered` as a single mtime-desc / basename-tiebreaker
	// merged list across midx-covered AND sibling packs. The midx
	// insertion order is an implementation detail consumers of
	// `AllPacks` (e.g. cross-pack REF_DELTA scans) must not depend
	// on; canonical Git's [packfile.c::sort_pack] heuristic governs
	// the order across the whole pack set.
	//
	// [packfile.c::sort_pack]: https://github.com/git/git/blob/v2.54.0/packfile.c#L1042
	type orderedEntry struct {
		pack  *objfmt.Pack[H]
		idx   *objfmt.Idx[H]
		mtime time.Time
	}
	all := make([]orderedEntry, 0, len(b.coveredByMidxIndex)+len(b.siblings))
	for i, p := range b.coveredByMidxIndex {
		all = append(all, orderedEntry{
			pack:  p,
			idx:   b.coveredIdxs[i],
			mtime: coveredMtimes[i],
		})
	}
	for _, e := range b.siblings {
		all = append(all, orderedEntry{pack: e.pack, idx: e.idx, mtime: e.mtime})
	}
	slices.SortFunc(all, func(a, c orderedEntry) int {
		if d := c.mtime.Compare(a.mtime); d != 0 {
			return d
		}
		return strings.Compare(filepath.Base(a.idx.Path()),
			filepath.Base(c.idx.Path()))
	})
	b.ordered = make([]*objfmt.Pack[H], len(all))
	for i, e := range all {
		b.ordered[i] = e.pack
	}

	return b, nil
}

// Lookup answers h by consulting the midx first and falling back to a
// per-idx scan over [midxBackend.siblings] on a midx miss. The
// midx-first half of the contract is the fast index — that's the
// primary win and stays unchanged. The fallback matches canonical
// Git's "midx is authoritative for its packs; siblings exist for
// newly-added packs" rule (`midx.c::nth_midxed_*` vs
// `find_pack_entry`); the sibling scan order is mtime-descending
// with basename tiebreaker, matching [packfile.c::sort_pack] so the
// pack most likely to satisfy the next lookup is the first one
// consulted.
//
// A miss surfaces as `(nil, 0, false, nil)` so the upper layer can fall
// through to loose objects, alternates, or [ErrCorruptObject] without
// reinterpreting the error slot. The error slot is preserved for
// forward compatibility with a future shape that may surface per-pack
// failures (e.g. a lazily-mapped read error).
//
// [packfile.c::sort_pack]: https://github.com/git/git/blob/v2.54.0/packfile.c#L1042
func (b *midxBackend[H]) Lookup(h H) (*objfmt.Pack[H], int64, bool, error) {
	if packIdx, off, ok := b.midx.Find(h); ok {
		// `coveredByMidxIndex` is sized to `PackNames()` at open
		// (see [openMidxBackend]) and `Midx.Find` returns the same
		// `PackNames` index, so this access is in-bounds by
		// construction. A truly corrupt midx surfaces as a runtime
		// slice-bounds panic, which is the right signal for a state
		// the open-time validation could not see.
		return b.coveredByMidxIndex[packIdx], off, true, nil
	}

	// Midx miss: scan the sibling packs in mtime-desc / basename
	// order so a fresh pack added after midx generation is the first
	// fallback consulted.
	for _, e := range b.siblings {
		if off, ok := e.idx.FindOffset(h); ok {
			return e.pack, off, true, nil
		}
	}
	return nil, 0, false, nil
}

// AllPacks yields every open `*objfmt.Pack` the backend holds — both
// midx-covered and sibling — in a single mtime-desc / basename
// tiebreaker order, matching canonical Git's [packfile.c::sort_pack]
// heuristic. The midx insertion order is an implementation detail
// that does NOT survive in the enumeration; consumers (the cross-pack
// REF_DELTA scan, integrity walks) want "younger first" across the
// whole set. See [packBackend.AllPacks] for the contract.
//
// [packfile.c::sort_pack]: https://github.com/git/git/blob/v2.54.0/packfile.c#L1042
func (b *midxBackend[H]) AllPacks() iter.Seq[*objfmt.Pack[H]] {
	return func(yield func(*objfmt.Pack[H]) bool) {
		for _, p := range b.ordered {
			if !yield(p) {
				return
			}
		}
	}
}

// IdxFor returns the [objfmt.Idx] paired with pack via the open-time
// [midxBackend.idxByPack] map — see [packBackend.IdxFor] for the
// contract. The map covers both midx-covered and sibling packs, so the
// CRC path resolves either bucket through a single probe. ok=false
// signals that pack is not one this backend opened.
func (b *midxBackend[H]) IdxFor(pack *objfmt.Pack[H]) (*objfmt.Idx[H], bool) {
	idx, ok := b.idxByPack[pack]
	return idx, ok
}

// Close releases the midx, every covered idx, and every (idx, pack)
// pair the backend opened. Errors from each are joined via
// [errors.Join] so a single failure does not mask the rest. Close is
// idempotent: subsequent calls return the same joined error without
// re-running the cascade.
func (b *midxBackend[H]) Close() error {
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

package objstore

import (
	"iter"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
)

// packBackend is the contract every pack-index implementation honours.
// The two concrete backends are the per-pack `.idx` catalogue (one
// `*objfmt.Pack` per `.idx` file under `objects/pack/`) and the
// multi-pack-index reader (`objects/pack/multi-pack-index`); the
// store opener picks one based on the presence of `multi-pack-index`
// and treats the rest of the lookup pipeline as backend-agnostic.
//
// The interface is parameterised on `H` — the [objfmt.Hash] of the
// owning [Store] — so [packBackend.Lookup] takes typed OIDs and the
// returned `*objfmt.Pack[H]` and `*objfmt.Idx[H]` carry the same type
// without the caller threading an [objfmt.Algo] through.
//
// All methods are safe for concurrent use by multiple goroutines once
// the backend has been constructed.
type packBackend[H objfmt.Hash] interface {
	// Lookup returns the pack and offset for h, if any. ok=false with
	// nil err signals "not in this backend"; the caller is responsible
	// for falling through to loose objects, alternates, or
	// [ErrCorruptObject] as appropriate.
	Lookup(h H) (pack *objfmt.Pack[H], offset int64, ok bool, err error)

	// AllPacks yields every pack the backend exposes in approximate
	// mtime-descending order — younger packs first, with basename as a
	// stable tiebreaker. The order matches canonical Git's `sort_pack`
	// heuristic (`packfile.c`): younger packs tend to hold more recent
	// objects and to satisfy the next lookup. The order is best-effort
	// (it depends on filesystem mtime resolution and is not stable
	// across renames or restamps); correctness must not depend on it.
	AllPacks() iter.Seq[*objfmt.Pack[H]]

	// IdxFor returns the [objfmt.Idx] paired with pack, if pack is one
	// the backend opened. The lookup is O(1) so callers on the CRC
	// verification hot path can pair (pack, idx) without rescanning the
	// backend's internal storage. ok=false signals the pack was not
	// produced by this backend — typically a sign the pack escaped from
	// a different backend or has been closed mid-flight; callers should
	// treat it as defence in depth rather than a recoverable miss.
	IdxFor(pack *objfmt.Pack[H]) (idx *objfmt.Idx[H], ok bool)

	// Close releases the index file handles and pack mappings held by
	// the backend. It must be safe to call exactly once; idempotency
	// is the [Store]'s responsibility.
	Close() error
}

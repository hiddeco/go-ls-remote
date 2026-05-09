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
// All methods are safe for concurrent use by multiple goroutines once
// the backend has been constructed.
type packBackend interface {
	// Lookup returns the pack and offset for h, if any. ok=false with
	// nil err signals "not in this backend"; the caller is responsible
	// for falling through to loose objects, alternates, or
	// [ErrCorruptObject] as appropriate.
	Lookup(h objfmt.Hash) (pack *objfmt.Pack, offset int64, ok bool, err error)

	// AllPacks yields every pack the backend exposes in approximate
	// mtime-descending order — younger packs first, with basename as a
	// stable tiebreaker. The order matches canonical Git's `sort_pack`
	// heuristic (`packfile.c`): younger packs tend to hold more recent
	// objects and to satisfy the next lookup. The order is best-effort
	// (it depends on filesystem mtime resolution and is not stable
	// across renames or restamps); correctness must not depend on it.
	AllPacks() iter.Seq[*objfmt.Pack]

	// Close releases the index file handles and pack mappings held by
	// the backend. It must be safe to call exactly once; idempotency
	// is the [Store]'s responsibility.
	Close() error
}

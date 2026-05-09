package objstore

import (
	"iter"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
)

// midxBackend reads `<commonDir>/objects/pack/multi-pack-index` and
// answers lookups across the entire pack set in one shot. Selected
// when the midx file exists, regardless of how many `.idx` files sit
// alongside it (canonical Git keeps both for compatibility).
//
// This file carries the type and constructor only. Midx parsing,
// pack-id chunk traversal, and the sibling-`.idx` fallback land in a
// follow-up.
type midxBackend struct {
	commonDir string
	algo      objfmt.Algo
}

// openMidxBackend constructs a [midxBackend]. It must succeed on a
// well-formed empty repository — even an empty `multi-pack-index`
// placeholder is enough at this stage; the real implementation will
// reject malformed bodies via [ErrCorruptObject] or a parse error.
func openMidxBackend(commonDir string, algo objfmt.Algo) (*midxBackend, error) {
	return &midxBackend{commonDir: commonDir, algo: algo}, nil
}

// Lookup reports h as not found by default. The real implementation
// consults the midx fanout / lookup / pack-id chunks.
func (b *midxBackend) Lookup(h objfmt.Hash) (*objfmt.Pack, int64, bool, error) {
	return nil, 0, false, nil
}

// AllPacks yields nothing by default. The real implementation iterates
// over the packs the midx references.
func (b *midxBackend) AllPacks() iter.Seq[*objfmt.Pack] {
	return func(yield func(*objfmt.Pack) bool) {}
}

// Close releases the backend. The skeleton holds no resources.
func (b *midxBackend) Close() error { return nil }

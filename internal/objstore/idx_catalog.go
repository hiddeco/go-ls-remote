package objstore

import (
	"iter"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
)

// idxCatalog opens every `<commonDir>/objects/pack/*.idx`, pairs each
// with its `.pack` sibling, and answers lookups by consulting the
// per-pack indexes in turn. Selected when no `multi-pack-index` file
// is present.
//
// This file carries the type and constructor only. Per-pack open,
// fanout lookup, and `AllPacks` enumeration land in a follow-up.
type idxCatalog struct {
	commonDir string
	algo      objfmt.Algo
}

// openIdxCatalog constructs an [idxCatalog]. It must succeed on a
// well-formed empty repository (no `.idx` files at all); per-pack
// errors surface from [idxCatalog.Lookup] once the real implementation
// lands.
func openIdxCatalog(commonDir string, algo objfmt.Algo) (*idxCatalog, error) {
	return &idxCatalog{commonDir: commonDir, algo: algo}, nil
}

// Lookup reports h as not found by default. The real implementation
// consults each `.idx`'s fanout / lookup / offset chunks in turn.
func (c *idxCatalog) Lookup(h objfmt.Hash) (*objfmt.Pack, int64, bool, error) {
	return nil, 0, false, nil
}

// AllPacks yields nothing by default. The real implementation iterates
// over every opened `*objfmt.Pack`.
func (c *idxCatalog) AllPacks() iter.Seq[*objfmt.Pack] {
	return func(yield func(*objfmt.Pack) bool) {}
}

// Close releases the backend. The skeleton holds no resources.
func (c *idxCatalog) Close() error { return nil }

package objstore

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
)

// Store is a read-only handle on a single Git object store. It binds
// together the resolved gitdir and common dir, the configured ref and
// pack backends, the always-present loose-object reader, and the
// transitive chain of alternates.
//
// A Store is safe for concurrent reads from multiple goroutines once
// [Open] has returned. Mutation of the underlying on-disk state is
// out of scope; this library never writes commits, refs, or packs.
//
// The constructor side picks one ref backend (loose files or
// reftable) and one pack backend (per-`.idx` catalogue or
// multi-pack-index) based on the repository configuration and the
// presence of `objects/pack/multi-pack-index`. The chosen backends
// are not switched at runtime; reopen the Store to pick up a layout
// change.
type Store struct {
	algo       objfmt.Algo
	refs       refBackend
	loose      *looseObjects
	packs      packBackend
	alternates []*Store
	cfg        storeConfig

	// closeOnce guards [Store.Close] so the cascade runs exactly once
	// even if the caller invokes Close repeatedly. closeErr stashes the
	// joined error so subsequent calls return the same value.
	closeOnce sync.Once
	closeErr  error
}

// Option configures [Open]. Options are applied after [readGitConfig]
// has populated the format-derived fields, so callers can override
// behaviour the on-disk config does not control (CRC verification,
// future tunables) without losing the algo / refStorage decisions
// the repository itself dictates.
type Option func(*storeConfig)

// WithoutCRCCheck disables per-object CRC32 verification on pack-index
// reads. Per-object CRC32 verification is enabled by default; passing
// this option trades a small integrity guarantee for a measurable
// throughput gain on cold reads of large packs. Callers that operate
// on trusted, locally-administered repositories may enable this; in
// any context where the on-disk state could have been tampered with
// the default protects against silent corruption.
func WithoutCRCCheck() Option {
	return func(c *storeConfig) { c.verifyCRC = false }
}

// Open resolves path to a usable [Store]. The lookup proceeds in four
// stages so each piece can fail with a focused error:
//
//  1. [resolveGitDir] turns path into the gitdir / common-dir pair.
//  2. [readGitConfig] parses `<commonDir>/config` for the algo and
//     ref-storage decisions.
//  3. Each [Option] mutates the resulting [storeConfig], layered on
//     top of the parsed values so option-driven flags such as
//     [WithoutCRCCheck] survive.
//  4. The ref, pack, loose-object, and alternates backends are
//     constructed, in that order. A failure at any step closes the
//     already-opened backends before returning.
//
// Errors from each step wrap the originating sentinel
// ([ErrNotARepo], [ErrUnsupportedFormat]) so callers can match with
// [errors.Is].
func Open(path string, opts ...Option) (*Store, error) {
	return openWithSeen(path, opts, map[string]bool{})
}

// openWithSeen is the alternates-aware constructor [Open] delegates to.
// The seen set tracks canonical gitdir paths already visited along the
// current alternates chain so [openAlternates] can detect cycles when
// recursing through transitive alternates. Public callers reach this
// through [Open] with a fresh empty set; [openAlternates] forwards its
// growing set into recursive opens of each child store.
func openWithSeen(path string, opts []Option, seen map[string]bool) (*Store, error) {
	gitDir, commonDir, err := resolveGitDir(path)
	if err != nil {
		return nil, err
	}

	cfg, err := readGitConfig(commonDir)
	if err != nil {
		return nil, err
	}
	for _, opt := range opts {
		opt(&cfg)
	}

	// Track every backend opened so a failure midway through tears down
	// the partially-constructed Store rather than leaking file handles.
	var opened []io.Closer
	closeAll := func() {
		for _, c := range opened {
			_ = c.Close()
		}
	}

	refs, err := openRefBackend(gitDir, commonDir, cfg)
	if err != nil {
		return nil, err
	}
	opened = append(opened, refs)

	loose, err := openLoose(commonDir, cfg.algo)
	if err != nil {
		closeAll()
		return nil, err
	}
	opened = append(opened, loose)

	packs, err := openPackBackend(commonDir, cfg)
	if err != nil {
		closeAll()
		return nil, err
	}
	opened = append(opened, packs)

	// Mark this store's canonical commonDir as in-flight on the active
	// alternates chain, then pop on return so a sibling alternate that
	// legitimately reaches the same store via a different path (a
	// diamond DAG) is not mis-classified as a cycle. The commonDir
	// (rather than gitdir) is the right key because an alternate entry
	// names another store's `objects/` directory, whose parent is by
	// definition that store's commonDir — comparing on the same axis
	// avoids a worktree's per-tree gitdir falsely escaping the check.
	canonical := canonicalRepoDir(commonDir)
	seen[canonical] = true
	defer delete(seen, canonical)

	alternates, err := openAlternates(commonDir, cfg, seen)
	if err != nil {
		closeAll()
		return nil, err
	}

	return &Store{
		algo:       cfg.algo,
		refs:       refs,
		loose:      loose,
		packs:      packs,
		alternates: alternates,
		cfg:        cfg,
	}, nil
}

// Algo reports the object hash algorithm in use, derived from the
// repository's `extensions.objectFormat` config (defaulting to
// [objfmt.SHA1] when absent).
func (s *Store) Algo() objfmt.Algo { return s.algo }

// Close releases the backends and every alternate Store in the chain.
// Errors from each step are joined via [errors.Join] so a single
// failure does not mask the rest. Close is idempotent: subsequent
// calls return the same error without re-running the cascade.
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		var errs []error
		if s.refs != nil {
			if err := s.refs.Close(); err != nil {
				errs = append(errs, err)
			}
		}
		if s.loose != nil {
			if err := s.loose.Close(); err != nil {
				errs = append(errs, err)
			}
		}
		if s.packs != nil {
			if err := s.packs.Close(); err != nil {
				errs = append(errs, err)
			}
		}
		for _, alt := range s.alternates {
			if err := alt.Close(); err != nil {
				errs = append(errs, err)
			}
		}
		s.closeErr = errors.Join(errs...)
	})
	return s.closeErr
}

// openRefBackend dispatches on the parsed `extensions.refStorage`
// value. The config parser already rejects unknown formats with
// [ErrUnsupportedFormat], so the default branch here is defence in
// depth — it would only fire if the parser ever grew a new
// recognised value the opener had not been taught about.
func openRefBackend(gitDir, commonDir string, cfg storeConfig) (refBackend, error) {
	switch cfg.refStorage.format {
	case "files":
		return openLooseRefs(gitDir, commonDir, cfg.algo)
	case "reftable":
		return openReftableBackend(gitDir, commonDir, cfg.refStorage.location)
	default:
		return nil, fmt.Errorf("objstore: refStorage=%q: %w",
			cfg.refStorage.format, ErrUnsupportedFormat)
	}
}

// openPackBackend selects the pack backend by the presence of
// `<commonDir>/objects/pack/multi-pack-index`. Canonical Git keeps
// per-pack `.idx` files alongside the midx for compatibility, so this
// is a one-way preference: midx wins whenever it exists, regardless of
// how many `.idx` files sit beside it.
func openPackBackend(commonDir string, cfg storeConfig) (packBackend, error) {
	midx := filepath.Join(commonDir, "objects", "pack", "multi-pack-index")
	if _, err := os.Stat(midx); err == nil {
		return openMidxBackend(commonDir, cfg.algo)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("objstore: stat %s: %w", midx, err)
	}
	return openIdxCatalog(commonDir, cfg.algo)
}

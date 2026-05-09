package objstore

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
)

// looseObjects reads objects stored under
// `<commonDir>/objects/<aa>/<rest-of-hash>` — the canonical loose
// object layout. Always present alongside whichever pack backend the
// store opener selects, since loose objects can coexist with packs.
//
// The backend holds only the immutable commonDir and the algo bound to
// the [Store]. [looseObjects.Find] is therefore safe for concurrent use
// by multiple goroutines without synchronisation; each call performs an
// independent `os.Open` and returns its own [io.ReadCloser] over the
// resulting file handle.
type looseObjects struct {
	commonDir string
	algo      objfmt.Algo
}

// openLoose constructs a [looseObjects] backed by commonDir. algo is
// the hash algorithm bound to the store; it controls the hex-fanout
// width used by [looseObjects.Find] (40 chars for SHA-1, 64 for
// SHA-256). The constructor performs no I/O — `objects/` may be
// missing entirely on a brand-new repo, and per-object errors surface
// from [looseObjects.Find] when callers actually request a hash.
func openLoose(commonDir string, algo objfmt.Algo) (*looseObjects, error) {
	return &looseObjects{commonDir: commonDir, algo: algo}, nil
}

// Find looks up the loose object identified by h. The first two hex
// characters of h are the fanout subdirectory; the remaining 38
// (SHA-1) or 62 (SHA-256) form the file name. ok is false when no such
// file exists; the error slot is reserved for genuine I/O or parse
// failures so callers can distinguish a clean miss from a degraded
// backend.
//
// Ownership: when ok is true, body is non-nil and the caller MUST close
// it. body wraps a zlib decoder over the open file; closing it
// releases both the decoder state and the underlying file handle. On
// ok=false (miss) or err != nil the file is closed before Find
// returns.
//
// Errors fall into two buckets. An [os.ErrNotExist] from the open
// surfaces as a miss with a nil error — the canonical "no such object
// here, try the next backend" shape. Every other open error (notably
// permission denied) is propagated wrapped as
// `objstore: open <path>: %w`. Header / zlib parse failures are
// wrapped as `objstore: read <path>: %w` and chained through
// [ErrCorruptObject] so callers can match with
// `errors.Is(err, ErrCorruptObject)`.
func (l *looseObjects) Find(h objfmt.Hash) (typ objfmt.ObjectType, size int64, body io.ReadCloser, ok bool, err error) {
	hex := h.Hex(l.algo)
	if len(hex) < 3 {
		// Defence in depth: an unknown algo yields an empty hex string,
		// and constructing a path from `objects//` would silently match
		// the directory itself. Surface the misuse rather than open it.
		return 0, 0, nil, false, fmt.Errorf(
			"objstore: loose lookup with unknown algo %v", l.algo)
	}
	path := filepath.Join(l.commonDir, "objects", hex[:2], hex[2:])

	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Missing fanout directory and missing leaf both surface as
			// ENOENT; either way, this backend has nothing for h.
			return 0, 0, nil, false, nil
		}
		return 0, 0, nil, false, fmt.Errorf("objstore: open %s: %w", path, err)
	}

	typ, size, inner, err := objfmt.ReadLooseHeader(f)
	if err != nil {
		_ = f.Close()
		// `objfmt.ReadLooseHeader` only fails on malformed bytes (zlib
		// framing, missing space / NUL, unparseable size, unknown type
		// name); chain through [ErrCorruptObject] so callers can match
		// the structural-corruption sentinel.
		return 0, 0, nil, false,
			fmt.Errorf("objstore: read %s: %w: %w", path, err, ErrCorruptObject)
	}

	// `inner` from objfmt.ReadLooseHeader closes only the zlib decoder.
	// The file handle's lifecycle is ours to manage; pair them in a
	// single Closer so the caller's body.Close() releases both.
	return typ, size, &looseFileBody{inner: inner, file: f}, true, nil
}

// Close releases the backend. The eager-load constructor holds no file
// handles or memory mappings; the per-call body returned to callers is
// owned by them.
func (l *looseObjects) Close() error { return nil }

// looseFileBody adapts the [io.ReadCloser] returned by
// [objfmt.ReadLooseHeader] (which closes only the zlib decoder) and
// the underlying *os.File into a single [io.ReadCloser] whose Close
// releases both. The decoder is closed first so any trailer-corruption
// error it surfaces is not masked by a subsequent file-close failure;
// errors from both are joined.
type looseFileBody struct {
	inner io.ReadCloser
	file  *os.File
}

func (b *looseFileBody) Read(p []byte) (int, error) { return b.inner.Read(p) }

// Close releases the inner zlib decoder and the file handle. Errors
// from both are surfaced via [errors.Join] so a trailer-corruption
// report from the decoder is not masked by a clean file close (or
// vice versa). Callers are expected to Close exactly once; the
// underlying *os.File returns [os.ErrClosed] on a second call, which
// would surface here.
func (b *looseFileBody) Close() error {
	innerErr := b.inner.Close()
	fileErr := b.file.Close()
	return errors.Join(innerErr, fileErr)
}

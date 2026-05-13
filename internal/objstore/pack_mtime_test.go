package objstore

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
)

// packMtimeAnchor is a fixed reference timestamp the pack-ordering
// tests anchor their `os.Chtimes` calls against. Pinned to a value
// well in the past so even an unusually slow CI host cannot land
// `time.Now()` close enough to interfere.
var packMtimeAnchor = time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC)

// stampPackMtimes restamps the access and modification times of one
// or more pack files under `<gitDir>/objects/pack/` to deterministic
// values. The map keys are pack basenames (e.g. `three-objects.pack`),
// the values are the desired mtimes.
//
// Pack ordering keys on the pack file itself (canonical Git's
// `a->pack->mtime` in [packfile.c::sort_pack]); the paired idx mtime
// is intentionally untouched so a real-world repack scenario, where
// the idx is rewritten alongside its pack, is not implied.
//
// [packfile.c::sort_pack]: https://github.com/git/git/blob/v2.54.0/packfile.c#L1042
func stampPackMtimes(t *testing.T, gitDir string, mtimes map[string]time.Time) {
	t.Helper()
	for name, when := range mtimes {
		path := filepath.Join(gitDir, "objects", "pack", name)
		require.NoError(t, os.Chtimes(path, when, when),
			"chtimes %s", path)
	}
}

// idxForPackInCatalog returns the `*objfmt.Idx[objfmt.SHA1Hash]` paired with pack
// inside c. The catalog's `*objfmt.Pack[objfmt.SHA1Hash]` instances do not carry a
// path of their own, so tests reach for the basename through the
// paired idx — every (idx, pack) pair shares a basename.
func idxForPackInCatalog(t *testing.T, c *idxCatalog[objfmt.SHA1Hash], pack *objfmt.Pack[objfmt.SHA1Hash]) *objfmt.Idx[objfmt.SHA1Hash] {
	t.Helper()
	for _, e := range c.packs {
		if e.pack == pack {
			return e.idx
		}
	}
	t.Fatalf("pack %p not found in catalog", pack)
	return nil
}

// idxForPackInMidx returns the `*objfmt.Idx[objfmt.SHA1Hash]` paired with pack inside
// b, scanning both the midx-covered and sibling slots. Same rationale
// as [idxForPackInCatalog].
func idxForPackInMidx(t *testing.T, b *midxBackend[objfmt.SHA1Hash], pack *objfmt.Pack[objfmt.SHA1Hash]) *objfmt.Idx[objfmt.SHA1Hash] {
	t.Helper()
	for i, p := range b.coveredByMidxIndex {
		if p == pack {
			return b.coveredIdxs[i]
		}
	}
	for _, e := range b.siblings {
		if e.pack == pack {
			return e.idx
		}
	}
	t.Fatalf("pack %p not found in midx backend", pack)
	return nil
}

// packFixtureRoot returns the on-disk `<gitDir>` for the named
// fixture without copying through `materializeFixture`. Used as a
// byte source for [clonePackPair] when a test builds a synthetic
// catalog directory in a fresh `t.TempDir()`.
func packFixtureRoot(t *testing.T, name string) string {
	t.Helper()
	root := materializeFixture(t, name)
	return filepath.Join(root, ".git")
}

// clonePackPair copies a `(<srcBase>.pack, <srcBase>.idx)` pair from
// srcDir into dstDir under dstBase. Used to materialize two distinct
// packs that share the same OID set so the ordering tests can assert
// "younger pack wins" without reaching for canonical Git to repack.
func clonePackPair(t *testing.T, srcDir, srcBase, dstDir, dstBase string) {
	t.Helper()
	for _, ext := range []string{".pack", ".idx"} {
		src := filepath.Join(srcDir, srcBase+ext)
		dst := filepath.Join(dstDir, dstBase+ext)
		data, err := os.ReadFile(src)
		require.NoError(t, err, "read %s", src)
		require.NoError(t, os.WriteFile(dst, data, 0o644),
			"write %s", dst)
	}
}

package objstore

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scaffoldAltRepo materializes a minimal repository skeleton (HEAD plus
// the empty `objects/`, `objects/pack/`, `objects/info/`, `refs/`
// directories) under root. Used by the absolute-path / synthetic
// alternates tests where committed fixtures cannot encode a
// host-specific tempdir.
func scaffoldAltRepo(t *testing.T, root string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "objects", "pack"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "objects", "info"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "refs"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "HEAD"),
		[]byte("ref: refs/heads/main\n"),
		0o644,
	))
}

func TestOpenAlternates_NoFile(t *testing.T) {
	// A repository without `objects/info/alternates` must surface an
	// empty chain with no error: alternates are optional.
	root := materializeFixture(t, "empty")
	commonDir := filepath.Join(root, ".git")

	got, err := openAlternates[objfmt.SHA1Hash](commonDir, map[string]bool{})
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestOpenAlternates_EmptyFile(t *testing.T) {
	// An empty alternates file is valid and yields zero entries: the
	// parser must skip without surfacing a parse error.
	root := materializeFixture(t, "empty")
	commonDir := filepath.Join(root, ".git")
	require.NoError(t, os.MkdirAll(filepath.Join(commonDir, "objects", "info"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(commonDir, "objects", "info", "alternates"),
		[]byte(""),
		0o644,
	))

	got, err := openAlternates[objfmt.SHA1Hash](commonDir, map[string]bool{})
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestOpenAlternates_CommentsOnly(t *testing.T) {
	// Lines beginning with `#` are comments and skipped. A file that is
	// pure comments yields zero entries.
	root := materializeFixture(t, "empty")
	commonDir := filepath.Join(root, ".git")
	require.NoError(t, os.MkdirAll(filepath.Join(commonDir, "objects", "info"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(commonDir, "objects", "info", "alternates"),
		[]byte("# this is a comment\n# another comment\n\n"),
		0o644,
	))

	got, err := openAlternates[objfmt.SHA1Hash](commonDir, map[string]bool{})
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestOpenAlternates_SingleAbsolutePath(t *testing.T) {
	// Absolute alternate path: the host-specific tempdir cannot be
	// committed as a fixture, so build the parent + alternate inline.
	// The resolved alternate must show up as a *Store[objfmt.SHA1Hash] in the returned
	// slice and its commonDir must match the alternate's gitdir.
	tmp := t.TempDir()
	parent := filepath.Join(tmp, "parent")
	alt := filepath.Join(tmp, "alt")
	scaffoldAltRepo(t, parent)
	scaffoldAltRepo(t, alt)
	require.NoError(t, os.WriteFile(
		filepath.Join(parent, "objects", "info", "alternates"),
		[]byte(filepath.Join(alt, "objects")+"\n"),
		0o644,
	))

	got, err := openAlternates[objfmt.SHA1Hash](parent,
		map[string]bool{canonicalRepoDir(parent): true})
	require.NoError(t, err)
	require.Len(t, got, 1)
	t.Cleanup(func() { _ = got[0].Close() })

	// Confirm the alternate opened against the right gitdir. `Store[objfmt.SHA1Hash]`
	// does not surface its commonDir directly, but the loose-objects
	// backend does — verify via its commonDir field.
	assert.Equal(t, canonicalRepoDir(alt), canonicalRepoDir(got[0].loose.commonDir))
}

func TestOpenAlternates_SingleRelativePath(t *testing.T) {
	// `with-alternates-relative/main` carries an alternates file
	// pointing at the sibling `alt` repo via a relative path resolved
	// against `main/.git/objects/`. After materialization the path
	// resolves to a real `objects/` directory.
	root := materializeFixture(t, "with-alternates-relative")
	mainCommonDir := filepath.Join(root, "main", ".git")
	altGitDir := filepath.Join(root, "alt", ".git")

	got, err := openAlternates[objfmt.SHA1Hash](mainCommonDir,
		map[string]bool{canonicalRepoDir(mainCommonDir): true})
	require.NoError(t, err)
	require.Len(t, got, 1)
	t.Cleanup(func() { _ = got[0].Close() })

	assert.Equal(t, canonicalRepoDir(altGitDir),
		canonicalRepoDir(got[0].loose.commonDir))
}

func TestOpenAlternates_QuotedPath(t *testing.T) {
	// Build a quoted path with a space inside. The unquoter must strip
	// the surrounding double quotes and the alternate must open against
	// the literal directory name (`alt store`).
	tmp := t.TempDir()
	parent := filepath.Join(tmp, "parent")
	alt := filepath.Join(tmp, "alt store")
	scaffoldAltRepo(t, parent)
	scaffoldAltRepo(t, alt)
	require.NoError(t, os.WriteFile(
		filepath.Join(parent, "objects", "info", "alternates"),
		[]byte("\""+filepath.Join(alt, "objects")+"\"\n"),
		0o644,
	))

	got, err := openAlternates[objfmt.SHA1Hash](parent,
		map[string]bool{canonicalRepoDir(parent): true})
	require.NoError(t, err)
	require.Len(t, got, 1)
	t.Cleanup(func() { _ = got[0].Close() })

	assert.Equal(t, canonicalRepoDir(alt),
		canonicalRepoDir(got[0].loose.commonDir))
}

func TestUnquoteCStyle_BackslashEscapes(t *testing.T) {
	// Tabletop check that the C-style unquoter handles every escape
	// canonical Git's [quote.c::unquote_c_style] accepts: the quoted
	// form must round-trip through the documented runes. Kept as a
	// table-driven unit test rather than a fixture-backed alternates
	// test because writing literal backslashes through a path-resolving
	// `Open` would conflate parsing with filesystem semantics.
	//
	// [quote.c::unquote_c_style]: https://github.com/git/git/blob/v2.54.0/quote.c#L403
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", `"hello"`, "hello"},
		{"escaped-quote", `"a\"b"`, `a"b`},
		{"escaped-backslash", `"a\\b"`, `a\b`},
		{"newline", `"a\nb"`, "a\nb"},
		{"tab", `"a\tb"`, "a\tb"},
		{"bell", `"a\ab"`, "a\ab"},
		{"backspace", `"a\bb"`, "a\bb"},
		{"formfeed", `"a\fb"`, "a\fb"},
		{"carriage-return", `"a\rb"`, "a\rb"},
		{"vertical-tab", `"a\vb"`, "a\vb"},
		{"octal-low", `"\101"`, "A"},    // 'A' = 0o101
		{"octal-mid", `"\377"`, "\xff"}, // max byte through three-digit octal
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := unquoteCStyle([]byte(tc.in))
			require.True(t, ok, "input %q should unquote", tc.in)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestUnquoteCStyle_BrokenInputReturnsFalse(t *testing.T) {
	// Malformed inputs return ok=false so the alternates parser can
	// fall back to the literal line.
	bad := []string{
		`"unterminated`,
		`"bad-escape\zhere"`,
		`"trailing-backslash\"`,
		`"short-octal\1"`,
		`"non-octal-digit\189"`,
	}
	for _, in := range bad {
		t.Run(in, func(t *testing.T) {
			_, ok := unquoteCStyle([]byte(in))
			assert.False(t, ok, "input %q should not unquote", in)
		})
	}
}

func TestOpenAlternates_MultipleEntriesPreserveOrder(t *testing.T) {
	// Three alternates listed in a stable order; the returned slice
	// must reflect the same order so callers traversing the chain see
	// the configured priority.
	tmp := t.TempDir()
	parent := filepath.Join(tmp, "parent")
	alt1 := filepath.Join(tmp, "alt1")
	alt2 := filepath.Join(tmp, "alt2")
	alt3 := filepath.Join(tmp, "alt3")
	scaffoldAltRepo(t, parent)
	for _, p := range []string{alt1, alt2, alt3} {
		scaffoldAltRepo(t, p)
	}
	require.NoError(t, os.WriteFile(
		filepath.Join(parent, "objects", "info", "alternates"),
		[]byte(filepath.Join(alt1, "objects")+"\n"+
			filepath.Join(alt2, "objects")+"\n"+
			filepath.Join(alt3, "objects")+"\n"),
		0o644,
	))

	got, err := openAlternates[objfmt.SHA1Hash](parent,
		map[string]bool{canonicalRepoDir(parent): true})
	require.NoError(t, err)
	require.Len(t, got, 3)
	t.Cleanup(func() {
		for _, s := range got {
			_ = s.Close()
		}
	})

	want := []string{alt1, alt2, alt3}
	for i, p := range want {
		assert.Equal(t, canonicalRepoDir(p),
			canonicalRepoDir(got[i].loose.commonDir),
			"alternate %d should resolve to %s", i, p)
	}
}

func TestOpenAlternates_TransitiveChain(t *testing.T) {
	// `with-alternates-chain/a` -> b -> c. Opening A surfaces B as its
	// sole alternate; B's own `s.alternates` carries C. The chain is
	// per-Store[objfmt.SHA1Hash] rather than flattened into a single slice.
	root := materializeFixture(t, "with-alternates-chain")
	a := filepath.Join(root, "a")

	s, err := Open[objfmt.SHA1Hash](a)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	require.Len(t, s.alternates, 1, "A should carry exactly one direct alternate (B)")
	b := s.alternates[0]
	assert.Equal(t,
		canonicalRepoDir(filepath.Join(root, "b", ".git")),
		canonicalRepoDir(b.loose.commonDir))

	require.Len(t, b.alternates, 1, "B should carry exactly one direct alternate (C)")
	c := b.alternates[0]
	assert.Equal(t,
		canonicalRepoDir(filepath.Join(root, "c", ".git")),
		canonicalRepoDir(c.loose.commonDir))

	assert.Empty(t, c.alternates, "C should have no further alternates")
}

func TestOpenAlternates_SelfCycle(t *testing.T) {
	// `with-alternates-cycle/` lists its own `objects/` directory. The
	// recursive open re-encounters the parent's canonical gitdir in the
	// `seen` set and surfaces ErrCorruptObject naming that gitdir.
	root := materializeFixture(t, "with-alternates-cycle")

	_, err := Open[objfmt.SHA1Hash](root)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrCorruptObject),
		"expected ErrCorruptObject, got %v", err)
	assert.Contains(t, err.Error(), canonicalRepoDir(filepath.Join(root, ".git")))
}

func TestOpenAlternates_ChainCycle(t *testing.T) {
	// `with-alternates-cycle-chain/` is A -> B -> A. Opening A descends
	// into B, which then re-encounters A's canonical gitdir on its own
	// alternates lookup. The error must wrap ErrCorruptObject and name
	// A (the first repo to be re-encountered).
	root := materializeFixture(t, "with-alternates-cycle-chain")
	a := filepath.Join(root, "a")

	_, err := Open[objfmt.SHA1Hash](a)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrCorruptObject),
		"expected ErrCorruptObject, got %v", err)
	assert.Contains(t, err.Error(),
		canonicalRepoDir(filepath.Join(root, "a", ".git")),
		"cycle error must name A's gitdir")
}

func TestOpenAlternates_PopulatedOnOpen(t *testing.T) {
	// End-to-end via the public `Open` entry point: a repo with an
	// alternate must surface that alternate on `s.alternates` so the
	// follow-up object-lookup methods can fan out across the chain.
	// Today no methods consume `s.alternates`; this assertion locks
	// in the wiring so the upcoming `ObjectInfo` lookup can rely on it.
	root := materializeFixture(t, "with-alternates-relative")
	mainPath := filepath.Join(root, "main")

	s, err := Open[objfmt.SHA1Hash](mainPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	require.Len(t, s.alternates, 1)
	assert.Equal(t,
		canonicalRepoDir(filepath.Join(root, "alt", ".git")),
		canonicalRepoDir(s.alternates[0].loose.commonDir))
}

func TestOpenAlternates_DiamondDAG(t *testing.T) {
	// A -> [B, C], B -> D, C -> D. The shared leaf is reachable
	// through two independent chains; canonical Git silently dedupes
	// the global source list and we mirror that intent: neither chain
	// must trip the cycle check, because by the time C's
	// `openAlternates` considers D, B's recursion has already finished
	// and popped D from the in-flight `seen` set. Each chain opens its
	// own `*Store[objfmt.SHA1Hash]` for D — no sharing — but neither errors.
	tmp := t.TempDir()
	a := filepath.Join(tmp, "a")
	b := filepath.Join(tmp, "b")
	c := filepath.Join(tmp, "c")
	d := filepath.Join(tmp, "d")
	for _, p := range []string{a, b, c, d} {
		scaffoldAltRepo(t, p)
	}
	require.NoError(t, os.WriteFile(
		filepath.Join(a, "objects", "info", "alternates"),
		[]byte(filepath.Join(b, "objects")+"\n"+
			filepath.Join(c, "objects")+"\n"),
		0o644,
	))
	for _, parent := range []string{b, c} {
		require.NoError(t, os.WriteFile(
			filepath.Join(parent, "objects", "info", "alternates"),
			[]byte(filepath.Join(d, "objects")+"\n"),
			0o644,
		))
	}

	s, err := Open[objfmt.SHA1Hash](a)
	require.NoError(t, err, "diamond DAG must not be mistaken for a cycle")
	t.Cleanup(func() { _ = s.Close() })

	require.Len(t, s.alternates, 2, "A should carry both direct alternates B and C")
	for i, parent := range []string{b, c} {
		assert.Equal(t,
			canonicalRepoDir(parent),
			canonicalRepoDir(s.alternates[i].loose.commonDir),
			"alternate %d should resolve to %s", i, parent)
		require.Len(t, s.alternates[i].alternates, 1,
			"each parent should carry exactly one alternate (D)")
		assert.Equal(t,
			canonicalRepoDir(d),
			canonicalRepoDir(s.alternates[i].alternates[0].loose.commonDir),
			"the shared leaf must resolve to D from both chains")
	}
}

func TestOpenAlternates_NonExistentTargetReturnsError(t *testing.T) {
	// An alternates entry pointing at a path that is not a repository
	// must surface an error wrapping the recursive `Open` failure with
	// the alternate's own path for diagnostic context.
	tmp := t.TempDir()
	parent := filepath.Join(tmp, "parent")
	scaffoldAltRepo(t, parent)
	bogus := filepath.Join(tmp, "does-not-exist", "objects")
	require.NoError(t, os.WriteFile(
		filepath.Join(parent, "objects", "info", "alternates"),
		[]byte(bogus+"\n"),
		0o644,
	))

	_, err := openAlternates[objfmt.SHA1Hash](parent,
		map[string]bool{canonicalRepoDir(parent): true})
	require.Error(t, err)
	// The recursive `openWithSeen` surfaces ErrNotARepo for a missing
	// path; the alternates wrapper layers in the alternate's gitdir for
	// diagnostic context.
	assert.True(t, errors.Is(err, ErrNotARepo),
		"expected ErrNotARepo, got %v", err)
}

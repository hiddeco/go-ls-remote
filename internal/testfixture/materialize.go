// Package testfixture materialises on-disk repository fixtures from
// `testdata/repos/<name>/` into a fresh `t.TempDir()`. The fixtures
// are checked into the tree under a `dotgit/` directory rather than a
// literal `.git/` because canonical Git refuses to track a path with a
// `.git` component ([read-cache.c::verify_path_internal] guards the
// index with [is_hfs_dotgit] and [is_ntfs_dotgit] checks); this
// package renames the component on materialisation.
//
// The package is test-support code and is never imported from
// production paths.
//
// [read-cache.c::verify_path_internal]: https://github.com/git/git/blob/v2.54.0/read-cache.c#L987
// [is_hfs_dotgit]: https://github.com/git/git/blob/v2.54.0/utf8.c#L784
// [is_ntfs_dotgit]: https://github.com/git/git/blob/v2.54.0/path.c#L1415
package testfixture

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// fixturesRoot resolves to `<module>/testdata/repos`. The path is
// derived from this source file's location via [runtime.Caller] rather
// than `os.Getwd()` so callers at any depth resolve fixtures
// identically; the prior duplicates each assumed a two-levels-deep
// caller package and computed `..`/`..` against the working
// directory.
func fixturesRoot(t testing.TB) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller(0) failed; package layout changed?")
	// `file` is `<module>/internal/testfixture/materialize.go`; walk
	// up to the module root and join `testdata/repos`.
	return filepath.Join(filepath.Dir(file), "..", "..", "testdata", "repos")
}

// MaterializeRepoTree copies the named fixture from
// `testdata/repos/<name>/` into a fresh `t.TempDir()`, renaming any
// `dotgit` path component to `.git`, and returns the destination root.
//
// Use this for fixtures whose top-level layout is not a single gitdir
// — for example `with-alternates-chain` ships sibling repos under
// `a/`, `b/`, `c/` with no top-level `dotgit/`. For the common
// single-repo case, prefer [MaterializeRepo].
func MaterializeRepoTree(t testing.TB, name string) string {
	t.Helper()

	src := filepath.Join(fixturesRoot(t), name)
	info, err := os.Stat(src)
	require.NoError(t, err,
		"fixture %q missing; regenerate with testdata/_gen/repos.sh",
		name)
	require.True(t, info.IsDir(), "fixture %q is not a directory", name)

	dst := t.TempDir()
	require.NoError(t, filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		var parts []string
		if rel != "." {
			parts = strings.Split(filepath.ToSlash(rel), "/")
		}
		for i, part := range parts {
			if part == "dotgit" {
				parts[i] = ".git"
			}
		}
		target := filepath.Join(append([]string{dst}, parts...)...)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	}))
	return dst
}

// MaterializeRepo materialises the named single-repo fixture and
// returns the path to its `.git` directory under the destination root.
// The function fails the test if `<dst>/.git` does not exist after
// materialisation — that condition signals a multi-repo layout that
// should use [MaterializeRepoTree] instead.
func MaterializeRepo(t testing.TB, name string) string {
	t.Helper()
	dst := MaterializeRepoTree(t, name)
	gitdir := filepath.Join(dst, ".git")
	info, err := os.Stat(gitdir)
	require.NoError(t, err,
		"fixture %q has no top-level dotgit/; use MaterializeRepoTree",
		name)
	require.True(t, info.IsDir(), "%s is not a directory", gitdir)
	return gitdir
}

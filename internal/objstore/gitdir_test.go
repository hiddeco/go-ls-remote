package objstore

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// materializeFixture copies a committed fixture tree from
// `testdata/repos/<name>/` into a fresh `t.TempDir()`, renaming any
// path component called `dotgit` to `.git` on the way through.
//
// Canonical `git` refuses to track paths containing a literal `.git`
// component ([read-cache.c::verify_path_internal] guards the index
// with [is_hfs_dotgit] and [is_ntfs_dotgit] checks), so the on-disk
// fixtures store their `.git` artefacts under the name `dotgit`. This
// helper reverses that rename so the resolver under test sees the
// same shape it would see on a real working tree. Tests still never
// shell out; they only read the committed bytes and write them back
// unchanged.
//
// [read-cache.c::verify_path_internal]: https://github.com/git/git/blob/v2.54.0/read-cache.c#L987
// [is_hfs_dotgit]: https://github.com/git/git/blob/v2.54.0/utf8.c#L784
// [is_ntfs_dotgit]: https://github.com/git/git/blob/v2.54.0/path.c#L1415
func materializeFixture(t *testing.T, name string) string {
	t.Helper()

	wd, err := os.Getwd()
	require.NoError(t, err)
	src := filepath.Join(wd, "..", "..", "testdata", "repos", name)
	info, err := os.Stat(src)
	require.NoError(t, err, "fixture %q missing; regenerate with testdata/_gen/repos.sh", name)
	require.True(t, info.IsDir(), "fixture %q is not a directory", name)

	dst := t.TempDir()
	err = filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		// Rewrite every `dotgit` component to `.git`. Doing this on
		// the relative path covers both files (`dotgit`) and
		// directories (`dotgit/HEAD`, `dotgit/worktrees/...`).
		parts := splitAll(rel)
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
	})
	require.NoError(t, err)
	return dst
}

// writeMinimalGitDir lays down the three signatures required by
// canonical Git's [setup.c::is_git_directory] — a regular `HEAD`
// file plus empty `objects/` and `refs/` directories — at dir.
// Tests that synthesise a gitdir in `t.TempDir()` use this helper
// so the resolver accepts the fixture under the tightened rule.
//
// [setup.c::is_git_directory]: https://github.com/git/git/blob/v2.54.0/setup.c#L416
func writeMinimalGitDir(t *testing.T, dir string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "HEAD"),
		[]byte("ref: refs/heads/main\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "objects"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "refs"), 0o755))
}

// splitAll splits a relative path into its components without leaving
// a leading `.` for in-place paths.
func splitAll(p string) []string {
	if p == "." || p == "" {
		return nil
	}
	var parts []string
	for {
		dir, base := filepath.Split(p)
		parts = append([]string{base}, parts...)
		if dir == "" {
			return parts
		}
		p = filepath.Clean(dir)
		if p == "." {
			return parts
		}
	}
}

func TestResolveGitDir_PathIsGitDir(t *testing.T) {
	// Rule 1: the supplied path itself satisfies `is_git_directory`,
	// so it is the gitdir. A bare repo (or any `.git/` directory
	// passed directly) hits this branch.
	dir := t.TempDir()
	writeMinimalGitDir(t, dir)

	gitDir, commonDir, err := resolveGitDir(dir)
	require.NoError(t, err)
	assert.Equal(t, filepath.Clean(dir), gitDir)
	assert.Equal(t, gitDir, commonDir, "no commondir present, commonDir must equal gitDir")
}

func TestResolveGitDir_DotGitSubdirectory(t *testing.T) {
	// Rule 2: the supplied path contains a `.git` subdirectory. The
	// resolver descends into it and returns the subdirectory.
	work := t.TempDir()
	gitDir := filepath.Join(work, ".git")
	writeMinimalGitDir(t, gitDir)

	got, commonDir, err := resolveGitDir(work)
	require.NoError(t, err)
	assert.Equal(t, filepath.Clean(gitDir), got)
	assert.Equal(t, got, commonDir)
}

func TestResolveGitDir_DotGitFileAbsolute(t *testing.T) {
	// Rule 3a: `.git` is a regular file whose `gitdir:` directive
	// references an absolute path. The absolute path is returned
	// verbatim (after `filepath.Clean`).
	work := t.TempDir()
	target := t.TempDir()
	writeMinimalGitDir(t, target)
	payload := []byte("gitdir: " + target + "\n")
	require.NoError(t, os.WriteFile(filepath.Join(work, ".git"), payload, 0o644))

	got, commonDir, err := resolveGitDir(work)
	require.NoError(t, err)
	assert.Equal(t, filepath.Clean(target), got)
	assert.Equal(t, got, commonDir)
}

func TestResolveGitDir_DotGitFileRelative(t *testing.T) {
	// Rule 3b: a relative `gitdir:` path resolves against the
	// directory containing the `.git` file. The fixture
	// `worktree-as-file/linked/.git` points at
	// `../main/.git/worktrees/linked` and ships with a `commondir`
	// pointing at `../..`, exercising the worktree common-dir
	// indirection in one go.
	root := materializeFixture(t, "worktree-as-file")
	work := filepath.Join(root, "linked")

	gitDir, commonDir, err := resolveGitDir(work)
	require.NoError(t, err)

	wantGitDir := filepath.Clean(filepath.Join(root, "main", ".git", "worktrees", "linked"))
	assert.Equal(t, wantGitDir, gitDir)

	wantCommonDir := filepath.Clean(filepath.Join(root, "main", ".git"))
	assert.Equal(t, wantCommonDir, commonDir)
}

func TestResolveGitDir_SubmoduleAsFile(t *testing.T) {
	// Submodule shape: the submodule working tree's `.git` is a file
	// pointing at `../.git/modules/sub` in the parent repo. No
	// `commondir` is present, so `commonDir` equals `gitDir`.
	root := materializeFixture(t, "submodule-as-file")
	work := filepath.Join(root, "parent", "sub")

	gitDir, commonDir, err := resolveGitDir(work)
	require.NoError(t, err)

	wantGitDir := filepath.Clean(filepath.Join(root, "parent", ".git", "modules", "sub"))
	assert.Equal(t, wantGitDir, gitDir)
	assert.Equal(t, gitDir, commonDir)
}

func TestResolveGitDir_NotARepo(t *testing.T) {
	// Rule 4: no `HEAD`, no `.git`. The error must wrap
	// [ErrNotARepo] so callers can match with [errors.Is].
	dir := t.TempDir()

	_, _, err := resolveGitDir(dir)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotARepo), "expected ErrNotARepo, got %v", err)
}

func TestResolveGitDir_DotGitFileInvalidPrefix(t *testing.T) {
	// Rule 3 only accepts the `gitdir: ` prefix. A file with any
	// other content is rejected as not-a-repo so callers can fall
	// through to the next discovery step.
	work := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(work, ".git"), []byte("not-a-pointer\n"), 0o644))

	_, _, err := resolveGitDir(work)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotARepo), "expected ErrNotARepo, got %v", err)
}

func TestResolveGitDir_DotGitFileEmptyTarget(t *testing.T) {
	// `gitdir:` with no path body is malformed; reject as not-a-repo
	// rather than returning an empty path that would later resolve to
	// the worktree itself.
	work := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(work, ".git"), []byte("gitdir: \n"), 0o644))

	_, _, err := resolveGitDir(work)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotARepo), "expected ErrNotARepo, got %v", err)
}

func TestResolveGitDir_CommondirPresentFixture(t *testing.T) {
	// Open the linked-worktree gitdir directly (not the working
	// tree). Rule 1 fires for `HEAD`, then `commondir` redirects the
	// common-dir to the parent repo's `.git/`.
	root := materializeFixture(t, "worktree-with-commondir")
	gitDirPath := filepath.Join(root, "repo", ".git", "worktrees", "wt")

	gitDir, commonDir, err := resolveGitDir(gitDirPath)
	require.NoError(t, err)
	assert.Equal(t, filepath.Clean(gitDirPath), gitDir)

	wantCommonDir := filepath.Clean(filepath.Join(root, "repo", ".git"))
	assert.Equal(t, wantCommonDir, commonDir)
}

func TestResolveGitDir_CommondirRelativeSynthetic(t *testing.T) {
	// Synthetic equivalent of the fixture case: open a gitdir whose
	// `commondir` points at a sibling directory via `../repo`.
	// `objects/` and `refs/` live under the common dir (matching
	// canonical Git's `is_git_directory`, which performs the
	// objects/refs check post-`get_common_dir`).
	root := t.TempDir()
	gitDirPath := filepath.Join(root, "wt")
	commonTarget := filepath.Join(root, "repo")
	writeMinimalGitDir(t, commonTarget)
	require.NoError(t, os.MkdirAll(gitDirPath, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(gitDirPath, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(gitDirPath, "commondir"), []byte("../repo\n"), 0o644))

	gitDir, commonDir, err := resolveGitDir(gitDirPath)
	require.NoError(t, err)
	assert.Equal(t, filepath.Clean(gitDirPath), gitDir)
	assert.Equal(t, filepath.Clean(commonTarget), commonDir)
}

func TestResolveGitDir_CommondirAbsent(t *testing.T) {
	// A bare gitdir without a `commondir` file: `commonDir` must
	// equal `gitDir`. Constructed in `t.TempDir()` so the test does
	// not depend on the larger fixture trees.
	dir := t.TempDir()
	writeMinimalGitDir(t, dir)

	gitDir, commonDir, err := resolveGitDir(dir)
	require.NoError(t, err)
	assert.Equal(t, gitDir, commonDir)
}

func TestResolveGitDir_HeadAlonePathIsRejected(t *testing.T) {
	// Canonical Git's [setup.c::is_git_directory] requires HEAD,
	// `objects/`, and `refs/` together. A directory carrying only a
	// stray `HEAD` file must not be accepted as a gitdir.
	//
	// [setup.c::is_git_directory]: https://github.com/git/git/blob/v2.54.0/setup.c#L416
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644))

	_, _, err := resolveGitDir(dir)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotARepo), "expected ErrNotARepo, got %v", err)
}

func TestResolveGitDir_HeadPlusObjectsButNoRefsIsRejected(t *testing.T) {
	// Two-out-of-three is not enough: `refs/` is independently
	// required by [setup.c::is_git_directory].
	//
	// [setup.c::is_git_directory]: https://github.com/git/git/blob/v2.54.0/setup.c#L416
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "objects"), 0o755))

	_, _, err := resolveGitDir(dir)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotARepo), "expected ErrNotARepo, got %v", err)
}

func TestResolveGitDir_HeadPlusRefsButNoObjectsIsRejected(t *testing.T) {
	// Symmetric to the previous case: missing `objects/` is fatal.
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "refs"), 0o755))

	_, _, err := resolveGitDir(dir)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotARepo), "expected ErrNotARepo, got %v", err)
}

func TestResolveGitDir_HeadIsDirectoryIsRejected(t *testing.T) {
	// Canonical Git's `validate_headref` rejects a HEAD that is not a
	// regular file (or symlink to one). A directory named `HEAD` must
	// not satisfy the gitdir signature.
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "HEAD"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "objects"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "refs"), 0o755))

	_, _, err := resolveGitDir(dir)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotARepo), "expected ErrNotARepo, got %v", err)
}

func TestResolveGitDir_GitfileTargetMustBeRealRepo(t *testing.T) {
	// A `.git` file resolves to a path; the resolved path must itself
	// satisfy `is_git_directory`. A directory with only a stray HEAD
	// at the resolved target is rejected just as a direct call would
	// be.
	work := t.TempDir()
	bogus := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bogus, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(work, ".git"),
		[]byte("gitdir: "+bogus+"\n"), 0o644))

	_, _, err := resolveGitDir(work)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotARepo), "expected ErrNotARepo, got %v", err)
}

func TestResolveGitDir_CommondirAbsolute(t *testing.T) {
	// An absolute `commondir` payload must be used verbatim rather
	// than re-rooted under the gitdir. Mirrors the canonical Git
	// behaviour in [setup.c::get_common_dir_noenv].
	//
	// [setup.c::get_common_dir_noenv]: https://github.com/git/git/blob/v2.54.0/setup.c#L324
	gitDirPath := t.TempDir()
	commonTarget := t.TempDir()
	writeMinimalGitDir(t, commonTarget)
	require.NoError(t, os.WriteFile(filepath.Join(gitDirPath, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(gitDirPath, "commondir"), []byte(commonTarget+"\n"), 0o644))

	gitDir, commonDir, err := resolveGitDir(gitDirPath)
	require.NoError(t, err)
	assert.Equal(t, filepath.Clean(gitDirPath), gitDir)
	assert.Equal(t, filepath.Clean(commonTarget), commonDir)
}

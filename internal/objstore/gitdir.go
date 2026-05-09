package objstore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// gitfilePrefix is the literal directive that introduces the gitdir
// pointer inside a `.git` regular file. Canonical Git rejects any
// other prefix in `setup.c::read_gitfile_gently`.
const gitfilePrefix = "gitdir: "

// resolveGitDir returns the git directory and the common directory
// for path. gitDir is where `HEAD`, `index`, and per-worktree state
// live; commonDir is where shared state — `refs`, `packed-refs`,
// `objects`, `config`, `reftable` — lives. They differ only for
// linked worktrees: the worktree's gitDir contains a `commondir` file
// pointing at the original repo's gitdir.
//
// The resolution order mirrors canonical Git's
// `setup.c::is_git_directory` and `setup.c::get_common_dir_noenv`:
//
//  1. If path is itself a git directory, gitDir = path.
//  2. Else if path/.git is a directory and is itself a git
//     directory, gitDir = path/.git.
//  3. Else if path/.git is a regular file beginning with `gitdir: `,
//     gitDir is the trimmed remainder of that line. A relative path
//     is resolved against the directory containing the `.git` file
//     (the worktree). The resolved target must itself be a git
//     directory.
//  4. Otherwise resolveGitDir returns [ErrNotARepo].
//
// "Is a git directory" matches canonical Git's `is_git_directory`:
// a regular `HEAD` file, an `objects` entry, and a `refs` entry,
// where `objects` and `refs` are looked up under the resolved
// common directory (so a linked-worktree gitdir whose `commondir`
// points at the parent repo passes validation).
//
// Both returned paths are passed through [filepath.Clean].
func resolveGitDir(path string) (gitDir, commonDir string, err error) {
	gitDir, err = locateGitDir(path)
	if err != nil {
		return "", "", err
	}

	commonDir, err = resolveCommonDir(gitDir)
	if err != nil {
		return "", "", err
	}
	return gitDir, commonDir, nil
}

// locateGitDir applies the four-step resolution rule documented on
// [resolveGitDir] and returns the cleaned gitdir path.
func locateGitDir(path string) (string, error) {
	// Rule 1: path is itself the gitdir if it satisfies the full
	// `is_git_directory` signature.
	if isGitDirectory(path) {
		return filepath.Clean(path), nil
	}

	dotGit := filepath.Join(path, ".git")
	info, err := os.Stat(dotGit)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("objstore: %s: %w", path, ErrNotARepo)
		}
		return "", fmt.Errorf("objstore: stat %s: %w", dotGit, err)
	}

	// Rule 2: `.git` is a directory and itself a git directory.
	if info.IsDir() {
		if !isGitDirectory(dotGit) {
			return "", fmt.Errorf("objstore: %s: %w", dotGit, ErrNotARepo)
		}
		return filepath.Clean(dotGit), nil
	}

	// Rule 3: `.git` is a regular file. `os.Stat` follows symlinks,
	// so any non-directory mode satisfies the gitfile contract. The
	// resolved target must itself satisfy `is_git_directory` —
	// canonical Git re-runs the check on the resolved path before
	// accepting it.
	target, err := readGitfile(dotGit)
	if err != nil {
		return "", err
	}
	if !isGitDirectory(target) {
		return "", fmt.Errorf("objstore: %s: %w", target, ErrNotARepo)
	}
	return target, nil
}

// isGitDirectory reports whether path is a git repository directory
// (or a per-worktree gitdir). It mirrors canonical Git's
// `setup.c::is_git_directory` (lines 416–454 in 2.45-era Git):
//
//   - `HEAD` under path must be a regular file (or symlink to one).
//   - `objects/` and `refs/` must exist as directories. They are
//     looked up under the directory pointed at by `path/commondir`,
//     when present, so a linked-worktree gitdir whose objects and
//     refs live under the parent repo still validates.
//
// All filesystem lookups follow symlinks: canonical Git uses
// `access(..., X_OK)`, which dereferences, and Go's [os.Stat] does
// the same. [os.Lstat] is deliberately avoided.
func isGitDirectory(path string) bool {
	if info, err := os.Stat(filepath.Join(path, "HEAD")); err != nil || !info.Mode().IsRegular() {
		return false
	}

	common, err := resolveCommonDir(path)
	if err != nil {
		return false
	}
	return isDir(filepath.Join(common, "objects")) && isDir(filepath.Join(common, "refs"))
}

// isDir reports whether path resolves (after symlink dereferencing)
// to a directory.
func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// readGitfile parses a `.git` regular file and returns the resolved
// gitdir path. The pointer is rejected as [ErrNotARepo] if it does
// not start with `gitdir: ` or has an empty payload.
func readGitfile(gitfile string) (string, error) {
	raw, err := os.ReadFile(gitfile)
	if err != nil {
		return "", fmt.Errorf("objstore: read %s: %w", gitfile, err)
	}

	body, ok := strings.CutPrefix(string(raw), gitfilePrefix)
	if !ok {
		return "", fmt.Errorf("objstore: %s: missing %q prefix: %w",
			gitfile, gitfilePrefix, ErrNotARepo)
	}

	target := strings.TrimRight(body, "\r\n")
	if target == "" {
		return "", fmt.Errorf("objstore: %s: empty gitdir target: %w",
			gitfile, ErrNotARepo)
	}

	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(gitfile), target)
	}
	return filepath.Clean(target), nil
}

// resolveCommonDir reads gitDir/commondir, if present, and returns
// the resolved common directory. When the file is absent commonDir
// equals gitDir. Mirrors `setup.c::get_common_dir_noenv`: an
// absolute payload is taken verbatim, a relative payload joins
// against gitDir.
func resolveCommonDir(gitDir string) (string, error) {
	commonFile := filepath.Join(gitDir, "commondir")
	raw, err := os.ReadFile(commonFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return gitDir, nil
		}
		return "", fmt.Errorf("objstore: read %s: %w", commonFile, err)
	}

	target := strings.TrimRight(string(raw), "\r\n")
	if target == "" {
		// Treat an empty commondir as absent. Canonical Git's
		// `setup.c::get_common_dir_noenv` reads the file with
		// `strbuf_read_file(...) <= 0` and dies on a zero-byte
		// payload, but the resolver's contract is to surface a
		// usable directory whenever it can — and in this branch the
		// gitdir itself is the only sensible fallback. This is a
		// deliberate v0 design choice: more permissive than
		// canonical, never less correct (a worktree without an
		// indirection is just a non-linked worktree). Revisit if v0
		// ever needs strict canonical-Git behavior, e.g. for an
		// `fsck`-equivalent integrity path.
		return gitDir, nil
	}

	if !filepath.IsAbs(target) {
		target = filepath.Join(gitDir, target)
	}
	return filepath.Clean(target), nil
}

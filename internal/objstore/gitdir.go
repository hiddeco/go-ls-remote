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
//  1. If path/HEAD exists, gitDir = path.
//  2. Else if path/.git is a directory, gitDir = path/.git.
//  3. Else if path/.git is a regular file beginning with `gitdir: `,
//     gitDir is the trimmed remainder of that line. A relative path
//     is resolved against the directory containing the `.git` file
//     (the worktree).
//  4. Otherwise resolveGitDir returns [ErrNotARepo].
//
// Both returned paths are passed through [filepath.Clean]. The
// function does not validate that gitDir or commonDir actually
// contains a usable repository; that is a higher-layer concern.
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
	// Rule 1: path itself is the gitdir if it has a `HEAD` file.
	if ok, err := fileExists(filepath.Join(path, "HEAD")); err != nil {
		return "", err
	} else if ok {
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

	// Rule 2: `.git` is a directory.
	if info.IsDir() {
		return filepath.Clean(dotGit), nil
	}

	// Rule 3: `.git` is a regular file. `os.Stat` follows symlinks,
	// so any non-directory mode satisfies the gitfile contract.
	return readGitfile(dotGit)
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
		// Treat an empty commondir as absent. Canonical Git would
		// die here, but the resolver's contract is to surface a
		// usable directory whenever it can — and in this branch the
		// gitdir itself is the only sensible fallback.
		return gitDir, nil
	}

	if !filepath.IsAbs(target) {
		target = filepath.Join(gitDir, target)
	}
	return filepath.Clean(target), nil
}

// fileExists reports whether path exists as anything other than a
// permission/IO failure. It distinguishes a missing entry (returns
// false, nil) from a hard error (returns false, err) so callers can
// fall through to the next discovery step on absence and bubble up
// real failures.
func fileExists(path string) (bool, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

#!/usr/bin/env bash
#
# Regenerate the repository-shape fixtures under `testdata/repos/`.
#
# This script is developer-invoked; tests never shell out. Run it once
# whenever the fixture set needs to change, then commit both the script
# and the regenerated artifacts.
#
# Each fixture is a self-contained directory tree designed to exercise
# one shape of the gitdir / common-dir resolution rules implemented in
# `internal/objstore/gitdir.go`. The trees are deliberately minimal:
# enough for the resolver to traverse the indirection correctly, and
# nothing else. No commits, refs, or objects are needed because the
# resolver does not validate that the destination is a real repo.
#
# # Why `dotgit` instead of `.git`
#
# Canonical `git` refuses to track any path component literally named
# `.git` (see `is_dotgit_path()` in `path.c`), so committed fixtures
# cannot ship `.git` files or directories. Each fixture therefore
# stores its `.git` artifacts under the name `dotgit`. The Go test
# helper `materializeFixture` copies a fixture tree into `t.TempDir()`
# and renames every `dotgit` back to `.git` on the way through, so
# tests see the on-disk shape canonical Git would produce.
#
# Where canonical Git would write absolute paths (notably
# `git worktree add`, which embeds the host filesystem path into the
# linked worktree's `.git` file and into the parent repo's
# `.git/worktrees/<name>/gitdir` pointer), this script writes its own
# relative-path payloads so the committed fixtures are portable.
#
# Generated artifacts (relative to repository root):
#
#   testdata/repos/worktree-as-file/
#       linked/dotgit                regular file: `gitdir: ../main/dotgit/worktrees/linked`
#       main/dotgit/HEAD             trivial parent gitdir
#       main/dotgit/worktrees/linked/HEAD
#       main/dotgit/worktrees/linked/commondir   `../..`
#       main/dotgit/worktrees/linked/gitdir      `../../../linked/.git`
#
#   testdata/repos/submodule-as-file/
#       parent/dotgit/HEAD                trivial parent gitdir
#       parent/dotgit/modules/sub/HEAD    trivial submodule gitdir
#       parent/sub/dotgit                 regular file: `gitdir: ../.git/modules/sub`
#
#   testdata/repos/worktree-with-commondir/
#       repo/dotgit/HEAD                  trivial parent gitdir (`commonDir`)
#       repo/dotgit/worktrees/wt/HEAD     the linked-worktree gitdir tested directly
#       repo/dotgit/worktrees/wt/commondir   `../..`
#
# The `gitdir:` payloads inside `dotgit` files reference `.git` (the
# post-materialization name) rather than `dotgit`, because that is
# what the resolver under test will see on disk after
# `materializeFixture` renames the fixture into `t.TempDir()`.
#
# The submodule fixture is hand-crafted rather than generated via
# `git submodule add` because that command requires network or
# `protocol.file.allow=always` and produces extra files (`.gitmodules`,
# index entries, a real commit) that are noise for the resolver test.
# The committed `dotgit` payload is identical to what canonical Git
# writes (`gitdir: ../.git/modules/<name>`) once the fixture is
# materialized.
#
# Tested with: git version 2.45+ (no version-specific behaviour relied
# upon; the script writes its own content rather than trusting `git`'s).
#
# Usage:
#   testdata/_gen/repos.sh
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/../.." && pwd)"
out="$root/testdata/repos"
rm -rf "$out"
mkdir -p "$out"

# write_head <dir>
#   Drop a minimal `HEAD` file into <dir>. Marks the directory as a
#   git directory for the resolver's HEAD-presence check (rule 1) and
#   gives `dotgit/worktrees/<name>/` the same shape as canonical Git
#   produces. The contents are not parsed by the resolver, but use a
#   realistic value so the fixture reads as a plausible repo to anyone
#   inspecting it by hand.
write_head() {
    local dir="$1"
    mkdir -p "$dir"
    printf 'ref: refs/heads/main\n' >"$dir/HEAD"
}

# --- worktree-as-file --------------------------------------------------------
# Shape: a working tree (`linked/`) whose `.git` is a regular file
# pointing at a sibling repo's `.git/worktrees/<name>/` gitdir. The
# linked-worktree gitdir then carries a `commondir` file pointing back
# at the parent repo's `.git/`.
#
# All paths inside the fixture are relative so the tree is portable.
wt_root="$out/worktree-as-file"
mkdir -p "$wt_root/main/dotgit" "$wt_root/linked"
write_head "$wt_root/main/dotgit"
mkdir -p "$wt_root/main/dotgit/worktrees/linked"
write_head "$wt_root/main/dotgit/worktrees/linked"
# `commondir` is interpreted relative to the gitdir
# (`main/.git/worktrees/linked/` post-materialization), so `../..`
# resolves to `main/.git/`.
printf '../..\n' >"$wt_root/main/dotgit/worktrees/linked/commondir"
# `gitdir` is the back-pointer that canonical Git uses to detect a
# stale linked worktree; the resolver does not consume it, but it is
# included so the fixture matches the on-disk shape `git worktree add`
# would produce. The path is relative to the file's own directory.
printf '../../../linked/.git\n' >"$wt_root/main/dotgit/worktrees/linked/gitdir"
# The linked worktree's `.git` file. Relative to `linked/`, the target
# is `../main/.git/worktrees/linked` after materialization.
printf 'gitdir: ../main/.git/worktrees/linked\n' >"$wt_root/linked/dotgit"

# --- submodule-as-file -------------------------------------------------------
# Shape: a parent repo with one submodule (`sub/`). The submodule's
# working tree carries a regular `.git` file pointing at
# `../.git/modules/sub`, exactly as canonical Git's `submodule add`
# writes it. No `commondir` is present because submodule gitdirs are
# self-contained.
sm_root="$out/submodule-as-file"
mkdir -p "$sm_root/parent/dotgit/modules/sub" "$sm_root/parent/sub"
write_head "$sm_root/parent/dotgit"
write_head "$sm_root/parent/dotgit/modules/sub"
printf 'gitdir: ../.git/modules/sub\n' >"$sm_root/parent/sub/dotgit"

# --- worktree-with-commondir -------------------------------------------------
# Shape: a parent repo (`repo/.git/`) plus one linked-worktree gitdir
# (`repo/.git/worktrees/wt/`). The test opens the linked-worktree
# gitdir directly (not the working tree) and asserts that `commonDir`
# resolves to the parent gitdir.
cd_root="$out/worktree-with-commondir"
mkdir -p "$cd_root/repo/dotgit/worktrees/wt"
write_head "$cd_root/repo/dotgit"
write_head "$cd_root/repo/dotgit/worktrees/wt"
printf '../..\n' >"$cd_root/repo/dotgit/worktrees/wt/commondir"

echo "wrote fixtures into $out"

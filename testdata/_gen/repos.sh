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
#   testdata/repos/empty/
#       dotgit/HEAD                       sha1+files defaults; no commits
#       dotgit/objects/.gitkeep           preserve empty dir under git
#       dotgit/objects/pack/.gitkeep      preserve empty dir under git
#       dotgit/refs/.gitkeep              preserve empty dir under git
#
#   testdata/repos/sha256/
#       dotgit/HEAD                       same shape as `empty/`
#       dotgit/config                     `[extensions] objectFormat = sha256`
#       dotgit/objects/.gitkeep
#       dotgit/objects/pack/.gitkeep
#       dotgit/refs/.gitkeep
#
#   testdata/repos/with-reftable/
#       dotgit/HEAD
#       dotgit/config                     `[extensions] refStorage = reftable`
#       dotgit/objects/.gitkeep
#       dotgit/objects/pack/.gitkeep
#       dotgit/refs/.gitkeep
#       dotgit/reftable/tables.list       empty placeholder; the opener
#                                          must succeed even when the
#                                          stack carries no entries
#
#   testdata/repos/with-reftable-content/
#       dotgit/HEAD                       symref to refs/heads/main
#                                          (canonical Git writes a HEAD
#                                          file alongside the reftable;
#                                          see the resolveGitDir contract)
#       dotgit/config                     `[extensions] refStorage = reftable`
#       dotgit/objects/.gitkeep
#       dotgit/objects/pack/.gitkeep
#       dotgit/refs/.gitkeep
#       dotgit/reftable/{0001-0001-aaaaaaaa.ref,tables.list}
#                                          one-commit reftable: HEAD →
#                                          refs/heads/main plus
#                                          refs/heads/main = <commit OID>
#
#   testdata/repos/with-reftable-unborn/
#       Same skeleton as `with-reftable-content/` but the reftable stack
#       comes from `git init --ref-format=reftable` with no commit, so
#       HEAD is bound to refs/heads/main but main itself is absent —
#       the canonical "unborn HEAD" state for reftable repos.
#
#   testdata/repos/with-reftable-detached/
#       Same skeleton as `with-reftable-content/` but the reftable stack
#       was rewritten with `git update-ref --no-deref HEAD <oid>` so HEAD
#       carries a value record (no TargetRef) — the detached-HEAD shape.
#
#   testdata/repos/with-midx/
#       dotgit/HEAD
#       dotgit/objects/pack/multi-pack-index   zero-byte placeholder so the
#                                              opener takes the midx branch;
#                                              non-empty content is asserted
#                                              by the midx-backend tests
#
#   testdata/repos/loose-only/
#       dotgit/HEAD                       symref to refs/heads/main
#       dotgit/refs/heads/main            loose OID (synthetic SHA-1 hex)
#       dotgit/refs/heads/feature/x       loose OID under a subdirectory
#       dotgit/refs/tags/v1               loose OID
#       (no packed-refs)
#
#   testdata/repos/packed-only/
#       dotgit/HEAD                       symref to refs/heads/main
#       dotgit/refs/.gitkeep              empty refs directory
#       dotgit/packed-refs                header `# pack-refs with: peeled
#                                         fully-peeled` plus refs and a
#                                         `^peel` line for the tag
#
#   testdata/repos/mixed/
#       dotgit/HEAD
#       dotgit/refs/heads/main            loose OID-C — shadows the
#                                         packed `refs/heads/main = OID-A`
#       dotgit/packed-refs                refs/heads/main = OID-A and
#                                         refs/heads/old = OID-B; the
#                                         loose entry overrides the first
#
#   testdata/repos/unborn-head/
#       dotgit/HEAD                       symref to refs/heads/main
#       dotgit/refs/.gitkeep              empty: refs/heads/main missing
#                                         (no loose, no packed entry)
#
#   testdata/repos/detached-head/
#       dotgit/HEAD                       raw 40-char SHA-1 hex
#       dotgit/refs/.gitkeep              empty
#
#   testdata/repos/packed-refs-fully-peeled/
#       dotgit/HEAD
#       dotgit/refs/.gitkeep
#       dotgit/packed-refs                `# pack-refs with: peeled
#                                         fully-peeled` plus a tag with
#                                         a `^peel` line
#
#   testdata/repos/packed-refs-no-traits/
#       dotgit/HEAD
#       dotgit/refs/.gitkeep
#       dotgit/packed-refs                no header line; ref entries
#                                         from the first byte
#
#   testdata/repos/packed-refs-sorted/
#       dotgit/HEAD
#       dotgit/refs/.gitkeep
#       dotgit/packed-refs                `# pack-refs with: sorted`
#                                         plus a couple of refs in order
#
#   testdata/repos/loose-objects/
#       dotgit/HEAD
#       dotgit/refs/.gitkeep
#       dotgit/objects/pack/.gitkeep
#       dotgit/objects/39/3a7c05257a543bc1369537c7fdb2851dc04b11
#                                          blob `hello loose object world\n`
#       dotgit/objects/4c/b61db1e9094ba0e955298fcbd038ec69bc7a38
#                                          tree (single entry `blob.txt` -> blob)
#       dotgit/objects/9a/1288dcf7ead9936f178d8dd8a1f14c81eafbf9
#                                          commit (tree above, one author)
#       dotgit/objects/85/5c1386ff144601eb847df1b4e59057ca415883
#                                          annotated tag `v1` -> commit
#
#       The four bytes-on-disk are produced by canonical Git's
#       `hash-object -w`, `commit`, and `tag -a` pipelines under pinned
#       `GIT_AUTHOR_DATE` / `GIT_COMMITTER_DATE`, a fixed identity, and
#       `commit.gpgsign=false` / `tag.gpgsign=false` so neither object
#       carries a signature. The OIDs are stable across regenerations
#       and supported zlib implementations (Git uses the default
#       compression level and the loose-object framing is not version-
#       dependent). Tests reference these hex values directly.
#
#   testdata/repos/loose-objects-sha256/
#       dotgit/HEAD
#       dotgit/config                     `[extensions] objectFormat = sha256`
#       dotgit/refs/.gitkeep
#       dotgit/objects/pack/.gitkeep
#       dotgit/objects/c6/0061d62336c6b760e2c4ec860873a193c61662e4f2a6aa5cb3cbaf9339cd10
#                                          blob `hello loose object world\n`
#       dotgit/objects/e2/60f0e971c7745ca923fc46c3ea01378efc0a68b0e6f73dc30ecaf9e9ffa546
#                                          tree (single entry `blob.txt` -> blob)
#       dotgit/objects/92/d2fbd767b5d4ce56ba1dcbc710860b5255f42259c9c7f3fe0c33895545a1d3
#                                          commit
#       dotgit/objects/fa/1eca2ffe8355c2de5fafcb6da9f5e768e0bf14713a5bfa8b4f5e2ec215dc6c
#                                          annotated tag `v1` -> commit
#
#       Same shape as `loose-objects/` but with `--object-format=sha256`
#       so the fanout is taken from a 64-char hex OID.
#
#   testdata/repos/idx-single/
#       dotgit/HEAD
#       dotgit/refs/.gitkeep
#       dotgit/objects/pack/three-objects.{idx,pack}
#                                          single canonical pack/idx pair
#                                          copied verbatim from
#                                          `testdata/objfmt/`. Exercises
#                                          the `idxCatalog` happy path.
#
#   testdata/repos/idx-multi/
#       dotgit/HEAD
#       dotgit/refs/.gitkeep
#       dotgit/objects/pack/{ofs-delta,three-objects}.{idx,pack}
#                                          two pack/idx pairs so iteration
#                                          order matters; the second-pack
#                                          hit case asserts that the
#                                          backend visits packs in
#                                          basename-sorted order.
#
#   testdata/repos/idx-corrupt/
#       dotgit/HEAD
#       dotgit/refs/.gitkeep
#       dotgit/objects/pack/bogus.idx     fixed garbage payload (16 bytes
#                                          of zeros) so the opener exercises
#                                          the corrupt-idx path. Synthetic
#                                          rather than truncated-real to
#                                          avoid accidentally producing a
#                                          file that happens to parse.
#
#   testdata/repos/idx-missing-pack/
#       dotgit/HEAD
#       dotgit/refs/.gitkeep
#       dotgit/objects/pack/three-objects.idx   no `.pack` sibling; the
#                                          opener must surface an error
#                                          referencing both paths and not
#                                          leak the already-opened idx.
#
# Empty directories cannot be tracked by git, so each fixture that
# needs to ship one carries a zero-byte `.gitkeep` placeholder. The
# materializer copies it through unchanged; the resolver and backends
# ignore it.
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

# Make this script hermetic against the developer's personal Git
# configuration. Without this guard a global `commit.gpgsign=true`,
# `tag.gpgsign=true`, or `tag.forceSignAnnotated=true` would invoke the
# user's signing key during fixture generation — which both prompts
# for a hardware-token touch on every run AND embeds non-portable
# bytes (signatures over fixture commits and tags differ between
# generators, breaking byte-stable fixture review). The per-work-repo
# `git config commit.gpgsign=false` / `tag.gpgsign=false` calls below
# are belt-and-suspenders against the same hazard. If a future fixture
# legitimately requires a signed Git object, generate it with a
# checked-in deterministic test key (PGP or SSH) under this same env
# isolation, never the developer's personal identity.
export GIT_CONFIG_GLOBAL=/dev/null
export GIT_CONFIG_SYSTEM=/dev/null
export GIT_CONFIG_NOSYSTEM=1
export GIT_TEMPLATE_DIR=
unset GIT_AUTHOR_NAME GIT_AUTHOR_EMAIL GIT_COMMITTER_NAME GIT_COMMITTER_EMAIL

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

# scaffold_minimal_repo <root>
#   Lay down the canonical empty-repo skeleton: a `dotgit/HEAD` file
#   plus the empty `objects/`, `objects/pack/`, and `refs/` directories
#   each guarded by a `.gitkeep`. Used as the starting point for the
#   `Open`-orchestration fixtures below.
scaffold_minimal_repo() {
    local root="$1"
    mkdir -p "$root/dotgit/objects/pack" "$root/dotgit/refs"
    write_head "$root/dotgit"
    : >"$root/dotgit/objects/.gitkeep"
    : >"$root/dotgit/objects/pack/.gitkeep"
    : >"$root/dotgit/refs/.gitkeep"
}

# --- empty -------------------------------------------------------------------
# Shape: a brand-new sha1+files repository with no commits, no packs,
# no `packed-refs`. The opener must succeed and report `objfmt.SHA1`;
# `IterRefs` must yield nothing once the loose-refs backend lands.
scaffold_minimal_repo "$out/empty"

# --- sha256 ------------------------------------------------------------------
# Shape: identical to `empty/` but flips `extensions.objectFormat` to
# `sha256`. Exercises the algo plumbing through the opener.
sha256_root="$out/sha256"
scaffold_minimal_repo "$sha256_root"
cat >"$sha256_root/dotgit/config" <<'EOF'
[core]
	repositoryformatversion = 1
[extensions]
	objectFormat = sha256
EOF

# --- with-reftable -----------------------------------------------------------
# Shape: same skeleton as `empty/` plus an empty `reftable/` directory
# (the canonical Git location is `<commonDir>/reftable/tables.list` —
# we ship an empty placeholder so the directory survives `git add` and
# the opener can take the reftable branch). Populated reftable content
# lives in the `with-reftable-*` siblings below.
rt_root="$out/with-reftable"
scaffold_minimal_repo "$rt_root"
mkdir -p "$rt_root/dotgit/reftable"
: >"$rt_root/dotgit/reftable/tables.list"
cat >"$rt_root/dotgit/config" <<'EOF'
[core]
	repositoryformatversion = 1
[extensions]
	refStorage = reftable
EOF

# scaffold_reftable_fixture <fixture-root> <work-repo>
#   Lay down the canonical reftable-backed repo skeleton — HEAD, config,
#   empty objects/pack/refs placeholders — then copy the work-repo's
#   `.git/reftable/` payload into `dotgit/reftable/` under stable
#   basenames so the on-disk bytes are identical across regenerations.
#
#   The work repo is created via `git init --ref-format=reftable` plus
#   any caller-supplied ref operations; this helper only consumes the
#   resulting reftable directory. Stable basenames mirror the convention
#   used by `reftable.sh::rename_reftable_dir`: `NNNN-NNNN-<suffix>.ref`
#   with positional pseudo-rand suffixes (`aaaaaaaa`, `bbbbbbbb`, ...).
scaffold_reftable_fixture() {
    local root="$1"
    local work="$2"
    mkdir -p "$root/dotgit/objects/pack" "$root/dotgit/refs" "$root/dotgit/reftable"
    write_head "$root/dotgit"
    : >"$root/dotgit/objects/.gitkeep"
    : >"$root/dotgit/objects/pack/.gitkeep"
    : >"$root/dotgit/refs/.gitkeep"
    cat >"$root/dotgit/config" <<'EOF'
[core]
	repositoryformatversion = 1
[extensions]
	refStorage = reftable
EOF
    : >"$root/dotgit/reftable/tables.list"
    local idx=1
    local suffixes=("aaaaaaaa" "bbbbbbbb" "cccccccc" "dddddddd" "eeeeeeee" "ffffffff" "12345678" "9abcdef0")
    while IFS= read -r line; do
        [ -z "$line" ] && continue
        if [ "$idx" -gt "${#suffixes[@]}" ]; then
            echo "scaffold_reftable_fixture: too many tables ($idx) for known suffixes" >&2
            exit 1
        fi
        local stable
        stable="$(printf '%04d-%04d-%s.ref' "$idx" "$idx" "${suffixes[$((idx - 1))]}")"
        cp "$work/.git/reftable/$line" "$root/dotgit/reftable/$stable"
        printf '%s\n' "$stable" >>"$root/dotgit/reftable/tables.list"
        idx=$((idx + 1))
    done <"$work/.git/reftable/tables.list"
}

# Reftable record bytes embed the committer's wall-clock time (log
# records carry `time_seconds`). Pin both dates so subsequent commits
# produce identical `.ref` files across regenerations.
export GIT_AUTHOR_DATE='2020-01-02T03:04:05+00:00'
export GIT_COMMITTER_DATE='2020-01-02T03:04:05+00:00'

# Shared scratch root for the reftable work-repos. A single trap covers
# everything underneath; the per-fixture work directories live in here.
rt_work_root="$(mktemp -d)"
trap 'rm -rf "'"$rt_work_root"'"' EXIT

# init_reftable_work_repo <work-dir>
#   `git init --ref-format=reftable` plus the deterministic identity /
#   gpgsign config the fixture commits need. Caller is responsible for
#   committing or otherwise mutating refs.
init_reftable_work_repo() {
    local work="$1"
    git -c init.defaultBranch=main \
        -c extensions.refStorage=reftable \
        init -q --ref-format=reftable --object-format=sha1 "$work"
    git -C "$work" config user.email fixtures@example.invalid
    git -C "$work" config user.name  fixtures
    git -C "$work" config commit.gpgsign false
    git -C "$work" config tag.gpgsign    false
    git -C "$work" config tag.forceSignAnnotated false
}

# --- with-reftable-content ---------------------------------------------------
# A one-commit reftable-backed repo. The reftable stack carries HEAD as
# a symref to refs/heads/main plus refs/heads/main as a value record.
# Used by the reftable-backend tests for IterRefs and the
# symref-to-existing-target HEAD case.
rtc_work="$rt_work_root/with-reftable-content"
init_reftable_work_repo "$rtc_work"
(
    cd "$rtc_work"
    printf 'reftable content\n' >payload.txt
    git add payload.txt
    git commit -q -m "fixture: with-reftable-content"
)
scaffold_reftable_fixture "$out/with-reftable-content" "$rtc_work"

# --- with-reftable-unborn ----------------------------------------------------
# A `git init --ref-format=reftable` repo with no commit. The reftable
# stack carries only HEAD (a symref to refs/heads/main) — main itself
# is absent. Exercises the reftable-backend "unborn HEAD" path where
# FindRef on the symref target misses.
rtu_work="$rt_work_root/with-reftable-unborn"
init_reftable_work_repo "$rtu_work"
scaffold_reftable_fixture "$out/with-reftable-unborn" "$rtu_work"

# --- with-reftable-detached --------------------------------------------------
# A one-commit reftable-backed repo whose HEAD has been re-bound to the
# commit OID with `git update-ref --no-deref HEAD <oid>`. The resulting
# reftable carries HEAD as a value record (no TargetRef), which the
# backend must report as Symref="" with the OID populated.
rtd_work="$rt_work_root/with-reftable-detached"
init_reftable_work_repo "$rtd_work"
(
    cd "$rtd_work"
    printf 'reftable detached\n' >payload.txt
    git add payload.txt
    git commit -q -m "fixture: with-reftable-detached"
    head_oid=$(git rev-parse HEAD)
    git update-ref --no-deref HEAD "$head_oid"
)
scaffold_reftable_fixture "$out/with-reftable-detached" "$rtd_work"

# --- with-midx ---------------------------------------------------------------
# Shape: a HEAD plus an empty pack directory carrying a zero-byte
# `multi-pack-index` placeholder. The opener uses the file's presence
# (not its contents) to choose the midx pack backend over the loose
# `.idx` catalogue. Real midx bodies are exercised by the midx-backend
# tests and live alongside their pack/idx fixtures.
midx_root="$out/with-midx"
mkdir -p "$midx_root/dotgit/objects/pack" "$midx_root/dotgit/refs"
write_head "$midx_root/dotgit"
: >"$midx_root/dotgit/objects/pack/multi-pack-index"
: >"$midx_root/dotgit/refs/.gitkeep"

# --- loose-refs / packed-refs fixtures --------------------------------------
# The loose-refs backend reads `<commonDir>/refs/...` and the optional
# `<commonDir>/packed-refs` file. The fixtures below exercise the backend
# in isolation: synthetic hex OIDs (deterministic, never real objects)
# avoid pulling pack/loose-object machinery into ref tests, and stand-in
# `dotgit/refs/.gitkeep` placeholders preserve the empty `refs/`
# directory that canonical Git always materializes.

# Synthetic SHA-1 OIDs used across the loose/packed fixtures. Distinct
# values per role keep the assertions unambiguous when a packed ref is
# shadowed by a loose ref.
oid_main='aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
oid_feature='bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb'
oid_tag='cccccccccccccccccccccccccccccccccccccccc'
oid_tag_peel='dddddddddddddddddddddddddddddddddddddddd'
oid_packed_main='1111111111111111111111111111111111111111'
oid_packed_old='2222222222222222222222222222222222222222'
oid_loose_main='3333333333333333333333333333333333333333'
oid_detached='4444444444444444444444444444444444444444'

# --- loose-only --------------------------------------------------------------
# Loose refs spanning a subdirectory (`refs/heads/feature/x`) and a tag
# under `refs/tags/`. No `packed-refs`. HEAD is a symref to a ref that
# exists, so the resolver picks up the loose OID.
lo_root="$out/loose-only"
mkdir -p "$lo_root/dotgit/refs/heads/feature" "$lo_root/dotgit/refs/tags"
write_head "$lo_root/dotgit"
printf '%s\n' "$oid_main"    >"$lo_root/dotgit/refs/heads/main"
printf '%s\n' "$oid_feature" >"$lo_root/dotgit/refs/heads/feature/x"
printf '%s\n' "$oid_tag"     >"$lo_root/dotgit/refs/tags/v1"

# --- packed-only -------------------------------------------------------------
# Only `packed-refs` carries entries; `refs/` is empty. The header
# advertises `peeled fully-peeled` so the parser sets both traits, and a
# `^peel` line follows the tag entry to exercise the peel-known path.
po_root="$out/packed-only"
mkdir -p "$po_root/dotgit/refs"
write_head "$po_root/dotgit"
: >"$po_root/dotgit/refs/.gitkeep"
{
    printf '# pack-refs with: peeled fully-peeled\n'
    printf '%s refs/heads/main\n' "$oid_main"
    printf '%s refs/tags/v1\n'    "$oid_tag"
    printf '^%s\n'                "$oid_tag_peel"
} >"$po_root/dotgit/packed-refs"

# --- mixed -------------------------------------------------------------------
# Loose `refs/heads/main` shadows the packed entry of the same name; the
# packed-only `refs/heads/old` still surfaces. Confirms loose-overrides-
# packed precedence and that orphaned packed entries are not dropped.
mx_root="$out/mixed"
mkdir -p "$mx_root/dotgit/refs/heads"
write_head "$mx_root/dotgit"
printf '%s\n' "$oid_loose_main" >"$mx_root/dotgit/refs/heads/main"
{
    printf '# pack-refs with: peeled fully-peeled sorted\n'
    printf '%s refs/heads/main\n' "$oid_packed_main"
    printf '%s refs/heads/old\n'  "$oid_packed_old"
} >"$mx_root/dotgit/packed-refs"

# --- unborn-head -------------------------------------------------------------
# HEAD points at refs/heads/main but no such ref exists either loose or
# packed: canonical "unborn" state. The resolver must report Symref set,
# OID zero, Unborn true.
ub_root="$out/unborn-head"
mkdir -p "$ub_root/dotgit/refs"
write_head "$ub_root/dotgit"
: >"$ub_root/dotgit/refs/.gitkeep"

# --- detached-head -----------------------------------------------------------
# HEAD is a raw 40-char SHA-1 hex. Detached HEAD: Symref empty, OID set,
# Unborn false.
dh_root="$out/detached-head"
mkdir -p "$dh_root/dotgit/refs"
printf '%s\n' "$oid_detached" >"$dh_root/dotgit/HEAD"
: >"$dh_root/dotgit/refs/.gitkeep"

# --- packed-refs-fully-peeled ------------------------------------------------
# Minimal repo carrying `peeled fully-peeled` traits and a single tag
# with a `^peel` line. Pairs with packed-refs-no-traits and
# packed-refs-sorted to cover the trait-parser branches.
fp_root="$out/packed-refs-fully-peeled"
mkdir -p "$fp_root/dotgit/refs"
write_head "$fp_root/dotgit"
: >"$fp_root/dotgit/refs/.gitkeep"
{
    printf '# pack-refs with: peeled fully-peeled\n'
    printf '%s refs/heads/main\n' "$oid_main"
    printf '%s refs/tags/v1\n'    "$oid_tag"
    printf '^%s\n'                "$oid_tag_peel"
} >"$fp_root/dotgit/packed-refs"

# --- packed-refs-no-traits ---------------------------------------------------
# `packed-refs` body without the `# pack-refs with:` header line. All
# trait flags must remain false.
nt_root="$out/packed-refs-no-traits"
mkdir -p "$nt_root/dotgit/refs"
write_head "$nt_root/dotgit"
: >"$nt_root/dotgit/refs/.gitkeep"
{
    printf '%s refs/heads/main\n' "$oid_main"
    printf '%s refs/tags/v1\n'    "$oid_tag"
} >"$nt_root/dotgit/packed-refs"

# --- packed-refs-sorted ------------------------------------------------------
# `# pack-refs with: sorted` header only: just the sorted trait flips.
sr_root="$out/packed-refs-sorted"
mkdir -p "$sr_root/dotgit/refs"
write_head "$sr_root/dotgit"
: >"$sr_root/dotgit/refs/.gitkeep"
{
    printf '# pack-refs with: sorted\n'
    printf '%s refs/heads/main\n' "$oid_main"
    printf '%s refs/tags/v1\n'    "$oid_tag"
} >"$sr_root/dotgit/packed-refs"

# --- loose-objects ----------------------------------------------------------
# Real loose objects covering all four [objfmt.ObjectType] variants
# (commit, tree, blob, tag). Generated by `git hash-object -w` and
# friends in a scratch repo, then copied into the fixture under their
# `aa/rest` fanout directories. The pinned identity / dates above keep
# the commit and tag OIDs deterministic across regenerations.
#
# The file bytes are zlib-compressed `<type> <size>\0<payload>` blobs
# produced by canonical Git; supported zlib implementations all encode
# them identically because Git uses the default compression level and
# the loose-object format is not framed with version-dependent options.
lo_work="$(mktemp -d)"
trap 'rm -rf "'"$rt_work_root"'" "'"$lo_work"'"' EXIT
(
    cd "$lo_work"
    git -c init.defaultBranch=main init -q --object-format=sha1
    git config user.email fixtures@example.invalid
    git config user.name  fixtures
    git config commit.gpgsign false
    git config tag.gpgsign    false
    git config tag.forceSignAnnotated false
    printf 'hello loose object world\n' >blob.txt
    git add blob.txt
    git commit -q -m "fixture: loose objects"
    git tag -a -m "fixture annotated tag" v1 HEAD
)
loobj_root="$out/loose-objects"
mkdir -p "$loobj_root/dotgit/objects/pack" "$loobj_root/dotgit/refs"
write_head "$loobj_root/dotgit"
: >"$loobj_root/dotgit/objects/pack/.gitkeep"
: >"$loobj_root/dotgit/refs/.gitkeep"
# Copy every loose object the work-repo materialized. Each lives under
# `objects/<aa>/<rest>` already, so a recursive copy of `objects/*/`
# preserves the canonical layout.
for fanout in "$lo_work"/.git/objects/*/; do
    name=$(basename "$fanout")
    # Skip canonical sibling directories (`info`, `pack`).
    case "$name" in
        info|pack) continue ;;
    esac
    mkdir -p "$loobj_root/dotgit/objects/$name"
    cp "$fanout"/* "$loobj_root/dotgit/objects/$name/"
done

# --- loose-objects-sha256 ---------------------------------------------------
# Same shape as `loose-objects/` but with `--object-format=sha256`. The
# 64-char fanout exercises the algo-aware path-construction code in the
# loose-objects backend.
lo256_work="$(mktemp -d)"
trap 'rm -rf "'"$rt_work_root"'" "'"$lo_work"'" "'"$lo256_work"'"' EXIT
(
    cd "$lo256_work"
    git -c init.defaultBranch=main init -q --object-format=sha256
    git config user.email fixtures@example.invalid
    git config user.name  fixtures
    git config commit.gpgsign false
    git config tag.gpgsign    false
    git config tag.forceSignAnnotated false
    printf 'hello loose object world\n' >blob.txt
    git add blob.txt
    git commit -q -m "fixture: loose objects sha256"
    git tag -a -m "fixture annotated tag sha256" v1 HEAD
)
loobj256_root="$out/loose-objects-sha256"
mkdir -p "$loobj256_root/dotgit/objects/pack" "$loobj256_root/dotgit/refs"
write_head "$loobj256_root/dotgit"
: >"$loobj256_root/dotgit/objects/pack/.gitkeep"
: >"$loobj256_root/dotgit/refs/.gitkeep"
cat >"$loobj256_root/dotgit/config" <<'EOF'
[core]
	repositoryformatversion = 1
[extensions]
	objectFormat = sha256
EOF
for fanout in "$lo256_work"/.git/objects/*/; do
    name=$(basename "$fanout")
    case "$name" in
        info|pack) continue ;;
    esac
    mkdir -p "$loobj256_root/dotgit/objects/$name"
    cp "$fanout"/* "$loobj256_root/dotgit/objects/$name/"
done

# --- idx catalog fixtures ---------------------------------------------------
# Pack/idx fixtures for the per-`.idx` catalogue backend. These reuse
# the canonical bytes already shipped under `testdata/objfmt/`, so no
# `git` invocation is required: each fixture is a `cp` of pre-built
# pack/idx pairs into a minimal repo skeleton.

# scaffold_idx_repo <root>
#   Lay down a minimal repo skeleton with an empty `objects/pack/`
#   directory ready to receive committed pack/idx fixtures.
scaffold_idx_repo() {
    local root="$1"
    mkdir -p "$root/dotgit/objects/pack" "$root/dotgit/refs"
    write_head "$root/dotgit"
    : >"$root/dotgit/refs/.gitkeep"
}

# --- idx-single --------------------------------------------------------------
# One pack/idx pair under `objects/pack/`. Used to assert the happy
# path: a known OID resolves to the correct (`*Pack`, offset) tuple.
idx_single_root="$out/idx-single"
scaffold_idx_repo "$idx_single_root"
cp "$root/testdata/objfmt/three-objects.idx" \
    "$idx_single_root/dotgit/objects/pack/three-objects.idx"
cp "$root/testdata/objfmt/three-objects.pack" \
    "$idx_single_root/dotgit/objects/pack/three-objects.pack"

# --- idx-multi ---------------------------------------------------------------
# Two pack/idx pairs so the iteration-order assertions have something
# to bite on. `ofs-delta` sorts before `three-objects` lexically, which
# makes "OID lives in second pack" exercise the post-first-miss path.
idx_multi_root="$out/idx-multi"
scaffold_idx_repo "$idx_multi_root"
cp "$root/testdata/objfmt/three-objects.idx" \
    "$idx_multi_root/dotgit/objects/pack/three-objects.idx"
cp "$root/testdata/objfmt/three-objects.pack" \
    "$idx_multi_root/dotgit/objects/pack/three-objects.pack"
cp "$root/testdata/objfmt/ofs-delta.idx" \
    "$idx_multi_root/dotgit/objects/pack/ofs-delta.idx"
cp "$root/testdata/objfmt/ofs-delta.pack" \
    "$idx_multi_root/dotgit/objects/pack/ofs-delta.pack"

# --- idx-corrupt -------------------------------------------------------------
# An idx file whose first bytes are neither the v2 magic nor a
# plausible v1 fan-out. Sixteen zero bytes are short enough to trip the
# v1 truncation check (`v1` requires `256*4` bytes for the fan-out
# alone) and synthetic enough that no future edit of the canonical
# fixtures could accidentally produce parsable bytes.
idx_corrupt_root="$out/idx-corrupt"
scaffold_idx_repo "$idx_corrupt_root"
# 16 NULs: classified as v1 (no v2 magic) and immediately rejected for
# truncation. Written with `dd` because `printf` cannot emit raw NULs
# portably across shells.
dd if=/dev/zero of="$idx_corrupt_root/dotgit/objects/pack/bogus.idx" \
    bs=1 count=16 status=none

# --- idx-missing-pack --------------------------------------------------------
# An idx with no `.pack` sibling. Surfaces the constructor's pairing
# check: the opener must close the already-opened idx and return an
# error mentioning both paths.
idx_missing_root="$out/idx-missing-pack"
scaffold_idx_repo "$idx_missing_root"
cp "$root/testdata/objfmt/three-objects.idx" \
    "$idx_missing_root/dotgit/objects/pack/three-objects.idx"

echo "wrote fixtures into $out"

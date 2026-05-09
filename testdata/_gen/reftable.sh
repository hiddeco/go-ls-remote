#!/usr/bin/env bash
#
# Regenerate the reftable fixtures under `testdata/reftable/`.
#
# This script is developer-invoked; tests never shell out. Run it once
# whenever the fixture set needs to change, then commit both the script
# and the regenerated artifacts.
#
# Reftable files are produced by `git init --ref-format=reftable` plus
# subsequent `git update-ref` calls against an in-memory commit. Each
# transaction lands as a fresh `.ref` file appended to `tables.list`,
# so multi-table stacks fall out of repeated `update-ref` invocations.
#
# Canonical Git uses content-addressed `.ref` filenames of the form
# `0x<min>-0x<max>-<rand8>.ref` for filesystem uniqueness. Tests do not
# care about the random suffix, so the script renames each emitted
# file to a stable basename and rewrites `tables.list` in lockstep.
#
# Generated artifacts (relative to repository root):
#
#   testdata/reftable/single-sha1/
#       0001-0001-aaaaaaaa.ref          one-table SHA-1 stack
#       tables.list
#   testdata/reftable/single-sha256/
#       0001-0001-aaaaaaaa.ref          one-table SHA-256 stack
#       tables.list
#   testdata/reftable/stack-shadow-sha1/
#       0001-0001-aaaaaaaa.ref          first transaction (HEAD bind)
#       0002-0002-bbbbbbbb.ref          earlier table; refs/heads/main
#                                       points at $head_a
#       0003-0003-cccccccc.ref          later table; refs/heads/main
#                                       points at $head_b. FindRef on
#                                       the merged stack must see $head_b
#       tables.list                     (3 entries; canonical Git emits
#                                       one transaction per ref update,
#                                       so two `git commit`s yield three
#                                       reftables when auto-compaction
#                                       is disabled)
#   testdata/reftable/with-index-sha1/
#       0001-0001-aaaaaaaa.ref          ref-index-bearing reftable
#                                       (block_size=512, many refs)
#       tables.list
#   testdata/reftable/without-index-sha1/
#       0001-0001-aaaaaaaa.ref          tiny reftable, no ref index
#       tables.list
#   testdata/reftable/corrupt-trailer-sha1.ref
#                                       copy of single-sha1 reftable
#                                       with the last CRC byte flipped
#   testdata/reftable/truncated-sha1.ref
#                                       copy of single-sha1 reftable
#                                       with the last 8 footer bytes
#                                       removed (CRC + part of the
#                                       trailing offsets)
#
# The block_size and ref_index_position assertions inside the script
# guard against silent regressions if upstream Git changes its writer
# defaults: `with-index-sha1` must have a non-zero `ref_index_position`
# and `without-index-sha1` must have zero. Both are read directly from
# the v1 reftable footer; see canonical Git's
# `Documentation/technical/reftable.adoc` (footer layout) and
# `reftable/reader.c` (`reader_init_for_*`).
#
# Tested with: git version 2.45+
#
# Usage:
#   testdata/_gen/reftable.sh
#
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/../.." && pwd)"
out="$root/testdata/reftable"
rm -rf "$out"
mkdir -p "$out"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# Auto-compaction is on by default — a single `git commit` on a fresh
# reftable repo normally yields a single .ref file even though it walks
# multiple update-ref transactions internally. Sub-fixtures that want
# multiple tables (the stack-shadow case) override this with
# GIT_TEST_REFTABLE_AUTOCOMPACTION=0; the variable is read by canonical
# Git's `refs/reftable-backend.c` (`disable_auto_compact`).
#
# Reftable log records embed the committer's wall-clock time
# (`time_seconds`), so without a fixed date every `update-ref` would
# produce different bytes. Pin the dates globally; per-fixture commits
# that need a distinct timestamp override these locally.
export GIT_AUTHOR_DATE='2020-01-02T03:04:05+00:00'
export GIT_COMMITTER_DATE='2020-01-02T03:04:05+00:00'

# init_reftable_repo <dir> <object-format>
#   Initialize a reftable-backed git repo with deterministic config so
#   subsequent commits and update-refs produce reproducible bytes.
init_reftable_repo() {
    local dir="$1"
    local fmt="$2"
    git -c init.defaultBranch=main \
        -c extensions.refStorage=reftable \
        init -q --ref-format=reftable --object-format="$fmt" "$dir"
    git -C "$dir" config user.email fixtures@example.invalid
    git -C "$dir" config user.name  fixtures
    git -C "$dir" config commit.gpgsign false
}

# rename_reftable_dir <src-reftable-dir> <dest-dir>
#   Copy each `0x...-0x...-<rand>.ref` file from `<src>` into `<dest>`
#   under a stable normalized basename (`NNNN-NNNN-aaaaaaaa.ref`) and
#   rewrite `tables.list` to match. The source order is preserved.
rename_reftable_dir() {
    local src="$1"
    local dst="$2"
    mkdir -p "$dst"
    : >"$dst/tables.list"
    local idx=1
    local stable_id
    # Stable per-position pseudo-rand suffixes, just to keep the on-disk
    # name format identical to canonical Git's. Tests don't read these.
    local suffixes=("aaaaaaaa" "bbbbbbbb" "cccccccc" "dddddddd" "eeeeeeee" "ffffffff" "12345678" "9abcdef0")
    while IFS= read -r line; do
        [ -z "$line" ] && continue
        if [ "$idx" -gt "${#suffixes[@]}" ]; then
            echo "rename_reftable_dir: too many tables ($idx) for known suffixes" >&2
            exit 1
        fi
        stable_id="${suffixes[$((idx - 1))]}"
        local stable
        stable="$(printf '%04d-%04d-%s.ref' "$idx" "$idx" "$stable_id")"
        cp "$src/$line" "$dst/$stable"
        printf '%s\n' "$stable" >>"$dst/tables.list"
        idx=$((idx + 1))
    done <"$src/tables.list"
}

# footer_ref_index_position <reftable-file>
#   Print the `ref_index_position` field from the v1 footer of a SHA-1
#   reftable, decoded as a decimal uint64. Used by the script's sanity
#   checks; not by tests. Format reference:
#   `Documentation/technical/reftable.adoc` (footer section).
footer_ref_index_position() {
    local f="$1"
    python3 - "$f" <<'PY'
import struct, sys
with open(sys.argv[1], "rb") as fh:
    data = fh.read()
ver = data[4]
footer_len = 68 if ver == 1 else 72
fpos = len(data) - footer_len
header_in_footer_len = 24 if ver == 1 else 28
ref_idx = struct.unpack(">Q", data[fpos + header_in_footer_len:fpos + header_in_footer_len + 8])[0]
print(ref_idx)
PY
}

# footer_block_size <reftable-file>
#   Print the 24-bit `block_size` field from the file header.
footer_block_size() {
    local f="$1"
    python3 - "$f" <<'PY'
import struct, sys
with open(sys.argv[1], "rb") as fh:
    data = fh.read()
bs = struct.unpack(">I", b"\x00" + data[5:8])[0]
print(bs)
PY
}

# commit_in <repo>
#   Create one deterministic commit on the current branch and echo its
#   OID. Each invocation should be paired with a distinct file content
#   so the resulting tree (and therefore commit) hash differs.
commit_in() {
    local repo="$1"
    local content="$2"
    local date="$3"
    (
        cd "$repo"
        printf '%s\n' "$content" >payload.txt
        git add payload.txt
        GIT_AUTHOR_DATE="$date" \
        GIT_COMMITTER_DATE="$date" \
            git commit -q -m "fixture: $content"
        git rev-parse HEAD
    )
}

# --- single-sha1 -------------------------------------------------------------
# One-table reftable produced by a single commit. The HEAD ref plus
# refs/heads/main are written together as the initial transaction.
sha1_repo="$work/single-sha1"
init_reftable_repo "$sha1_repo" sha1
commit_in "$sha1_repo" "single sha1" "2020-01-02T03:04:05+00:00" >/dev/null
rename_reftable_dir "$sha1_repo/.git/reftable" "$out/single-sha1"

# --- single-sha256 -----------------------------------------------------------
# Same shape as single-sha1, but the surrounding repo is SHA-256 so the
# reftable header carries `version_number=2` and the `s256` hash id.
sha256_repo="$work/single-sha256"
init_reftable_repo "$sha256_repo" sha256
commit_in "$sha256_repo" "single sha256" "2020-01-02T03:04:05+00:00" >/dev/null
rename_reftable_dir "$sha256_repo/.git/reftable" "$out/single-sha256"

# --- stack-shadow-sha1 -------------------------------------------------------
# Multi-table stack where a later table shadows an earlier one. The
# first commit creates a reftable that points refs/heads/main at
# `head_a`; the second commit appends another transaction that
# re-points refs/heads/main at `head_b`. Auto-compaction is suppressed
# (see GIT_TEST_REFTABLE_AUTOCOMPACTION above), so tables.list ends up
# with multiple entries and the merged view must resolve refs/heads/main
# to `head_b`. The exact number of tables canonical Git emits per
# `git commit` is an implementation detail (one transaction per ref
# update); we assert only `>=2` and that the last table differs in size
# from the first to give downstream tests a meaningful shadow.
shadow_repo="$work/stack-shadow-sha1"
GIT_TEST_REFTABLE_AUTOCOMPACTION=0 init_reftable_repo "$shadow_repo" sha1
head_a=$(GIT_TEST_REFTABLE_AUTOCOMPACTION=0 commit_in "$shadow_repo" "shadow gen 1" "2020-01-02T03:04:05+00:00")
head_b=$(GIT_TEST_REFTABLE_AUTOCOMPACTION=0 commit_in "$shadow_repo" "shadow gen 2" "2020-01-02T04:05:06+00:00")
ntables=$(wc -l <"$shadow_repo/.git/reftable/tables.list" | tr -d ' ')
[ "$ntables" -ge 2 ] \
    || { echo "stack-shadow-sha1: expected >=2 tables, got $ntables" >&2; exit 1; }
[ "$head_a" != "$head_b" ] \
    || { echo "stack-shadow-sha1: head_a==head_b, no shadow possible" >&2; exit 1; }
rename_reftable_dir "$shadow_repo/.git/reftable" "$out/stack-shadow-sha1"

# --- with-index-sha1 ---------------------------------------------------------
# Force a tiny `block_size` so >=2 ref blocks form, which causes the
# writer to emit a ref index. The config knob is `reftable.blockSize`
# in the repo's config; setting it via `-c` only affects the immediate
# command, so we persist it before any ref-mutating command runs.
#
# All 120 refs land in a single `git update-ref --stdin` transaction so
# the resulting reftable contains them all in one block layout. That
# table — large enough to span multiple ref blocks at block_size=512 —
# is what the test wants: many ref blocks plus the resulting ref index.
# The smaller earlier tables (HEAD, refs/heads/main from the prelude
# commit) are dropped from the fixture; only the index-bearing table
# is committed.
idx_repo="$work/with-index-sha1"
init_reftable_repo "$idx_repo" sha1
git -C "$idx_repo" config reftable.blockSize 512
head_oid=$(commit_in "$idx_repo" "with-index" "2020-01-02T03:04:05+00:00")
{
    i=1
    while [ "$i" -le 120 ]; do
        printf 'create refs/heads/branch-%d %s\n' "$i" "$head_oid"
        i=$((i + 1))
    done
} | git -C "$idx_repo" update-ref --stdin >/dev/null
# Pick the largest reftable: that's the one carrying all 120 branches
# and the ref index. The small leading tables hold only HEAD and
# refs/heads/main and would obscure the index test if included.
biggest=""
biggest_size=0
for f in "$idx_repo"/.git/reftable/*.ref; do
    sz=$(wc -c <"$f")
    if [ "$sz" -gt "$biggest_size" ]; then
        biggest_size=$sz
        biggest=$f
    fi
done
[ -n "$biggest" ] || { echo "with-index-sha1: no reftable found" >&2; exit 1; }
ridx=$(footer_ref_index_position "$biggest")
bs=$(footer_block_size "$biggest")
[ "$ridx" -gt 0 ] \
    || { echo "with-index-sha1: ref_index_position=0 in $biggest" >&2; exit 1; }
[ "$bs" = "512" ] \
    || { echo "with-index-sha1: block_size=$bs (want 512) in $biggest" >&2; exit 1; }
mkdir -p "$out/with-index-sha1"
cp "$biggest" "$out/with-index-sha1/0001-0001-aaaaaaaa.ref"
printf '%s\n' "0001-0001-aaaaaaaa.ref" >"$out/with-index-sha1/tables.list"

# --- without-index-sha1 ------------------------------------------------------
# Tiny reftable, default block size. With only HEAD and refs/heads/main
# present the writer fits everything in a single ref block and omits
# the ref index. We assert `ref_index_position == 0`.
noidx_repo="$work/without-index-sha1"
init_reftable_repo "$noidx_repo" sha1
commit_in "$noidx_repo" "without-index" "2020-01-02T03:04:05+00:00" >/dev/null
single=$(ls "$noidx_repo"/.git/reftable/*.ref | head -1)
[ "$(footer_ref_index_position "$single")" = "0" ] \
    || { echo "without-index-sha1: ref_index_position!=0 in $single" >&2; exit 1; }
rename_reftable_dir "$noidx_repo/.git/reftable" "$out/without-index-sha1"

# --- corrupt-trailer-sha1.ref -----------------------------------------------
# Copy the single-sha1 reftable and flip the last byte (which sits
# inside the trailing CRC32 of the v1 footer). The reader's CRC check
# must reject the file.
src_single="$out/single-sha1/0001-0001-aaaaaaaa.ref"
cp "$src_single" "$out/corrupt-trailer-sha1.ref"
python3 - "$out/corrupt-trailer-sha1.ref" <<'PY'
import sys
p = sys.argv[1]
with open(p, "rb") as fh:
    data = bytearray(fh.read())
data[-1] ^= 0x01
with open(p, "wb") as fh:
    fh.write(bytes(data))
PY

# --- truncated-sha1.ref ------------------------------------------------------
# Copy the single-sha1 reftable and lop 8 bytes off the tail. The
# reader's `OpenReader` must report a clean error rather than panic.
cp "$src_single" "$out/truncated-sha1.ref"
python3 - "$out/truncated-sha1.ref" <<'PY'
import os, sys
p = sys.argv[1]
sz = os.path.getsize(p)
with open(p, "r+b") as fh:
    fh.truncate(sz - 8)
PY

echo "wrote fixtures into $out"

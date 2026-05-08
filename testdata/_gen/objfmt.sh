#!/usr/bin/env bash
#
# Regenerate the objfmt pack/idx fixtures under `testdata/objfmt/`.
#
# This script is developer-invoked; tests never shell out. Run it once
# whenever the fixture set needs to change, then commit both the script
# and the regenerated artifacts.
#
# Each pack lands with a sidecar `<name>.offsets.txt` derived from
# `git verify-pack -v` so tests can assert against known offsets and
# sizes without re-parsing the index at runtime.
#
# Generated artifacts (relative to repository root):
#
#   testdata/objfmt/empty.pack             zero-object SHA-1 pack
#   testdata/objfmt/three-objects.pack     blob+tree+commit SHA-1 pack
#   testdata/objfmt/three-objects.idx      idx v2 paired with the above
#   testdata/objfmt/three-objects.offsets.txt
#   testdata/objfmt/ofs-delta.pack         two near-identical blobs,
#                                          OFS_DELTA encoded (default)
#   testdata/objfmt/ofs-delta.idx
#   testdata/objfmt/ofs-delta.offsets.txt
#   testdata/objfmt/ref-delta.pack         same blobs as ofs-delta but
#                                          --no-delta-base-offset
#   testdata/objfmt/ref-delta.idx
#   testdata/objfmt/ref-delta.offsets.txt
#   testdata/objfmt/sha256-empty.pack      zero-object SHA-256 pack
#   testdata/objfmt/sha256-empty.idx       idx v2 paired with the above
#   testdata/objfmt/sha256-three.pack      blob+tree+commit SHA-256 pack
#   testdata/objfmt/sha256-three.idx
#   testdata/objfmt/sha256-three.offsets.txt
#   testdata/objfmt/multi-pack-index        midx over the two midx packs
#                                          below (SHA-1)
#   testdata/objfmt/midx-pack-1.pack       first pack referenced by the
#                                          midx
#   testdata/objfmt/midx-pack-1.idx
#   testdata/objfmt/midx-pack-1.offsets.txt
#   testdata/objfmt/midx-pack-2.pack       second pack referenced by the
#                                          midx
#   testdata/objfmt/midx-pack-2.idx
#   testdata/objfmt/midx-pack-2.offsets.txt
#   testdata/objfmt/multi-pack-index.packnames
#                                          newline-separated PNAM
#                                          contents (`.idx` basenames) in
#                                          PNAM order; tests use this to
#                                          map a midx pack-index back to
#                                          a `.idx` fixture without
#                                          re-parsing the midx
#   testdata/objfmt/sha256-multi-pack-index
#                                          SHA-256 midx + two SHA-256
#                                          packs (same shape as above)
#   testdata/objfmt/sha256-midx-pack-1.{pack,idx,offsets.txt}
#   testdata/objfmt/sha256-midx-pack-2.{pack,idx,offsets.txt}
#   testdata/objfmt/sha256-multi-pack-index.packnames
#
# `git index-pack` defaults to writing version 2 idx files; that is
# what tests exercise. The version 1 layout is exercised separately
# via a test-only helper that hand-rolls a synthetic idx (see
# `internal/objfmt/idx_test.go`).
#
# Tested with: git version 2.53.0
#
# Usage:
#   testdata/_gen/objfmt.sh
#
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/../.." && pwd)"
out="$root/testdata/objfmt"
mkdir -p "$out"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# index_and_dump <pack-path> [extra-index-pack-args...]
#   Build a sidecar `.idx` next to the pack, run `git verify-pack -v`
#   to capture the canonical per-object listing. The `.idx` persists
#   alongside the pack so tests can exercise [Idx] without shelling
#   out; the auxiliary `.rev` file is discarded.
#
#   The trailing `<path>: ok` line that `git verify-pack -v` emits is
#   stripped via `sed '$d'` so the sidecar holds only the per-object
#   records and the `non delta:` summary — otherwise the host's
#   absolute path leaks into the committed fixture.
index_and_dump() {
    local pack="$1"; shift
    local stem="${pack%.pack}"
    git index-pack "$@" "$pack" >/dev/null
    git verify-pack -v "$stem.idx" | sed '$d' >"$stem.offsets.txt"
    rm -f "$stem.rev"
}

# --- Empty SHA-1 pack ---------------------------------------------------------
git -C "$work" init -q --object-format=sha1 --initial-branch=main empty-sha1
( cd "$work/empty-sha1" && git pack-objects --stdout </dev/null >"$out/empty.pack" )

# --- Three-object SHA-1 pack (blob, tree, commit) -----------------------------
repo="$work/three"
git -C "$work" init -q --object-format=sha1 --initial-branch=main three
(
    cd "$repo"
    git config user.email fixtures@example.invalid
    git config user.name  fixtures
    git config commit.gpgsign false
    printf 'hello fixture\n' >hello.txt
    git add hello.txt
    GIT_AUTHOR_DATE='2020-01-02T03:04:05+00:00' \
    GIT_COMMITTER_DATE='2020-01-02T03:04:05+00:00' \
        git commit -q -m 'fixture'
    commit=$(git rev-parse HEAD)
    tree=$(git rev-parse HEAD^{tree})
    blob=$(git rev-parse HEAD:hello.txt)
    printf '%s\n%s\n%s\n' "$commit" "$tree" "$blob" \
        | git pack-objects --stdout >"$out/three-objects.pack"
)
index_and_dump "$out/three-objects.pack"

# --- OFS_DELTA / REF_DELTA packs ---------------------------------------------
# Two near-identical blobs encourage `git pack-objects` to delta one
# against the other. Default is OFS_DELTA; --no-delta-base-offset
# coerces REF_DELTA encoding.
delta_repo="$work/delta"
git -C "$work" init -q --object-format=sha1 --initial-branch=main delta
(
    cd "$delta_repo"
    git config user.email fixtures@example.invalid
    git config user.name  fixtures
    git config commit.gpgsign false
    perl -e 'print "the quick brown fox jumps over the lazy dog\n" x 400' >a.txt
    perl -e 'print "the quick brown fox jumps over the lazy dog\n" x 400; print "tail\n"' >b.txt
    blob_a=$(git hash-object -w a.txt)
    blob_b=$(git hash-object -w b.txt)
    printf '%s\n%s\n' "$blob_a" "$blob_b" \
        | git pack-objects --stdout --delta-base-offset \
            >"$out/ofs-delta.pack"
    printf '%s\n%s\n' "$blob_a" "$blob_b" \
        | git pack-objects --stdout --no-delta-base-offset \
            >"$out/ref-delta.pack"
)
index_and_dump "$out/ofs-delta.pack"
index_and_dump "$out/ref-delta.pack"

# Sanity: confirm a delta chain is present in each pack. `git
# verify-pack -v` resolves deltas so the per-object lines show the
# final (non-delta) type; a non-zero chain length is what proves a
# delta exists. The OFS vs REF distinction is dictated by the
# `--delta-base-offset` flag passed to `git pack-objects` above.
grep -Eq '^chain length = [1-9]' "$out/ofs-delta.offsets.txt" \
    || { echo "ofs-delta.pack: no delta chain"; exit 1; }
grep -Eq '^chain length = [1-9]' "$out/ref-delta.offsets.txt" \
    || { echo "ref-delta.pack: no delta chain"; exit 1; }

# --- Empty SHA-256 pack -------------------------------------------------------
# `git index-pack` infers the hash algorithm from the surrounding
# repository, so the `.idx` build has to run inside the SHA-256 repo.
git -C "$work" init -q --object-format=sha256 --initial-branch=main empty-sha256
(
    cd "$work/empty-sha256"
    git pack-objects --stdout </dev/null >"$out/sha256-empty.pack"
    git index-pack "$out/sha256-empty.pack" >/dev/null
    rm -f "$out/sha256-empty.rev"
)

# --- Three-object SHA-256 pack (blob, tree, commit) ---------------------------
sha256_repo="$work/sha256-three"
git -C "$work" init -q --object-format=sha256 --initial-branch=main sha256-three
(
    cd "$sha256_repo"
    git config user.email fixtures@example.invalid
    git config user.name  fixtures
    git config commit.gpgsign false
    printf 'hello sha256\n' >hello.txt
    git add hello.txt
    GIT_AUTHOR_DATE='2020-01-02T03:04:05+00:00' \
    GIT_COMMITTER_DATE='2020-01-02T03:04:05+00:00' \
        git commit -q -m 'fixture'
    commit=$(git rev-parse HEAD)
    tree=$(git rev-parse HEAD^{tree})
    blob=$(git rev-parse HEAD:hello.txt)
    printf '%s\n%s\n%s\n' "$commit" "$tree" "$blob" \
        | git pack-objects --stdout >"$out/sha256-three.pack"
    git index-pack "$out/sha256-three.pack" >/dev/null
    git verify-pack -v "$out/sha256-three.idx" | sed '$d' >"$out/sha256-three.offsets.txt"
    rm -f "$out/sha256-three.rev"
)

# --- Multi-pack-index over two SHA-1 packs -----------------------------------
# A midx is only meaningful when it references at least two packs. Build
# a small repo, partition its objects into two disjoint groups, run
# `git pack-objects` once per group to produce two packs with stable
# names, then drop them into a fresh bare host and let
# `git multi-pack-index write` index them. The host repo is bare and
# disposable; only the resulting `multi-pack-index` plus the two
# referenced `.pack`/`.idx` pairs are retained as fixtures.
#
# Pack basenames are normalized to `midx-pack-{1,2}.pack` so test code
# can refer to them without re-discovering the content-addressed names
# emitted by `git pack-objects`. The PNAM chunk only stores the basename
# regardless.
midx_repo="$work/midx-src"
git -C "$work" init -q --object-format=sha1 --initial-branch=main midx-src
(
    cd "$midx_repo"
    git config user.email fixtures@example.invalid
    git config user.name  fixtures
    git config commit.gpgsign false

    printf 'midx pack-1 content\n' >a.txt
    git add a.txt
    GIT_AUTHOR_DATE='2020-01-02T03:04:05+00:00' \
    GIT_COMMITTER_DATE='2020-01-02T03:04:05+00:00' \
        git commit -q -m 'pack-1'
    c1=$(git rev-parse HEAD)
    t1=$(git rev-parse HEAD^{tree})
    b1=$(git rev-parse HEAD:a.txt)

    printf 'midx pack-2 content\n' >b.txt
    git add b.txt
    GIT_AUTHOR_DATE='2020-01-02T04:05:06+00:00' \
    GIT_COMMITTER_DATE='2020-01-02T04:05:06+00:00' \
        git commit -q -m 'pack-2'
    c2=$(git rev-parse HEAD)
    t2=$(git rev-parse HEAD^{tree})
    b2=$(git rev-parse HEAD:b.txt)

    mkdir -p packs
    printf '%s\n%s\n%s\n' "$c1" "$t1" "$b1" \
        | git pack-objects packs/p1 >/dev/null
    printf '%s\n%s\n%s\n' "$c2" "$t2" "$b2" \
        | git pack-objects packs/p2 >/dev/null
)

midx_host="$work/midx-host"
mkdir -p "$midx_host/objects/pack"
i=1
for pack in "$midx_repo"/packs/p?-*.pack; do
    stem="${pack%.pack}"
    cp "$pack" "$midx_host/objects/pack/midx-pack-$i.pack"
    cp "$stem.idx" "$midx_host/objects/pack/midx-pack-$i.idx"
    i=$((i + 1))
done
git --git-dir="$midx_host" --bare init -q --object-format=sha1
git --git-dir="$midx_host" multi-pack-index \
    --object-dir="$midx_host/objects" write
git --git-dir="$midx_host" multi-pack-index \
    --object-dir="$midx_host/objects" verify

install -m 0644 "$midx_host/objects/pack/multi-pack-index" \
    "$out/multi-pack-index"
for i in 1 2; do
    install -m 0644 "$midx_host/objects/pack/midx-pack-$i.pack" \
        "$out/midx-pack-$i.pack"
    install -m 0644 "$midx_host/objects/pack/midx-pack-$i.idx" \
        "$out/midx-pack-$i.idx"
    # verify-pack reads the hash algorithm from the surrounding repo
    # config, so run it inside the host repo where the idx lives.
    git --git-dir="$midx_host" verify-pack -v \
        "$midx_host/objects/pack/midx-pack-$i.idx" \
        | sed '$d' >"$out/midx-pack-$i.offsets.txt"
done
ls "$midx_host/objects/pack/" \
    | grep '^midx-pack-.*\.idx$' \
    | sort >"$out/multi-pack-index.packnames"

# --- Multi-pack-index over two SHA-256 packs ---------------------------------
# Same shape as above, but every step runs in a SHA-256 repo so the
# resulting midx records hash-version 2.
sha256_midx_repo="$work/sha256-midx-src"
git -C "$work" init -q --object-format=sha256 --initial-branch=main sha256-midx-src
(
    cd "$sha256_midx_repo"
    git config user.email fixtures@example.invalid
    git config user.name  fixtures
    git config commit.gpgsign false

    printf 'sha256 midx pack-1\n' >a.txt
    git add a.txt
    GIT_AUTHOR_DATE='2020-01-02T03:04:05+00:00' \
    GIT_COMMITTER_DATE='2020-01-02T03:04:05+00:00' \
        git commit -q -m 'pack-1'
    c1=$(git rev-parse HEAD)
    t1=$(git rev-parse HEAD^{tree})
    b1=$(git rev-parse HEAD:a.txt)

    printf 'sha256 midx pack-2\n' >b.txt
    git add b.txt
    GIT_AUTHOR_DATE='2020-01-02T04:05:06+00:00' \
    GIT_COMMITTER_DATE='2020-01-02T04:05:06+00:00' \
        git commit -q -m 'pack-2'
    c2=$(git rev-parse HEAD)
    t2=$(git rev-parse HEAD^{tree})
    b2=$(git rev-parse HEAD:b.txt)

    mkdir -p packs
    printf '%s\n%s\n%s\n' "$c1" "$t1" "$b1" \
        | git pack-objects packs/p1 >/dev/null
    printf '%s\n%s\n%s\n' "$c2" "$t2" "$b2" \
        | git pack-objects packs/p2 >/dev/null
)

sha256_midx_host="$work/sha256-midx-host"
mkdir -p "$sha256_midx_host/objects/pack"
i=1
for pack in "$sha256_midx_repo"/packs/p?-*.pack; do
    stem="${pack%.pack}"
    cp "$pack" "$sha256_midx_host/objects/pack/sha256-midx-pack-$i.pack"
    cp "$stem.idx" "$sha256_midx_host/objects/pack/sha256-midx-pack-$i.idx"
    i=$((i + 1))
done
git --git-dir="$sha256_midx_host" --bare init -q --object-format=sha256
git --git-dir="$sha256_midx_host" multi-pack-index \
    --object-dir="$sha256_midx_host/objects" write
git --git-dir="$sha256_midx_host" multi-pack-index \
    --object-dir="$sha256_midx_host/objects" verify

install -m 0644 "$sha256_midx_host/objects/pack/multi-pack-index" \
    "$out/sha256-multi-pack-index"
for i in 1 2; do
    install -m 0644 "$sha256_midx_host/objects/pack/sha256-midx-pack-$i.pack" \
        "$out/sha256-midx-pack-$i.pack"
    install -m 0644 "$sha256_midx_host/objects/pack/sha256-midx-pack-$i.idx" \
        "$out/sha256-midx-pack-$i.idx"
    git --git-dir="$sha256_midx_host" verify-pack -v \
        "$sha256_midx_host/objects/pack/sha256-midx-pack-$i.idx" \
        | sed '$d' >"$out/sha256-midx-pack-$i.offsets.txt"
done
ls "$sha256_midx_host/objects/pack/" \
    | grep '^sha256-midx-pack-.*\.idx$' \
    | sort >"$out/sha256-multi-pack-index.packnames"

echo "wrote fixtures into $out"

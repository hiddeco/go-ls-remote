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
#   testdata/objfmt/three-objects.offsets.txt
#   testdata/objfmt/ofs-delta.pack         two near-identical blobs,
#                                          OFS_DELTA encoded (default)
#   testdata/objfmt/ofs-delta.offsets.txt
#   testdata/objfmt/ref-delta.pack         same blobs as ofs-delta but
#                                          --no-delta-base-offset
#   testdata/objfmt/ref-delta.offsets.txt
#   testdata/objfmt/sha256-empty.pack      zero-object SHA-256 pack
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
#   to capture the canonical per-object listing, then drop the `.idx`
#   so only the pack and the text sidecar persist as fixtures.
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
    rm -f "$stem.idx" "$stem.rev"
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
git -C "$work" init -q --object-format=sha256 --initial-branch=main empty-sha256
( cd "$work/empty-sha256" && git pack-objects --stdout </dev/null >"$out/sha256-empty.pack" )

echo "wrote fixtures into $out"

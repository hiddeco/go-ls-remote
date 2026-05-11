#!/usr/bin/env bash
#
# Regenerate the canonical-Git byte corpus under
# `testdata/canonical/`. Runs canonical Git against each fixture in
# the curated matrix, captures wire bytes, and commits them as the
# byte-equivalence baseline our `internal/server` is asserted against.
#
# Developer-invoked. Tests do not shell out — they read the committed
# bytes only. Regenerate when the matrix changes or the pinned git
# version is bumped.
#
# Pinned canonical-Git version: see testdata/canonical/VERSION.
# To force regeneration with a different version: pass --force.
#
# The script invokes the canonical binary named by `$CANONICAL_GIT`
# (default `git`). Point it at an in-tree build to capture against a
# pinned source checkout, e.g.
#
#   CANONICAL_GIT=/Users/me/src/git/git ./testdata/_gen/canonical.sh
#
# Tested against canonical Git's source tree at
# `/Users/hhh/Projects/github.com/git/git`. `gitprotocol-pack.adoc`
# §"Reference Discovery" and `gitprotocol-v2.adoc` §"Capability
# Advertisement" / §"ls-refs" specify the wire shape captured here;
# `upload-pack.c::write_v0_ref` and `serve.c::process_request` are
# the production sites driving it.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")"/../.. && pwd)"
CANONICAL_DIR="$ROOT/testdata/canonical"
FIXTURES_DIR="$ROOT/testdata/repos"
VERSION_FILE="$CANONICAL_DIR/VERSION"
GIT_BIN="${CANONICAL_GIT:-git}"

force=""
if [ "${1:-}" = "--force" ]; then
    force="1"
fi

current_version="$("$GIT_BIN" --version | awk '{print $3}')"

if [ -z "$force" ] && [ -f "$VERSION_FILE" ]; then
    pinned_version="$(cat "$VERSION_FILE")"
    if [ "$current_version" != "$pinned_version" ]; then
        cat >&2 <<EOF
canonical.sh: pinned git version $pinned_version, but \`$GIT_BIN --version\`
reports $current_version. Refusing to regen the corpus against a
different version. Either point CANONICAL_GIT at $pinned_version or
pass --force to regenerate against the current version (and update
VERSION).
EOF
        exit 1
    fi
elif [ -z "$force" ] && [ ! -f "$VERSION_FILE" ]; then
    cat >&2 <<EOF
canonical.sh: no VERSION pin found at $VERSION_FILE. Pass --force on
the first run to establish the pin, then commit VERSION alongside
the captured bytes.
EOF
    exit 1
fi

mkdir -p "$CANONICAL_DIR"
echo "$current_version" > "$VERSION_FILE"

# materialize_fixture copies a fixture from testdata/repos/<name>/
# into a temp .git/, renaming any committed `dotgit` component to
# `.git`. Mirrors `internal/testfixture.MaterializeRepoTree`.
materialize_fixture() {
    local name="$1"
    local dest
    dest="$(mktemp -d)"
    cp -R "$FIXTURES_DIR/$name/." "$dest/"
    if [ -d "$dest/dotgit" ]; then
        mv "$dest/dotgit" "$dest/.git"
    fi
    # Nested dotgit components (worktrees, submodules) get the same
    # treatment. -depth so children get renamed before parents.
    find "$dest" -depth -name 'dotgit' -type d -print0 | while IFS= read -r -d '' d; do
        mv "$d" "$(dirname "$d")/.git"
    done
    # `objects/pack/` may be absent on ref-only fixtures; canonical Git
    # tolerates the absence on `--advertise-refs` but we materialise it
    # for parity with `internal/testfixture` and to keep object-info
    # captures (future) reproducible against the same tree.
    mkdir -p "$dest/.git/objects/pack"
    printf '%s' "$dest/.git"
}

# capture_advertisement_v2 runs `git upload-pack --advertise-refs`
# under v2 and writes the resulting wire bytes to outfile. v2's
# advertisement is the capability list (`serve.c:186-216`); refs are
# requested separately via the `ls-refs` command.
capture_advertisement_v2() {
    local gitdir="$1"
    local outfile="$2"
    GIT_PROTOCOL=version=2 \
        "$GIT_BIN" --git-dir="$gitdir" upload-pack \
            --advertise-refs --stateless-rpc "$gitdir" \
        > "$outfile"
}

# capture_ls_refs_v2 feeds a pre-built v2 ls-refs command-request to
# `git upload-pack --stateless-rpc` and writes the response to
# outfile. Canonical Git's stateless-rpc v2 path skips the
# advertisement and goes straight into the command loop
# (`serve.c::protocol_v2_serve_loop`), so the request bytes are the
# full payload the server side reads on the wire.
capture_ls_refs_v2() {
    local gitdir="$1"
    local request_path="$2"
    local outfile="$3"
    GIT_PROTOCOL=version=2 \
        "$GIT_BIN" --git-dir="$gitdir" upload-pack \
            --stateless-rpc "$gitdir" \
        < "$request_path" > "$outfile"
}

# write_ls_refs_request writes the v2 ls-refs command-request bytes
# to the given path. Frame shape per `gitprotocol-v2.adoc` §"Command
# Request" and `pkt-line.c::packet_write_fmt` (4-byte lowercase ASCII
# hex length prefix covering the prefix itself plus payload). Each
# line length is verified by hand:
#
#   `command=ls-refs\n`     16 + 4 = 20 = 0x0014
#   `object-format=sha1\n`  19 + 4 = 23 = 0x0017
#   `agent=lsremote/0\n`    17 + 4 = 21 = 0x0015
#   `peel\n`                 5 + 4 =  9 = 0x0009
#   `symrefs\n`              8 + 4 = 12 = 0x000c
#   `unborn\n`               7 + 4 = 11 = 0x000b
#
# The control packets are `0000` (flush) and `0001` (delim).
write_ls_refs_request() {
    local algo="$1"  # sha1 | sha256
    local outfile="$2"
    local algo_line
    case "$algo" in
        sha1)
            algo_line='0017object-format=sha1
'
            ;;
        sha256)
            algo_line='0019object-format=sha256
'
            ;;
        *)
            echo "canonical.sh: unknown algo $algo" >&2
            exit 1
            ;;
    esac
    {
        printf '0014command=ls-refs\n'
        printf '%s' "$algo_line"
        printf '0015agent=lsremote/0\n'
        printf '0001'
        printf '000bunborn\n'
        printf '0009peel\n'
        printf '000csymrefs\n'
        printf '0000'
    } > "$outfile"
}

# Curated matrix. `empty` exercises the no-refs branch; the others
# carry refs synthesised against placeholder OIDs (`aaaa...`,
# `cccc...`) that canonical Git happily advertises because
# `upload-pack --advertise-refs` does not validate ref targets
# against the object store.
fixtures_sha1=(
    empty
    loose-only
    packed-only
    with-reftable-content
)

for f in "${fixtures_sha1[@]}"; do
    out_dir="$CANONICAL_DIR/$f"
    mkdir -p "$out_dir"
    gitdir="$(materialize_fixture "$f")"
    parent="$(dirname "$gitdir")"

    capture_advertisement_v2 "$gitdir" "$out_dir/advertisement-v2.bin"

    # The empty fixture has no refs; ls-refs against it still produces
    # a valid (empty) response, so we capture it for the matrix.
    write_ls_refs_request sha1 "$out_dir/ls-refs.req"
    capture_ls_refs_v2 "$gitdir" "$out_dir/ls-refs.req" "$out_dir/ls-refs.bin"

    rm -rf "$parent"
done

echo "canonical.sh: captured $(find "$CANONICAL_DIR" -name '*.bin' | wc -l | tr -d ' ') artifacts under $CANONICAL_DIR"

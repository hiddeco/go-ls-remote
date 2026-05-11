# Canonical-Git byte corpus matrix

The committed corpus under `testdata/canonical/` is the byte-level
baseline `internal/server`'s output is asserted against by
`internal/server/canonical_test.go`. Regenerate via
`testdata/_gen/canonical.sh`; tests do not shell out.

## Pinned canonical-Git version

See `VERSION`. The capture script refuses to regen against a different
version unless `--force` is passed; if you bump the pinned version,
update `VERSION` and the corpus together in a single commit.

The script invokes the binary named by `$CANONICAL_GIT` (default
`git`). To capture against an in-tree build instead of the system
`git`, point the variable at the binary:

```
CANONICAL_GIT=/path/to/git/git testdata/_gen/canonical.sh
```

## Curated matrix

| Fixture                  | Advertisement | ls-refs | object-info |
|--------------------------|:-:|:-:|:-:|
| `empty`                  | ✓ | ✓ | (future) |
| `loose-only`             | ✓ | ✓ | (future) |
| `packed-only`            | ✓ | ✓ | (future) |
| `with-reftable-content`  | ✓ | ✓ | (future) |

Object-info captures and additional fixtures (sha256, alternates,
multi-pack-index) are deferred follow-ups. Each addition is one
commit: capture the bytes via `canonical.sh` (extending the
fixture/command list), add the matching test case, expand the
mask if the divergence pass surfaces a new variable region.

## Per-fixture artifacts

Each fixture directory carries up to three artifacts per supported
command:

- `advertisement-v2.bin` — the v2 capability advertisement bytes from
  `git upload-pack --advertise-refs --stateless-rpc` under
  `GIT_PROTOCOL=version=2`.
- `<command>.req` — the v2 command-request bytes fed to canonical
  Git over stdin (e.g. `ls-refs.req`). Committed alongside the
  response so the harness replays the exact bytes canonical was
  asked about.
- `<command>.bin` — the v2 response bytes canonical wrote to stdout.

## Adding a fixture or command

1. Edit `testdata/_gen/canonical.sh`: add the fixture name to the
   per-algo `fixtures_*` list (and the command to the per-fixture
   loop body).
2. Run the script:

   ```
   CANONICAL_GIT=/path/to/git/git testdata/_gen/canonical.sh
   ```

3. Inspect the captured bytes with `hexdump -C` to confirm framing.
4. Add a test row to `internal/server/canonical_test.go`'s matrix.
5. Run the test. If a real wire-divergence surfaces, fix it. If
   the divergence is a known acceptable difference (legitimately
   variable region the existing masks don't cover), extend the
   mask layer in `internal/server/canonical_mask.go`.
6. Commit the script edit, the captured bytes, and the test row
   together.

## The mask layer

Masks live in `internal/server/canonical_mask.go`. Two are
currently published:

- `MaskAgent` — replaces any `agent=<value>\n` data pkt-line with
  `agent=$AGENT$\n` and recomputes the 4-hex length prefix. Used by
  the ls-refs harness for defensive idempotence.
- `MaskV2Advertisement` — composes `MaskAgent` with a drop set
  covering capabilities the two implementations legitimately
  diverge on. Used by the advertisement harness.

The current drop set is:

| Capability       | Why it's dropped |
|------------------|------------------|
| `fetch=`         | canonical advertises by default; this read-only emulator does not implement fetch |
| `server-option`  | canonical advertises by default; the emulator does not service the optional extension |
| `object-info`    | the emulator advertises unconditionally; canonical 2.54 emits it only under `feature.experimental` |

The remaining caps (`agent`, `ls-refs`, `object-format`) survive
byte-identical so the harness substantively asserts equivalence on
the common cap subset.

## Mask justification bar

A mask is justified when:

- The masked region is legitimately variable across
  protocol-compliant implementations (e.g. version strings).
- The two implementations agree on framing — pkt-line lengths,
  flush placement, capability-list semantics — and the mask only
  normalises cosmetic content, or drops capabilities one side
  advertises and the other does not implement.

A mask is NOT justified when:

- The "divergence" is a real wire-bug. Fix the bug instead.
- The mask would hide framing differences. Investigate the framing
  before reaching for the mask.

Every mask must be idempotent: applying it twice yields the same
result as applying it once. The harness applies the mask to both
sides regardless of which one is canonical bytes off-disk; the
idempotence contract makes that order-independent.

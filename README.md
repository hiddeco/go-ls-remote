# go-ls-remote

[![Go Reference](https://pkg.go.dev/badge/github.com/hiddeco/go-ls-remote.svg)][godoc]
[![Go Report Card](https://goreportcard.com/badge/github.com/hiddeco/go-ls-remote)][goreport]

> Give a man a fish and you feed him for a day,
> give him an LLM and he will torture it into
> writing a spec-compliant Go port of `git ls-remote`.

A Go library for the discovery half of Git's wire protocols, i.e. the
surface you hit when you run `git ls-remote`. Self-contained: no
shelling out to a `git` binary, no `cgo`. Meant for tooling that needs
to know what's on a remote (branch heads, tags, default branch)
without cloning the repository or pulling in a full Git library like
[`go-git`][go-git], [`git2go`][git2go], or [`libgit2`][libgit2].

This library:

- Sticks to discovery. Ref advertisements with prefix filtering,
  peeled annotated tags, symref resolution, unborn-HEAD reporting,
  capability discovery, default-branch resolution, a reachability
  probe, and the v2 [`object-info`][object-info] command. No clone,
  fetch, push, or working-tree code lives anywhere in the module.
- Speaks all three wire-protocol versions (v0, v1, v2) over all four
  transports canonical Git supports: HTTP(S), both smart and legacy
  dumb HTTP; SSH; the anonymous `git://` daemon; and local `file://`
  repositories. Version negotiation matches canonical Git: a caller
  can pin a version, but by default the library asks for v2 and
  falls back to v1 or v0 when the server only speaks an earlier
  protocol.
- Pulls only the Go standard library for HTTP(S). SSH adds
  `golang.org/x/crypto/ssh`; the local-file transport adds
  `golang.org/x/exp/mmap`. Each dependency is opt-in and only pulled
  in when the corresponding transport is imported.
- Queries an object's size on the remote, without downloading the
  object, via the v2 `object-info` command on a long-lived
  [`Session`][session-godoc] that spreads the handshake cost across
  multiple discovery commands. Refs stream as an
  `iter.Seq2[Ref, error]`, and an early `break` drains the response
  so the session stays usable afterwards.
- Serves `file://` URLs by running its own in-process `upload-pack`
  loop against the on-disk repository, which means the library has
  to bundle read-only parsers for every format that loop needs to
  consult: loose refs, `packed-refs`, and [reftable][reftable];
  loose objects, pack v1/v2, and the multi-pack-index (including
  walking the `objects/info/alternates` chain). Both `sha1` and
  `sha256` repositories work, via a typed-per-algorithm hash
  abstraction.
- Funnels every transport-level failure (HTTP status, SSH fatal,
  file not-found, etc.) through a single `ProtocolError` to public
  sentinels (`ErrNotFound`, `ErrAuthRequired`, `ErrAuthFailed`,
  `ErrUnsupportedProtocol`, `ErrServerRefused`, `ErrNoDefaultBranch`,
  `ErrSessionDead`), so callers can match with `errors.Is` and not
  worry about transport-specific types.
- Exposes a `Tracer` interface for per-packet and per-event
  observability. Every emission site is gated by an explicit
  `if tracer != nil` check, so passing no tracer costs one nil
  comparison per event. No allocations, no formatting.
- Does not parse `~/.gitconfig` or `~/.ssh/config`, and does not
  speak the `git credential` helper protocol. Built-in helpers cover
  Basic, Bearer, `.netrc`, SSH agent, and SSH key authentication.
  Anything beyond that is on the caller.

For more on how to actually use this, see the [Go Reference][godoc].

## Compatibility with canonical Git

Canonical Git is the single source of truth for both wire behaviour
and on-disk format handling. Every protocol-sensitive decision was
made by reading the corresponding `builtin/`, `connect.c`, or
`pkt-line.c` source, or the relevant
`Documentation/gitprotocol-*.adoc` paragraph, before the test or
implementation got written. Load-bearing decisions carry a code
citation pinned to a specific Git version (currently v2.54.0). Where
Git's documented behaviour and its source-code behaviour disagree,
we follow the source and leave a comment explaining the divergence.

Test fixtures are real, not synthetic. The canonical corpus under
`testdata/canonical/` is captured by running an actual
`git upload-pack` against a pinned set of repositories and committing
the resulting bytes. The in-process server emulator and the wire
codec are then validated by comparing their output to those captured
bytes, byte for byte. A small mask layer names every region the two
implementations are allowed to differ in, and rejects new entries
unless the divergence is itself protocol-compliant.

Three categories of divergence are currently documented. First,
content that legitimately varies even when the framing is identical:
the `agent` capability, where canonical and the emulator advertise
different version strings, the same way any two `git` versions
would. Second, capabilities canonical advertises by default that
this library has chosen not to implement: `fetch` and
`server-option`, both excluded by the discovery-only scope. Third,
capabilities the emulator advertises unconditionally that canonical
only emits under `feature.experimental`, currently `object-info`,
fully implemented here and exposed as a first-class API. The framing
itself (pkt-line lengths, flush placement, capability-list ordering)
is never masked: a divergence there is a bug, not a difference to
paper over.

The same comparison is then run against every transport. An
in-process integration suite spins up HTTP(S), SSH, `git://`, and
`file://` harnesses against a shared fixture matrix and asserts that
every discovery command returns identical results across all four
wires. A regression in the wire codec, the in-process server, or URL
parsing then surfaces as a divergence between transports rather than
as silent drift. A separate nightly suite hits public repositories
on GitHub, GitLab, Codeberg, Bitbucket, and Gitea over
unauthenticated HTTPS, catching production-server behaviour drift
that in-process tests can't.

## Background

This library was built primarily as an experiment with LLM- and
agent-driven software development on a non-trivial, protocol-level
codebase. A secondary goal is to find patterns (in architecture,
scope discipline, error handling, and observability) that might be
useful as work on [`go-git`][go-git] continues.

## Contributing

This library is feature-complete for its stated scope, and new
features won't be accepted. Contributions that improve performance,
or that close gaps in compatibility with canonical Git
(`builtin/ls-remote.c`, `connect.c`, `pkt-line.c`, and the
[`gitprotocol-*.adoc`][git-protocol-v2] documentation), are welcome.

Please open an issue describing the problem before sending a pull
request, so we can agree on the approach first. Every commit needs a
`Signed-off-by:` trailer attesting to the [Developer Certificate of
Origin][dco] (`git commit -s`).

[git-protocol-v2]: https://github.com/git/git/blob/master/Documentation/gitprotocol-v2.adoc
[object-info]: https://github.com/git/git/blob/master/Documentation/gitprotocol-v2.adoc#object-info
[reftable]: https://github.com/git/git/blob/master/Documentation/technical/reftable.adoc
[go-git]: https://github.com/go-git/go-git
[git2go]: https://github.com/libgit2/git2go
[libgit2]: https://github.com/libgit2/libgit2
[session-godoc]: https://pkg.go.dev/github.com/hiddeco/go-ls-remote#Session
[dco]: https://developercertificate.org/
[godoc]: https://pkg.go.dev/github.com/hiddeco/go-ls-remote
[goreport]: https://goreportcard.com/report/github.com/hiddeco/go-ls-remote

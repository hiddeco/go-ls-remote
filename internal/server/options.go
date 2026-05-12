// Package server implements an in-process emulator of canonical Git's
// `upload-pack` discovery-time wire protocols. It speaks both the v0
// reference advertisement and the v2 capability advertisement plus
// command loop (`ls-refs`, `object-info`), reading from a [pktline.Reader]
// and writing to a [pktline.Writer] backed by any byte stream — a real
// network connection, an [io.Pipe], or a recording sink in tests.
//
// The emulator is read-only: it serves a [github.com/hiddeco/go-ls-remote/internal/objstore.Store]
// for ref enumeration and object metadata and never writes to the store.
// Object transfer (`fetch`) is out of scope.
package server

import (
	"github.com/hiddeco/go-ls-remote/trace"
	"github.com/hiddeco/go-ls-remote/transport"
)

// Options configures a [Serve] invocation.
type Options struct {
	// Agent is the agent string the server advertises in its capability
	// list. The value is opaque on the wire; canonical Git uses the
	// `git/<version>` format (see [version.c::git_user_agent]).
	//
	// [version.c::git_user_agent]: https://github.com/git/git/blob/v2.54.0/version.c#L28
	Agent string

	// PreferredProtocol selects which advertisement [Serve] emits: v0
	// (the empty-string ref list with embedded capabilities) or v2 (the
	// `version 2\n` prefix followed by a capability advertisement).
	//
	// The field is intentionally a non-pointer. Unlike client-side
	// `transport.ConnOptions.PreferredProtocol` — where `nil` means
	// "auto-negotiate" — the server has no auto mode: it must commit
	// to a single protocol before emitting its first byte. Defaulting
	// `Options{}` to [transport.ProtocolV0] (the integer zero) is
	// acceptable; callers that want v2 must say so explicitly.
	PreferredProtocol transport.ProtocolVersion

	// Tracer, when non-nil, receives [trace.CommandEvent] values around
	// each v2 command handler dispatch (`ls-refs`, `object-info`): one
	// with `Phase == trace.CommandStart` before the handler runs and one
	// with `Phase == trace.CommandEnd` after it returns, the latter
	// carrying the elapsed `Duration` and the handler's `Err` (nil on
	// success). The unknown-command path emits no CommandEvent: the
	// caller-visible wrapped [wire.ErrServerRefused] already encodes the
	// refusal. A nil Tracer disables emission entirely.
	//
	// Emitted events leave `URL` empty: the in-process emulator has no
	// remote URL to populate it with, and the field is reserved for
	// transports that do (notably HTTP). Consumers comparing events
	// across emulator and transport sources should treat `URL == ""` as
	// the emulator's deliberate signal rather than missing data.
	//
	// Packet-level tracing — the per-pkt-line [trace.PacketEvent] stream
	// — is the caller's responsibility, not Serve's. Serve receives an
	// already-constructed [pktline.Reader] and [pktline.Writer]; the
	// tracer must be wired in at construction time via
	// [pktline.WithReaderTracer] / [pktline.WithWriterTracer]. From the
	// server's perspective the reader's bytes flow inbound (client →
	// server) and the writer's bytes flow outbound (server → client),
	// matching [trace.DirectionInbound] and [trace.DirectionOutbound]
	// respectively.
	Tracer trace.Tracer
}

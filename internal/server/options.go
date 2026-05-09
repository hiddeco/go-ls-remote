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
	// `git/<version>` format (see `version.c::git_user_agent`).
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

	// Tracer, when non-nil, receives the same [trace.Event] values the
	// production transports emit, letting tests assert on the exact
	// wire activity the emulator generated. A nil Tracer disables
	// emission entirely.
	Tracer trace.Tracer
}

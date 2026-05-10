package lsremote

import "github.com/hiddeco/go-ls-remote/transport"

// Session represents an open discovery-time connection to a remote Git
// repository. A Session is produced by [Dial] after the handshake
// completes; methods on a Session issue one or more discovery commands
// — `ls-refs`, `object-info` — against the same underlying connection.
//
// # Opaque type
//
// Session has no exported fields. Callers obtain a `*Session` from
// [Dial] and interact with it only through its methods; the internal
// state — the live [transport.Conn], the negotiated [Capabilities],
// the cached advertisement-time refs (for v0/v1 only), the captured
// dial configuration and the redacted URL — is library-private.
//
// # Concurrency
//
// A `*Session` is safe for concurrent use only when the underlying
// transport multiplexes independent commands onto independent network
// requests. The HTTP transport satisfies this: each v2 command is its
// own POST, so two goroutines can issue commands against the same
// HTTP-backed Session without external synchronisation. The SSH,
// `git://`, and `file://` transports do NOT satisfy it: they share a
// single bidirectional byte stream where one in-flight command must
// drain before the next begins. Callers using a non-HTTP transport
// must serialise Session method calls externally.
//
// # Lifecycle
//
// A Session owns the underlying [transport.Conn]. The Session's
// `Close` method (added in a later iteration) closes the connection;
// calling Close more than once is a no-op per the
// [transport.Conn] contract.
type Session struct {
	// conn is the live transport-level connection. It owns the
	// advertisement reader (already consumed by [Dial]) and is the
	// channel through which future v2 commands flow.
	conn transport.Conn

	// caps is the negotiated capability snapshot built from the
	// advertisement. Handed back to callers via a future
	// `Capabilities()` accessor.
	caps Capabilities

	// refs holds the advertisement-time ref list for v0/v1 handshakes.
	// v2 leaves it nil — v2 callers fetch refs on demand via an
	// `ls-refs` command. The slice is allocated by [convertRefs] so it
	// does not alias any wire-layer buffer.
	refs []Ref

	// config is the resolved dial configuration. It is captured at
	// [Dial] time so later Session methods can reuse the configured
	// tracer, user agent, and protocol pin when issuing v2 commands.
	config dialConfig

	// url is the credential-redacted form of the URL [Dial] was called
	// with. It is stored once so each subsequent [ProtocolError] does
	// not need to re-derive the redaction.
	url string
}

// Package transport defines the abstraction every Git wire-protocol
// transport (HTTP, SSH, git daemon, file) implements.
//
// The package is dependency-free apart from
// [github.com/hiddeco/go-ls-remote/trace], so transport implementations
// can import it without pulling in unrelated parts of the library.
//
// # Layout
//
// The package owns four concepts:
//
//   - [ProtocolVersion] — the wire protocol version a client requests
//     or has negotiated.
//   - [URL] and [ParseURL] — the URL parser that all transports share,
//     hand-written because [net/url.Parse] mangles scp-style URLs.
//   - [Transport] and [Conn] — the interface contract every concrete
//     transport implementation satisfies.
//   - [Registry] — an explicit map of URL scheme to [Transport]. The
//     library never registers transports via init() side effects;
//     callers compose a Registry by hand or via the lsremote root
//     package's defaults.
package transport

import "fmt"

// ProtocolVersion is the wire protocol version a client requests or has
// negotiated with a server.
//
// Negotiation per canonical Git's `protocol.c`: the client announces a
// preferred version (or [ProtocolAuto]) and the server picks the
// highest it supports. See `determine_protocol_version_client` and
// `determine_protocol_version_server` in `protocol.c`.
type ProtocolVersion int

const (
	// ProtocolAuto requests the highest version available, accepting
	// v0 fallback. Most callers want this; the explicit Vn constants
	// exist for tests and for clients that need to pin a version for
	// compatibility with a known server.
	ProtocolAuto ProtocolVersion = -1

	// ProtocolV0 is the original wire protocol; the server's
	// capability list rides on the first ref-line of the
	// advertisement. See `gitprotocol-pack.adoc` §"Reference Discovery".
	ProtocolV0 ProtocolVersion = 0

	// ProtocolV1 prefixes a v0 advertisement with a literal
	// `version 1\n` line. Effectively unused in practice.
	ProtocolV1 ProtocolVersion = 1

	// ProtocolV2 is the modern stateless protocol with capability and
	// command negotiation. See `gitprotocol-v2.adoc`.
	ProtocolV2 ProtocolVersion = 2
)

// String returns a short label suitable for diagnostic output and
// log lines: `auto`, `v0`, `v1`, `v2`, or `unknown(N)` for any
// out-of-range value.
func (v ProtocolVersion) String() string {
	switch v {
	case ProtocolAuto:
		return "auto"
	case ProtocolV0:
		return "v0"
	case ProtocolV1:
		return "v1"
	case ProtocolV2:
		return "v2"
	default:
		return fmt.Sprintf("unknown(%d)", int(v))
	}
}

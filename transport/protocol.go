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

// ProtocolVersion identifies a Git wire protocol version. Defined values
// match the integer that travels on the wire: `int(ProtocolV2)` is the
// literal `2` in the `version 2\n` advertisement.
//
// The type represents a concrete wire identity, not a caller preference.
// The absence of a preference — "auto-negotiate" — is encoded one layer
// up by leaving [OpenOptions.PreferredProtocol] nil, not by a sentinel
// value here. Reserving every integer for a real version means
// [OpenOptions]'s zero value is unambiguously "no preference," which is
// also the spec default.
//
// Values outside the defined constants format as `unknown(N)` via
// [ProtocolVersion.String]; no other range checking is enforced at
// the type level.
//
// Negotiation per canonical Git's `protocol.c`: the client announces a
// preferred version and the server picks the highest it supports. See
// `determine_protocol_version_client` and
// `determine_protocol_version_server` in `protocol.c`.
type ProtocolVersion int

const (
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

// String returns a short label suitable for diagnostic output and log
// lines: `v0`, `v1`, `v2`, or `unknown(N)` for any out-of-range value.
func (v ProtocolVersion) String() string {
	switch v {
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

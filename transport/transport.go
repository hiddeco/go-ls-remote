package transport

import (
	"context"

	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/trace"
)

// Transport opens connections to repositories whose URL schemes it
// claims. A Transport is stateless metadata; per-call work happens in
// [Transport.Open].
//
// Built-in implementations live under the `transport/<scheme>`
// packages (e.g. `transport/http`, `transport/ssh`); the
// [github.com/hiddeco/go-ls-remote/transport/file] package will host
// `transport/file` once it ships. Callers compose a [Registry] with
// the implementations they need rather than relying on init() side
// effects.
//
// # User implementations
//
// Transport is an open extension point: third parties may implement
// it and register custom schemes via [Registry.Register]. A user
// implementation must honour the contracts documented on [Conn]
// (single-flight per connection, idempotent Close, lifecycle of
// readers returned from Advertisement and Command) and on
// [OpenOptions] (nil Tracer disables tracing; nil PreferredProtocol
// auto-negotiates). The library performs no runtime validation
// beyond the [Registry] scheme lookup, so a misbehaving Transport
// can corrupt session state — the contract is the only safety net.
type Transport interface {
	// Schemes returns the canonical scheme list this transport claims,
	// for example `{"https", "http"}`. Schemes are matched
	// case-insensitively when looked up in a [Registry].
	Schemes() []string

	// Open establishes a connection to u. On return the [Conn]'s
	// advertisement reader is ready and capability negotiation has
	// completed (or returned a transport-level error).
	//
	// opts carries the cross-transport options the root package
	// passes down — tracer, user agent, preferred protocol version.
	// Transport-specific options live on each transport package's
	// own constructor (for example `httpt.WithClient`).
	Open(ctx context.Context, u *URL, opts OpenOptions) (Conn, error)
}

// OpenOptions are the cross-transport options the root package passes
// down on every [Transport.Open] call.
//
// Field order packs the struct without padding on 64-bit platforms: the
// 16-byte interface and string headers cluster ahead of the
// pointer-shaped PreferredProtocol.
type OpenOptions struct {
	// Tracer receives [trace.Event] values emitted during the dial
	// and subsequent I/O. A nil Tracer disables tracing entirely;
	// transports check for nil at every emission site.
	Tracer trace.Tracer

	// UserAgent is the agent string the client advertises to the
	// server. The empty string means "use the transport's default."
	UserAgent string

	// PreferredProtocol pins the negotiation to a specific wire
	// version. A nil pointer means auto-negotiate — request the
	// highest version the server speaks, accepting v0 fallback —
	// which is what most callers want and what [OpenOptions]'s zero
	// value yields. A non-nil pointer pins; the transport returns
	// `ErrUnsupportedProtocol` (wrapped in `*ProtocolError`) if the
	// server cannot satisfy the pin.
	PreferredProtocol *ProtocolVersion
}

// Conn is a single-flight connection to a Git server.
//
// User implementations are supported as part of the [Transport]
// extension contract; the rules below are the load-bearing seams
// that custom transports must respect to interoperate with the
// higher-layer Session.
//
// # Concurrency
//
// Conn is NOT safe for concurrent use. While a `*pktline.Reader`
// returned from [Conn.Advertisement] or [Conn.Command] is open,
// calling [Conn.Command] again on the same Conn has undefined
// behaviour. Drain or close the previous reader first.
//
// The public Session contract (lsremote.Session) may permit
// concurrent calls when the underlying transport multiplexes
// (HTTP can; SSH, git, and file cannot). That contract is
// documented at the Session layer; at this layer Conn is always
// single-flight.
type Conn interface {
	// Advertisement returns a reader over the initial advertisement
	// pkt-lines. It is available on every negotiated protocol
	// version: v0 and v1 stream the cap-bearing first ref line and
	// any subsequent ref lines until flush, and v2 streams the
	// version line followed by capability lines until flush. The
	// reader must be called and consumed (or closed) exactly once
	// before [Conn.Command].
	Advertisement() *pktline.Reader

	// Command issues a v2 command and returns a reader over its
	// response. The returned reader streams the response pkt-lines.
	//
	// Returns the protocol-mismatch sentinel (defined at the lsremote
	// layer) when the negotiated version is not v2. body encodes the
	// v2 command-request frame onto the transport's [pktline.Writer];
	// it is invoked exactly once per call, before any response bytes
	// are read. name is forwarded for diagnostics and tracing only —
	// the body callback writes the on-wire `command=` line itself.
	Command(ctx context.Context, name string, body CommandBody) (*pktline.Reader, error)

	// Close releases any underlying resources.
	//
	// Implementations must make Close idempotent: a second or later
	// call after the first must be a no-op that returns nil.
	// Higher-level Sessions may rely on this when their own teardown
	// races with an explicit Close from the caller.
	Close() error
}

// CommandBody encodes the body of a v2 command-request onto w. A
// transport invokes the callback exactly once per [Conn.Command] call
// and supplies a [pktline.Writer] positioned at the start of the
// request frame; the writer must not be retained past the callback's
// return.
//
// The callback writes the complete frame — the `command=` line, every
// capability line, the `delim-pkt`, every argument line, and the
// closing `flush-pkt` — as documented in `gitprotocol-v2.adoc`
// §"Command Request". Returning a non-nil error aborts the call
// without dispatching a request; the transport surfaces the error to
// its caller unchanged or wrapped in its own diagnostic envelope per
// the transport's contract.
type CommandBody func(w *pktline.Writer) error

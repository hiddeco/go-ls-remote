package gitt

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hiddeco/go-ls-remote/internal/wire"
	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/transport"
)

// Open dials the git-daemon and sends the initial pkt-line request,
// then returns a [Conn] whose advertisement reader is ready for the
// caller to consume.
//
// # Wire protocol
//
// The initial pkt-line follows the grammar from
// `gitprotocol-pack.adoc §"Extra Parameters"` and matches
// `connect.c::git_connect_git` (lines 1288-1298) in canonical Git:
//
//	git-upload-pack <path> NUL host=<host[:port]> NUL [NUL version=<N> NUL]
//
// The payload carries no trailing LF; canonical Git strips one if
// present (see `daemon.c:752-754`), so omitting it is the safe shape.
//
// With a nil [transport.OpenOptions.PreferredProtocol] the transport
// auto-negotiates v2, so the version trailer carries `version=2`.
// Protocol v1 is rejected up-front (see below).
//
// # Protocol v1 rejection
//
// v1 is rejected before any dial because canonical Git's `serve.c` and
// `upload-pack.c` advertise v0 and v2 only; v1 is effectively unused
// in the ecosystem. Mirroring this at the client avoids a dial that
// would stall on a server that cannot satisfy the request.
//
// # Default port
//
// When the URL carries no port, port 9418 is used. The value is the
// well-known git-daemon port, defined as `DEFAULT_GIT_PORT` in
// `protocol.h:23` of canonical Git and resolved by
// `connect.c:818,888,1040`.
//
// # Error mapping
//
// A pre-cancelled context surfaces [context.Canceled] directly. A v1
// pin returns `*ProtocolError{Op: "dial", Err: ErrUnsupportedProtocol}`
// before dialing. Dial failures surface as `*ProtocolError{Op: "dial"}`
// wrapping the raw network error (or the context error directly for
// [context.Canceled] / [context.DeadlineExceeded]). An initial pkt-line
// write failure closes the TCP connection and surfaces as
// `*ProtocolError{Op: "dial"}` wrapping a descriptive `fmt.Errorf`.
func (t *Transport) Open(ctx context.Context, u *transport.URL, opts transport.OpenOptions) (transport.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	redacted := transport.RedactURL(u.Raw)

	if err := resolvePreferredProtocol(opts.PreferredProtocol); err != nil {
		return nil, &ProtocolError{URL: redacted, Op: "dial", Err: err}
	}

	addr := hostAddress(u)
	dial := t.resolvedDialFn()
	netConn, err := dial(ctx, "tcp", addr)
	if err != nil {
		return nil, mapDialError(err, redacted)
	}

	writer := pktline.NewWriter(netConn, outboundWriterOpts(opts.Tracer, redacted)...)
	if err := wire.WriteStreamRequest(writer, u, opts.PreferredProtocol); err != nil {
		_ = netConn.Close()
		return nil, &ProtocolError{
			URL: redacted,
			Op:  "dial",
			Err: fmt.Errorf("gitt: write initial pkt-line: %w", err),
		}
	}

	return &Conn{
		conn:        netConn,
		reader:      pktline.NewReader(netConn, inboundReaderOpts(opts.Tracer, redacted)...),
		writer:      writer,
		redactedURL: redacted,
	}, nil
}

// hostAddress assembles the `host:port` string for the TCP dial. The
// default port is `9418` (the well-known git-daemon port; see
// `protocol.h:23` and `connect.c:818` in canonical Git). IPv6 literals
// are bracketed so the host/port split is unambiguous; the bracketing
// assumes `u.Host` is unbracketed, which is the [transport.URL]
// invariant established by `ParseURL`.
func hostAddress(u *transport.URL) string {
	port := u.Port
	if port == "" {
		port = "9418"
	}
	host := u.Host
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return host + ":" + port
}

// resolvePreferredProtocol validates the caller's protocol pin. A nil
// pointer means "auto-negotiate" (commits to v2). v0 and v2 are
// accepted verbatim. v1 is rejected because canonical Git's server
// implementations (`serve.c`, `upload-pack.c`) advertise v0 and v2
// only; v1 is effectively unused in the ecosystem.
func resolvePreferredProtocol(p *transport.ProtocolVersion) error {
	if p == nil {
		return nil
	}
	switch *p {
	case transport.ProtocolV0, transport.ProtocolV2:
		return nil
	default:
		return ErrUnsupportedProtocol
	}
}

// mapDialError maps a TCP dial failure to a `*ProtocolError`. Context
// cancellation and deadline exceeded propagate directly so callers can
// match them via [errors.Is] without unwrapping. All other failures
// wrap verbatim.
func mapDialError(err error, redacted string) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return &ProtocolError{URL: redacted, Op: "dial", Err: err}
}

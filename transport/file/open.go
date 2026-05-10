package filet

import (
	"context"
	"errors"
	"io"
	"net/url"

	"github.com/hiddeco/go-ls-remote/internal/objstore"
	"github.com/hiddeco/go-ls-remote/internal/server"
	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/transport"
)

// open dials u and returns a [Conn] whose advertisement reader is
// already streaming bytes from an in-process `server.Serve`
// goroutine. The dial path is synchronous on the failure side: a bad
// URL, a missing repository, or an unsupported pinned protocol
// surface as `*ProtocolError` before any goroutine spawns. On
// success, ownership of the opened
// [github.com/hiddeco/go-ls-remote/internal/objstore.Store] and the
// spawned goroutine transfers to the [Conn]; [Conn.Close] is the
// single shutdown path.
//
// # Pipe topology
//
// Two `io.Pipe` pairs wire the client and server halves together,
// one per direction. The server goroutine reads from `serverReader`
// (fed by the client's `clientWriter`) and writes to `serverWriter`
// (drained by the client's `clientReader`). Two pipes — rather than
// one duplex stream — are required because `io.Pipe` is unidirectional;
// the per-direction pair preserves the back-pressure semantics the
// server's pkt-line writer relies on.
//
// # Error mapping
//
// `objstore.Open` returns one of three sentinels for non-IO failures:
//
//   - `objstore.ErrNotARepo` → `ErrNotFound`. A `file://` URL pointing
//     at a non-repo path is the local equivalent of HTTP 404; the
//     caller's intent ("query this repo") cannot be served.
//   - `objstore.ErrUnsupportedFormat` → `ErrUnsupportedFormat`. A
//     `core.repositoryformatversion` or `extensions.*` value the
//     parser rejects (e.g. a future reftable variant) surfaces here.
//   - `objstore.ErrCorruptObject` → `ErrServerRefused`. Rare at
//     `Open`, but possible when an alternates-chain entry refers to a
//     corrupt store; `*ProtocolError.Server` carries the corruption
//     message.
//
// Other `objstore.Open` errors (filesystem failures, permission
// denied) flow through verbatim wrapped in `*ProtocolError` with no
// sentinel.
func (t *Transport) open(ctx context.Context, u *transport.URL, opts transport.OpenOptions) (transport.Conn, error) {
	redacted := transport.RedactURL(u.Raw)

	// Pin v1 up-front. `internal/server.Serve` rejects v1 with
	// `server.ErrUnsupportedProtocol` once the goroutine starts, but
	// surfacing the error before the goroutine spawns matches the
	// file transport's fail-fast-at-dial posture and keeps the
	// `*ProtocolError` path symmetric across error sources.
	preferred, err := resolvePreferredProtocol(opts.PreferredProtocol)
	if err != nil {
		return nil, &ProtocolError{URL: redacted, Op: "dial", Err: err}
	}

	// `transport.ParseURL` leaves `%`-escapes intact in `URL.Path`;
	// `objstore.Open` expects an OS path. Decode here so a URL like
	// `file:///tmp/repo%20with%20spaces` reaches the store as
	// `/tmp/repo with spaces`. A malformed escape is callable
	// equivalent to a non-existent local repo, so the decode error
	// maps to `ErrNotFound` rather than a separate sentinel.
	path, err := url.PathUnescape(u.Path)
	if err != nil {
		return nil, &ProtocolError{URL: redacted, Op: "dial", Err: ErrNotFound}
	}

	store, err := objstore.Open(path)
	if err != nil {
		return nil, mapOpenError(err, redacted)
	}

	clientReader, serverWriter := io.Pipe()
	serverReader, clientWriter := io.Pipe()

	derivedCtx, cancel := context.WithCancel(ctx)

	// Wire the tracer at the client-side reader/writer
	// unconditionally. The server-side endpoints in the goroutine
	// below are wired only when [WithEndpointTrace] was supplied —
	// see `tracer.go` for the event-doubling rationale and why the
	// default matches the HTTP transport's one-event-per-pkt-line
	// shape.
	conn := &Conn{
		reader:       pktline.NewReader(clientReader, inboundReaderOpts(opts.Tracer, redacted)...),
		writer:       pktline.NewWriter(clientWriter, outboundWriterOpts(opts.Tracer, redacted)...),
		store:        store,
		cancel:       cancel,
		done:         make(chan struct{}),
		clientReader: clientReader,
		clientWriter: clientWriter,
		serverReader: serverReader,
		serverWriter: serverWriter,
	}

	// `server.Options.Tracer` carries the [trace.CommandEvent] surface
	// the in-process emulator emits around each request. It is
	// independent of the per-pkt-line `PacketEvent` wiring above: the
	// command-level surface stays wired by default so callers see
	// command tracing through every transport without opt-in.
	srvOpts := server.Options{
		Agent:             opts.UserAgent,
		PreferredProtocol: preferred,
		Tracer:            opts.Tracer,
	}

	serverTracer := serverEndpointTracer(t, opts.Tracer)
	go func() {
		defer close(conn.done)

		// Closing the server-side pipe ends with the `Serve` error
		// surfaces it on the client's next read or write rather than
		// silently dropping bytes. `io.PipeWriter.CloseWithError`
		// stores the error so the matching `clientReader.Read`
		// returns it directly, matching how a real transport would
		// propagate a server-side fault.
		srvErr := server.Serve(derivedCtx,
			pktline.NewReader(serverReader, inboundReaderOpts(serverTracer, redacted)...),
			pktline.NewWriter(serverWriter, outboundWriterOpts(serverTracer, redacted)...),
			store,
			srvOpts,
		)

		conn.serverErr = srvErr

		_ = serverWriter.CloseWithError(srvErr)
		_ = serverReader.CloseWithError(srvErr)
	}()

	return conn, nil
}

// resolvePreferredProtocol turns a caller-supplied
// [transport.OpenOptions.PreferredProtocol] pointer into the
// non-pointer value `internal/server.Options.PreferredProtocol`
// expects. A nil pointer means "auto-negotiate" — the file transport
// commits to v2 for the auto path, mirroring
// `wire.HTTPProtocolHeader(nil)`. A non-nil pointer pins the
// negotiation; v1 is rejected up-front because the in-process
// emulator does not implement it (see `internal/server.ErrUnsupportedProtocol`).
func resolvePreferredProtocol(p *transport.ProtocolVersion) (transport.ProtocolVersion, error) {
	if p == nil {
		return transport.ProtocolV2, nil
	}
	switch *p {
	case transport.ProtocolV0, transport.ProtocolV2:
		return *p, nil
	default:
		return 0, ErrUnsupportedProtocol
	}
}

// mapOpenError translates an `objstore.Open` failure into a
// `*ProtocolError` with the appropriate sentinel wrapped. Filesystem
// errors and any other non-sentinel-wrapped causes flow through
// verbatim so the caller sees the actionable underlying error.
func mapOpenError(err error, redacted string) error {
	switch {
	case errors.Is(err, objstore.ErrNotARepo):
		return &ProtocolError{URL: redacted, Op: "dial", Err: ErrNotFound}
	case errors.Is(err, objstore.ErrUnsupportedFormat):
		return &ProtocolError{URL: redacted, Op: "dial", Err: ErrUnsupportedFormat}
	case errors.Is(err, objstore.ErrCorruptObject):
		return &ProtocolError{URL: redacted, Op: "dial", Server: err.Error(), Err: ErrServerRefused}
	default:
		return &ProtocolError{URL: redacted, Op: "dial", Err: err}
	}
}

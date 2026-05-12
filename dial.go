package lsremote

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/hiddeco/go-ls-remote/internal/wire"
	"github.com/hiddeco/go-ls-remote/transport"
	filet "github.com/hiddeco/go-ls-remote/transport/file"
	httpt "github.com/hiddeco/go-ls-remote/transport/http"
	ssht "github.com/hiddeco/go-ls-remote/transport/ssh"
)

// Dial opens a discovery-time connection to the Git repository at
// rawURL and returns a [*Session] ready to issue discovery commands.
//
// Dial is eager: by the time it returns, the URL has been parsed, a
// transport has been selected, the connection has been opened, and the
// server's capability advertisement has been read and translated into
// a [Capabilities] value. Callers who want lazy connection setup
// should defer the Dial itself, not wrap it.
//
// # Options
//
// The resolved configuration is:
//
//   - The transport [transport.Registry] supplied via [WithTransports],
//     or — when omitted — an HTTP-only default registry covering the
//     `https` and `http` schemes.
//   - The [trace.Tracer] supplied via [WithTracer], or no tracing.
//   - The User-Agent string supplied via [WithUserAgent], or the
//     transport's own default.
//   - The protocol version pinned by [WithProtocol], or auto-negotiate
//     when omitted. Auto-negotiation prefers v2 and falls back to v0.
//
// # Error model
//
// Failure modes split by the discovery step that produced them.
//
//   - URL parse failures from [transport.ParseURL] are returned
//     verbatim; they never reach the wire, so wrapping them in a
//     [ProtocolError] would be misleading. Callers match these with
//     [errors.Is] against the [transport.ErrEmptyURL],
//     [transport.ErrUnsupportedScheme], and related sentinels.
//   - An unknown scheme — the URL parsed but no transport is
//     registered for it — surfaces as a [*ProtocolError] with
//     `Op == "dial"`, `Err` wrapping the public
//     [ErrUnsupportedProtocol] sentinel and a short message on
//     [ProtocolError.Server] naming the missing scheme.
//   - Any error returned by the chosen transport's
//     [transport.Transport.Open] is wrapped in a [*ProtocolError]
//     with `Op == "dial"` and the underlying error placed on `Err`.
//     [errors.Is] walks transitively through the wrap, so callers
//     match against the public [ErrNotFound], [ErrAuthRequired],
//     [ErrAuthFailed], and [ErrUnsupportedProtocol] sentinels without
//     having to know which transport produced the failure.
//   - An error reading or parsing the server's advertisement surfaces
//     as a [*ProtocolError] with `Op == "advertisement"`. The wire
//     layer's `ErrUnsupportedProtocol` sentinel is joined with the
//     public [ErrUnsupportedProtocol] so a caller's `errors.Is` check
//     against the public sentinel succeeds. The underlying
//     [transport.Conn] is closed on this path so callers do not see a
//     leaked half-open connection.
//
// See [Option], [WithTransports], [WithTracer], [WithUserAgent],
// [WithProtocol], and [Session] for the surrounding types.
func Dial(ctx context.Context, rawURL string, opts ...Option) (*Session, error) {
	cfg := dialConfig{}
	for _, opt := range opts {
		if opt == nil {
			continue
		}
		opt.applyDial(&cfg)
	}
	if cfg.registry == nil {
		cfg.registry = defaultRegistry()
	}

	u, err := transport.ParseURL(rawURL)
	if err != nil {
		// URL parse failures never reach the wire; surface them
		// verbatim so callers' `errors.Is` checks against
		// `transport.ErrEmptyURL` and friends keep working.
		return nil, err
	}

	redactedURL := transport.RedactURL(rawURL)

	tr, ok := cfg.registry.Lookup(u.Scheme)
	if !ok {
		return nil, &ProtocolError{
			URL:    redactedURL,
			Op:     "dial",
			Server: fmt.Sprintf("no transport registered for scheme %q", u.Scheme),
			Err:    ErrUnsupportedProtocol,
		}
	}

	conn, err := tr.Open(ctx, u, transport.OpenOptions{
		Tracer:            cfg.tracer,
		UserAgent:         cfg.userAgent,
		PreferredProtocol: cfg.protocol,
	})
	if err != nil {
		// Each scheme-specific transport defines its own sentinel
		// identities for the well-known failure modes (HTTP 404,
		// 401-without-creds, etc.). Bridge those to the public
		// sentinels here via `errors.Join` so a caller's
		// `errors.Is(err, lsremote.ErrNotFound)` succeeds without
		// having to know which transport produced the failure. The
		// underlying transport error stays reachable on the joined
		// chain, so `errors.As` against the transport's own
		// `*ProtocolError` continues to work for callers who want the
		// HTTP status code or server message.
		//
		// The transport's own `Server` (and `Status`, when the
		// transport is HTTP) is lifted onto the outer
		// `*ProtocolError` so the public surface honours the contract
		// on `ProtocolError.Server`: callers should not have to walk
		// the wrapped chain via `errors.As` to read a diagnostic
		// excerpt the library already captured.
		pe := &ProtocolError{
			URL: redactedURL,
			Op:  "dial",
			Err: bridgeOpenError(err),
		}
		populateFromTransportError(pe, err)
		return nil, pe
	}

	adv, err := wire.ParseAdvertisement(conn.Advertisement(), cfg.protocol)
	if err != nil {
		// The advertisement reader belongs to the transport; the
		// caller never sees the partially-consumed Conn, so close it
		// before returning to avoid leaking the underlying network
		// resource.
		_ = conn.Close()

		return nil, &ProtocolError{
			URL: redactedURL,
			Op:  "advertisement",
			Err: bridgeWireSentinel(err),
		}
	}

	return &Session{
		conn:    conn,
		caps:    convertCaps(adv.Caps, adv.Version),
		rawCaps: slices.Clone(adv.Caps),
		refs:    convertRefs(adv.Refs),
		config:  cfg,
		url:     redactedURL,
	}, nil
}

// openSentinelBridges maps generic transport-layer sentinel
// identities to the public sentinel that should be joined onto the
// error chain so `errors.Is(err, lsremote.ErrX)` succeeds regardless
// of which scheme produced the failure. Each scheme-specific
// transport defines its own `*transport.SchemeError` bound to one
// of these generic identities, so a single match here covers every
// current and future transport that follows the convention.
var openSentinelBridges = []struct{ generic, public error }{
	{transport.ErrNotFound, ErrNotFound},
	{transport.ErrAuthRequired, ErrAuthRequired},
	{transport.ErrAuthFailed, ErrAuthFailed},
	{transport.ErrUnsupportedProtocol, ErrUnsupportedProtocol},
}

// wireSentinelBridges maps wire-layer sentinel identities to the
// public sentinel that should be joined onto the chain. The wire
// layer keeps its own sentinels for in-package matching; the public
// surface joins them at the boundary so callers never have to walk
// the wire layer.
var wireSentinelBridges = []struct{ wire, public error }{
	{wire.ErrUnsupportedProtocol, ErrUnsupportedProtocol},
	{wire.ErrServerRefused, ErrServerRefused},
}

// bridgeOpenError translates an error returned by a
// [transport.Transport.Open] into a form whose `errors.Is` chain
// reaches the public sentinels [ErrNotFound], [ErrAuthRequired],
// [ErrAuthFailed], and [ErrUnsupportedProtocol]. See
// [openSentinelBridges] for the mapping.
//
// Each scheme-specific transport defines its own
// `*transport.SchemeError` bound to one of the generic
// `transport.Err*` identities listed in the table, so a single
// match here covers every current and future transport that
// follows the convention.
//
// `errors.Join` is the idiomatic way to satisfy `errors.Is` against
// multiple identities without rebuilding every other field the
// transport's own error type carries — its message, its status
// code, its server-body excerpt all remain reachable via
// `errors.As` on the joined chain.
//
// When err matches none of the known sentinels the original error
// is returned unchanged.
func bridgeOpenError(err error) error {
	for _, b := range openSentinelBridges {
		if errors.Is(err, b.generic) {
			return errors.Join(b.public, err)
		}
	}
	return err
}

// bridgeWireSentinel translates a wire-layer error into a form whose
// `errors.Is` chain reaches a public sentinel — see
// [wireSentinelBridges] for the mapping. Used at advertisement-parse
// time in [Dial] and during v2 command exchanges in
// [Session.protocolError]; the centralised table means a future wire
// sentinel is bridged uniformly at every site.
//
// When err matches none of the known sentinels the original error is
// returned unchanged.
func bridgeWireSentinel(err error) error {
	for _, b := range wireSentinelBridges {
		if errors.Is(err, b.wire) {
			return errors.Join(b.public, err)
		}
	}
	return err
}

// populateFromTransportError lifts a transport-level
// `*ProtocolError`'s diagnostic fields (`Server`, and `Status` when the
// underlying transport is HTTP) onto the outer `*ProtocolError` so
// callers can read them off the public surface without a manual
// `errors.As` walk.
//
// Each scheme-specific transport package defines its own
// `*ProtocolError` shape; the helper fishes them out via `errors.As`
// rather than via a shared interface so adding new transports does not
// drag a new package-level dependency into the root package. New
// transports that should propagate further fields plug a case in here.
//
// `Server` is copied verbatim from the transport-level excerpt: the
// HTTP transport already caps the body at ~1 KiB plus a trailing
// `"..."` truncation marker in `readServerExcerpt`. A second
// truncation here would strip the marker the transport emitted to
// flag that more bytes were dropped, leaving callers unable to tell a
// short body from a long-and-truncated one.
func populateFromTransportError(dst *ProtocolError, src error) {
	// Add a case here when another transport package gains a
	// Server-carrying excerpt that should surface on the public
	// `*ProtocolError`. The type-switch is deliberate: each
	// transport's `*ProtocolError` shape is its own type, and a shared
	// interface would expand the public surface without payback.
	var httpErr *httpt.ProtocolError
	if errors.As(src, &httpErr) {
		if httpErr.Server != "" {
			dst.Server = httpErr.Server
		}
		if httpErr.Status != 0 {
			dst.Status = httpErr.Status
		}
		return
	}
	var sshErr *ssht.ProtocolError
	if errors.As(src, &sshErr) {
		if sshErr.Server != "" {
			dst.Server = sshErr.Server
		}
		return
	}
	var fileErr *filet.ProtocolError
	if errors.As(src, &fileErr) {
		if fileErr.Server != "" {
			dst.Server = fileErr.Server
		}
		return
	}
}

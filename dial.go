package lsremote

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/hiddeco/go-ls-remote/internal/wire"
	"github.com/hiddeco/go-ls-remote/transport"
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
		return nil, &ProtocolError{
			URL: redactedURL,
			Op:  "dial",
			Err: bridgeOpenError(err),
		}
	}

	adv, err := wire.ParseAdvertisement(conn.Advertisement(), cfg.protocol)
	if err != nil {
		// The advertisement reader belongs to the transport; the
		// caller never sees the partially-consumed Conn, so close it
		// before returning to avoid leaking the underlying network
		// resource.
		_ = conn.Close()

		// Translate the wire-layer `ErrUnsupportedProtocol` into the
		// public sentinel via `errors.Join`. The joined error
		// satisfies `errors.Is` against both sentinels, so a caller's
		// `errors.Is(err, lsremote.ErrUnsupportedProtocol)` succeeds
		// even though the wire layer's sentinel has a distinct
		// identity.
		wrapped := err
		if errors.Is(err, wire.ErrUnsupportedProtocol) {
			wrapped = errors.Join(ErrUnsupportedProtocol, err)
		}
		return nil, &ProtocolError{
			URL: redactedURL,
			Op:  "advertisement",
			Err: wrapped,
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

// bridgeOpenError translates an error returned by a
// [transport.Transport.Open] into a form whose `errors.Is` chain
// reaches the public sentinels [ErrNotFound], [ErrAuthRequired],
// [ErrAuthFailed], and [ErrUnsupportedProtocol].
//
// Each scheme-specific transport defines its own sentinel identities
// as [*transport.SchemeError] values bound to one of the generic
// `transport.Err*` identities. Matching on the generic identity here
// means every scheme — including user-defined transports following
// the same convention — bridges into the public `errors.Is` chain
// without library changes. `errors.Join` is the idiomatic way to
// satisfy `errors.Is` against multiple identities without rebuilding
// every other field the transport's own error type carries — its
// message, its status code, its server-body excerpt all remain
// reachable via `errors.As` on the joined chain.
//
// When err matches none of the known sentinels the original error is
// returned unchanged, so the wrap is a no-op for transport-level
// errors the public surface area does not yet name.
func bridgeOpenError(err error) error {
	switch {
	case errors.Is(err, transport.ErrNotFound):
		return errors.Join(ErrNotFound, err)
	case errors.Is(err, transport.ErrAuthRequired):
		return errors.Join(ErrAuthRequired, err)
	case errors.Is(err, transport.ErrAuthFailed):
		return errors.Join(ErrAuthFailed, err)
	case errors.Is(err, transport.ErrUnsupportedProtocol):
		return errors.Join(ErrUnsupportedProtocol, err)
	default:
		return err
	}
}

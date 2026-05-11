package server

import (
	"context"
	"errors"
	"fmt"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
	"github.com/hiddeco/go-ls-remote/internal/objstore"
	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/transport"
)

// ErrUnsupportedProtocol is returned by [Serve] when
// [Options.PreferredProtocol] is not one of [transport.ProtocolV0] or
// [transport.ProtocolV2]. The `v1` prefix is effectively unused in the
// canonical Git ecosystem — `serve.c` and `upload-pack.c` advertise
// `v0` and `v2` only — so this emulator does not implement it.
var ErrUnsupportedProtocol = errors.New("server: unsupported preferred protocol")

// Serve runs an in-process upload-pack session on the given pkt-line
// reader and writer. It opens with a discovery-time advertisement
// matching opts.PreferredProtocol; when that protocol is v2, it then
// services the v2 command-request loop until the client terminates
// the session.
//
// For [transport.ProtocolV2] the advertisement begins with a single
// data packet whose payload is `version 2\n` (see canonical Git's
// `serve.c::protocol_v2_advertise_capabilities` and
// `gitprotocol-v2.adoc` §"Capability Advertisement"), followed by one
// data packet per advertised capability and a trailing flush. The
// capability set and emission order are documented on
// [writeV2Advertisement]. After the advertisement, Serve enters the
// command-request loop driven by `runV2CommandLoop`, dispatching each
// `ls-refs` or `object-info` request to its handler. Three exit paths
// return nil: an empty request (a bare flush before any `command=`),
// a clean stream-close between requests, and a normal completion of
// every request the client sent. An unknown command surfaces a
// structured `ERR command not supported` pkt-line on the wire and
// returns a wrapped [github.com/hiddeco/go-ls-remote/internal/wire.ErrServerRefused]
// so callers can detect the protocol-level refusal without re-decoding
// the response stream.
//
// For [transport.ProtocolV0] the advertisement is the canonical
// reference-discovery stream: HEAD (when valid) followed by every
// ref in C-locale byte order, with peeled lines for annotated tags
// and a trailing flush. An empty repository emits the
// `<zero-oid> capabilities^{}\0<caps>\n` placeholder defined at
// `upload-pack.c:1422-1428`. The cap list and emission rules are
// documented on [writeV0Advertisement]. v0 has no command loop:
// canonical Git terminates the connection after the advertisement
// when the client does not send a `want`, and Serve mirrors that by
// returning immediately.
//
// Any other value of opts.PreferredProtocol returns
// [ErrUnsupportedProtocol] without emitting any bytes on w.
//
// The store argument sources the `object-format` capability value
// today and will source ref enumeration and object metadata in later
// iterations. Passing a nil store is not permitted.
func Serve[H objfmt.Hash](ctx context.Context, r *pktline.Reader, w *pktline.Writer,
	store *objstore.Store[H], opts Options) error {
	switch opts.PreferredProtocol {
	case transport.ProtocolV2:
		if err := writeV2Advertisement(w, store, opts); err != nil {
			return err
		}
		return runV2CommandLoop(ctx, r, w, store, opts)
	case transport.ProtocolV0:
		return writeV0Advertisement(w, store, opts)
	default:
		return fmt.Errorf("%w: %s", ErrUnsupportedProtocol, opts.PreferredProtocol)
	}
}

// ServeCommandLoop runs the v2 command-request loop on r/w against
// store, without emitting the leading advertisement that [Serve]
// would otherwise produce. It is the right entry point for the
// smart-HTTP POST handler, which must return only the command
// response — the advertisement is served by the GET probe. See
// canonical Git's `http-backend.c::service_rpc` for the split.
//
// The function is v2-only by definition: v0 has no command loop, so
// `opts.PreferredProtocol` is ignored. Callers may leave the field
// unset; the response body begins with the first packet the v2
// command handler emits, not with `version 2\n`.
//
// The contract for r, w, store, and the rest of opts (notably
// [Options.Tracer]) matches [Serve]; consult that function's doc
// for the termination paths and error shapes the loop surfaces.
func ServeCommandLoop[H objfmt.Hash](ctx context.Context, r *pktline.Reader, w *pktline.Writer,
	store *objstore.Store[H], opts Options) error {
	return runV2CommandLoop(ctx, r, w, store, opts)
}

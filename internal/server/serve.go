package server

import (
	"context"
	"errors"
	"fmt"

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
// matching opts.PreferredProtocol and (in later iterations of the
// emulator) services the v2 command loop until the client closes the
// stream cleanly.
//
// For [transport.ProtocolV2] the advertisement begins with a single
// data packet whose payload is `version 2\n` (see canonical Git's
// `serve.c::protocol_v2_advertise_capabilities` and
// `gitprotocol-v2.adoc` §"Capability Advertisement"), followed by one
// data packet per advertised capability and a trailing flush. The
// capability set and emission order are documented on
// [writeV2Advertisement].
//
// For [transport.ProtocolV0] the advertisement is the empty ref list
// terminator — a single flush — pending the empty-repo placeholder
// and ref-list emission of a follow-up iteration.
//
// Any other value of opts.PreferredProtocol returns
// [ErrUnsupportedProtocol] without emitting any bytes on w.
//
// The store argument sources the `object-format` capability value
// today and will source ref enumeration and object metadata in later
// iterations. Passing a nil store is not permitted.
func Serve(ctx context.Context, r *pktline.Reader, w *pktline.Writer,
	store *objstore.Store, opts Options) error {
	_ = ctx
	_ = r

	switch opts.PreferredProtocol {
	case transport.ProtocolV2:
		return writeV2Advertisement(w, store, opts)
	case transport.ProtocolV0:
		if err := w.WriteFlush(); err != nil {
			return fmt.Errorf("server: write v0 advertisement flush: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("%w: %s", ErrUnsupportedProtocol, opts.PreferredProtocol)
	}
}

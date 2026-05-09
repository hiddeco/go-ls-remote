package server

import (
	"fmt"
	"io"

	"github.com/hiddeco/go-ls-remote/internal/objstore"
	"github.com/hiddeco/go-ls-remote/pktline"
)

// handleObjectInfo services a v2 `object-info` command request. The
// dispatch loop has already consumed the `command=object-info` line
// and the trailing delim of the capability section; the handler reads
// the command-args section up to the terminating flush
// (`serve.c::process_request` lines 323-329 and
// `object-info.c::cmd_object_info`) and writes the per-oid metadata
// section followed by a flush.
//
// The current body is a stub: it drains command-args without parsing
// them and emits a bare flush as the response. A later iteration
// replaces the body with the metadata-emission logic; the signature
// is stable so callers and the dispatcher do not change.
func handleObjectInfo(r *pktline.Reader, w *pktline.Writer,
	store *objstore.Store, opts Options) error {
	_ = store
	_ = opts
	if err := drainArgs(r); err != nil {
		return fmt.Errorf("server: object-info: drain args: %w", err)
	}
	if err := w.WriteFlush(); err != nil {
		return fmt.Errorf("server: object-info: write flush: %w", err)
	}
	return nil
}

// drainArgs reads pkt-lines from r until the terminating flush of a
// v2 command-args section. The canonical command handlers (e.g.
// `object-info.c::object_info`) consume args one at a time and stop on
// `PACKET_READ_FLUSH`; the stub mirrors that loop without interpreting
// any argument.
//
// A non-EOF error is wrapped; an unexpected EOF mid-args is reported
// as an [io.ErrUnexpectedEOF] wrap so callers can distinguish a
// truncated request from a clean stream close.
//
// The `ls-refs` handler used to call this helper too; once it grew real
// argument parsing, the helper was moved here. A subsequent iteration
// of `handleObjectInfo` will replace its sole call site, after which
// `drainArgs` can be deleted entirely.
func drainArgs(r *pktline.Reader) error {
	for {
		pkt, err := r.ReadPacket()
		if err != nil {
			if err == io.EOF {
				return io.ErrUnexpectedEOF
			}
			return err
		}
		if pkt.Kind == pktline.Flush {
			return nil
		}
		// Other kinds (Data, Delim, ResponseEnd) are silently
		// consumed; the stub does not interpret arg payloads.
	}
}

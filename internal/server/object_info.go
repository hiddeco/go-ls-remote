package server

import (
	"fmt"

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

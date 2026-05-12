package filet

import (
	"context"
	"errors"
	"io"

	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/transport"
)

// Command issues a v2 command and returns a [pktline.Reader] over the
// response. The returned reader is the same one [Conn.Advertisement]
// returns: the local-filesystem transport keeps a single in-process
// server goroutine attached to one pkt-line pipe pair for the entire
// session, and every command's response streams back on that pair. The
// caller therefore sees a single persistent stream whose packets are
// segmented by the canonical v2 command-response framing
// ([gitprotocol-v2.adoc §"Command Response"]).
//
// # Concurrency
//
// [Conn] is single-flight per the [transport.Conn] contract. Callers
// must drain the response of any prior command — through to its
// trailing flush or response-end — before invoking Command again. The
// in-process server reads the next request only after writing the
// previous response, so a request issued before the previous response
// is consumed will deadlock the goroutine against the unread pipe
// buffer.
//
// # Errors
//
// body is invoked exactly once against the [Conn]'s [pktline.Writer]
// and must encode the canonical v2 command-request frame. A non-nil
// return aborts the call: errors from a [pktline.Writer.WritePacket]
// payload-cap rejection wrap [pktline.ErrPayloadTooLarge] (the
// canonical cap defined at [pkt-line.h:234]); pipe-write failures
// surface as `*ProtocolError{Op: "command"}`. A write that fails with
// [io.ErrClosedPipe] indicates the server goroutine has exited; the
// returned error wraps the goroutine's own error (captured by the
// open-time `Serve` shutdown sequence) so callers can match the root
// cause via [errors.Is]. After any Command error the [Conn] is
// effectively dead — the goroutine is gone or about to be — and
// callers must invoke [Conn.Close] to release the pipes and the
// underlying object store.
//
// [gitprotocol-v2.adoc §"Command Response"]: https://github.com/git/git/blob/v2.54.0/Documentation/gitprotocol-v2.adoc#command-request
// [pkt-line.h:234]: https://github.com/git/git/blob/v2.54.0/pkt-line.h#L234
func (c *Conn[H]) Command(ctx context.Context, _ string, body transport.CommandBody) (*pktline.Reader, error) {
	// Honour cancellation up-front: the in-process server reads from a
	// memory pipe so a `ctx` deadline cannot be plumbed through the
	// write itself, but a caller who has already cancelled before
	// reaching here expects the cancellation to dominate.
	if err := ctx.Err(); err != nil {
		return nil, &ProtocolError{Op: "command", Err: err}
	}
	if err := body(c.writer); err != nil {
		return nil, c.wrapWriteError(err)
	}
	return c.reader, nil
}

// wrapWriteError maps a pipe-write error from [Conn.Command]'s request
// emission to a `*ProtocolError{Op: "command"}`. When the underlying
// failure is [io.ErrClosedPipe] — the canonical signal that the server
// goroutine has shut its end of the pipe — the wrapped cause is
// upgraded to the goroutine's own `Serve` error if one is available.
//
// The `Serve` error is read through [Conn.serverError], which performs
// the non-blocking select on `c.done` that makes the access
// synchronisation-safe. If the goroutine is still running the closed
// pipe is coming from elsewhere (e.g. a concurrent Close) and the
// original write error is surfaced unchanged.
func (c *Conn[H]) wrapWriteError(err error) error {
	if errors.Is(err, io.ErrClosedPipe) {
		if srvErr := c.serverError(); srvErr != nil {
			return &ProtocolError{Op: "command", Err: srvErr}
		}
	}
	return &ProtocolError{Op: "command", Err: err}
}

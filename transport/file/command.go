package filet

import (
	"context"
	"errors"
	"io"

	"github.com/hiddeco/go-ls-remote/internal/wire"
	"github.com/hiddeco/go-ls-remote/pktline"
)

// Command issues a v2 command and returns a [pktline.Reader] over the
// response. The returned reader is the same one [Conn.Advertisement]
// returns: the local-filesystem transport keeps a single in-process
// server goroutine attached to one pkt-line pipe pair for the entire
// session, and every command's response streams back on that pair. The
// caller therefore sees a single persistent stream whose packets are
// segmented by the canonical v2 command-response framing
// (`gitprotocol-v2.adoc` §"Command Response").
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
// Per-input validation rejects payloads whose pkt-line framing would
// exceed [pktline.MaxPayload] before any byte hits the pipe (the
// canonical cap defined at `pkt-line.h:234`). Such errors wrap
// [pktline.ErrPayloadTooLarge] for [errors.Is].
//
// Pipe-write failures during request emission map to
// `*ProtocolError{Op: "command"}`. A write that fails with
// [io.ErrClosedPipe] indicates the server goroutine has exited; the
// returned error wraps the goroutine's own error (captured by the
// open-time `Serve` shutdown sequence) so callers can match the root
// cause via [errors.Is]. After any Command error the [Conn] is
// effectively dead — the goroutine is gone or about to be — and
// callers must invoke [Conn.Close] to release the pipes and the
// underlying object store.
func (c *Conn) Command(ctx context.Context, name string, args, caps []string) (*pktline.Reader, error) {
	// Honour cancellation up-front: the in-process server reads from a
	// memory pipe so a `ctx` deadline cannot be plumbed through the
	// write itself, but a caller who has already cancelled before
	// reaching here expects the cancellation to dominate.
	if err := ctx.Err(); err != nil {
		return nil, &ProtocolError{Op: "command", Err: err}
	}
	if err := wire.ValidateV2CommandPayloads(name, args, caps); err != nil {
		return nil, err
	}

	if err := wire.EncodeV2CommandRequest(c.writer, name, args, caps); err != nil {
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
// The `Serve` error is captured by the open-time goroutine before it
// closes `c.done`, so a non-blocking peek through the channel is
// synchronisation-safe: a closed `done` happens-before the `serverErr`
// read here, and an open `done` means the goroutine is still running
// and the closed-pipe signal must be coming from elsewhere (e.g. a
// concurrent Close).
func (c *Conn) wrapWriteError(err error) error {
	if errors.Is(err, io.ErrClosedPipe) {
		select {
		case <-c.done:
			if c.serverErr != nil {
				return &ProtocolError{Op: "command", Err: c.serverErr}
			}
		default:
		}
	}
	return &ProtocolError{Op: "command", Err: err}
}

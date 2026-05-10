package filet

import (
	"context"
	"errors"
	"fmt"
	"io"

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
// canonical cap defined at `pkt-line.h:118`). Such errors wrap
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
	if err := validateCommandPayloads(name, args, caps); err != nil {
		return nil, err
	}

	if err := encodeV2CommandRequest(c.writer, name, args, caps); err != nil {
		return nil, c.wrapWriteError(err)
	}
	return c.reader, nil
}

// encodeV2CommandRequest writes a v2 command-request frame to w using
// the canonical layout from `gitprotocol-v2.adoc` §"Command Request"
// and matched by `serve.c::process_request`:
//
//	command-request = command-line *capability-line delim-pkt *arg-line flush-pkt
//	command-line    = PKT-LINE("command=" cmd LF)
//	capability-line = PKT-LINE(cap LF)
//	arg-line        = PKT-LINE(arg LF)
//
// Trailing LF on each line matches `packet_write_fmt(... "\n")` in
// canonical Git's writer. A write failure short-circuits the rest of
// the frame: the underlying [pktline.Writer] writes each pkt-line in a
// single [io.Writer.Write] call, so a partial frame on the wire is
// possible only when the underlying sink itself fails mid-frame, in
// which case there is nothing more this side can usefully emit.
func encodeV2CommandRequest(w *pktline.Writer, name string, args, caps []string) error {
	if err := w.WritePacket([]byte("command=" + name + "\n")); err != nil {
		return err
	}
	for _, cap := range caps {
		if err := w.WritePacket([]byte(cap + "\n")); err != nil {
			return err
		}
	}
	if err := w.WriteDelim(); err != nil {
		return err
	}
	for _, a := range args {
		if err := w.WritePacket([]byte(a + "\n")); err != nil {
			return err
		}
	}
	return w.WriteFlush()
}

// validateCommandPayloads rejects command-name, capability, and arg
// inputs whose pkt-line framing would exceed [pktline.MaxPayload].
// Each pkt-line carries the input value plus a trailing LF (and the
// `command=` prefix for the command name); a value above the cap
// cannot be framed as a single packet, so refuse before constructing
// the frame. Returns an error wrapping [pktline.ErrPayloadTooLarge]
// so callers can match with [errors.Is].
func validateCommandPayloads(name string, args, caps []string) error {
	const commandPrefix = "command="
	if n := len(commandPrefix) + len(name) + 1; n > pktline.MaxPayload {
		return fmt.Errorf("transport/file: command %q payload %d bytes: %w",
			name, n, pktline.ErrPayloadTooLarge)
	}
	for _, c := range caps {
		if n := len(c) + 1; n > pktline.MaxPayload {
			return fmt.Errorf("transport/file: capability payload %d bytes: %w",
				n, pktline.ErrPayloadTooLarge)
		}
	}
	for _, a := range args {
		if n := len(a) + 1; n > pktline.MaxPayload {
			return fmt.Errorf("transport/file: argument payload %d bytes: %w",
				n, pktline.ErrPayloadTooLarge)
		}
	}
	return nil
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

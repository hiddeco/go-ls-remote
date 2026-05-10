package filet

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/hiddeco/go-ls-remote/internal/objstore"
	"github.com/hiddeco/go-ls-remote/pktline"
)

// Conn is the local-filesystem [transport.Conn]. It is constructed by
// [Transport.Open] once the on-disk repository has opened successfully:
// a goroutine running `internal/server.Serve` is already producing the
// advertisement on the in-memory pipe the wrapped [pktline.Reader]
// reads from, with the [pktline.Writer] mirroring the client's
// commands back to the server.
//
// Conn is single-flight per the [transport.Conn] contract: while the
// reader returned from [Conn.Advertisement] is open, callers must not
// invoke [Conn.Command]. The local-filesystem transport has no
// natural multiplexing seam — a single in-process server goroutine
// owns both pipe ends — so the contract is enforced trivially.
//
// # Lifecycle
//
// `Open` spawns one goroutine running `server.Serve` against the
// server-side pipe ends. `Close` cancels the derived context, closes
// every pipe end the goroutine could be blocked on, waits for the
// goroutine to return, and then closes the underlying
// [github.com/hiddeco/go-ls-remote/internal/objstore.Store]. The two
// independent `io.Pipe` pairs (one per direction) ensure neither side
// can deadlock by writing into a closed peer.
type Conn struct {
	// reader decodes pkt-lines coming back from the server goroutine.
	// Its underlying source is the client-side end of the
	// server-to-client pipe; the goroutine writes its advertisement
	// and command responses there.
	reader *pktline.Reader

	// writer encodes pkt-lines bound for the server goroutine. Its
	// underlying sink is the client-side end of the client-to-server
	// pipe; the goroutine reads its command requests from the
	// matching server end.
	writer *pktline.Writer

	// store is the on-disk object store the goroutine serves.
	// Ownership transfers from `Open` to the [Conn]; `Close` releases
	// it as part of the lifecycle cascade.
	store *objstore.Store

	// cancel cancels the context the goroutine runs under. `Close`
	// invokes it before closing the pipe ends so the goroutine has a
	// chance to observe cancellation through `ctx.Err()` ahead of any
	// pipe-induced read or write error.
	cancel context.CancelFunc

	// done is closed by the spawned goroutine once `server.Serve`
	// returns. `Close` waits on it so a returned [Conn] can never
	// outlive the goroutine — no leaked goroutine survives a
	// completed `Close`.
	done chan struct{}

	// serverErr captures the error `server.Serve` returned. It is
	// written by the spawned goroutine before the deferred
	// `close(c.done)` fires, so any read that synchronises on
	// `<-c.done` (today: [Conn.Close]) sees the final value without
	// further locking. Reads off the synchronisation path will need
	// to introduce one in a follow-up commit.
	serverErr error

	// clientReader and clientWriter are the client-side pipe ends.
	// They are owned by the [Conn] and closed in [Conn.Close]; the
	// matching server-side ends are owned by the goroutine.
	clientReader *io.PipeReader
	clientWriter *io.PipeWriter

	// serverReader and serverWriter are the server-side pipe ends. The
	// goroutine reads from `serverReader` and writes to `serverWriter`;
	// [Conn.Close] closes both so any read or write the goroutine is
	// blocked on returns immediately.
	serverReader *io.PipeReader
	serverWriter *io.PipeWriter

	// closeOnce guards the [Conn.Close] body so a second or later
	// invocation is a no-op, matching the [transport.Conn]
	// idempotent-close contract. closeErr stores the result of the
	// first invocation so the very first call returns it.
	closeOnce sync.Once
	closeErr  error
}

// Advertisement returns the cached pkt-line reader. The reader is
// positioned at the first byte of the advertisement; for v2 that is
// the `version 2\n` data packet, for v0 it is the first ref line.
func (c *Conn) Advertisement() *pktline.Reader {
	return c.reader
}

// Close cancels the derived context, closes every pipe end so the
// goroutine cannot block on a peer that no longer reads, waits for
// the goroutine to return, and then closes the underlying object
// store. Errors from the store-close cascade are joined with any
// error captured from `server.Serve` and returned. Subsequent calls
// are no-ops that return nil, matching the [transport.Conn]
// idempotent-close contract.
//
// # Ordering
//
// The cancel-then-close-pipes ordering is deliberate. Cancellation
// signals the goroutine to wind down via `ctx.Err()`; closing the
// pipes immediately after ensures any blocking read or write the
// goroutine is parked on returns promptly with `io.ErrClosedPipe`
// (or, post-close, `io.EOF`). Closing pipes first would race the
// goroutine's pkt-line writes against the close, surfacing pipe
// errors that are not actionable from the caller's perspective.
func (c *Conn) Close() error {
	first := false
	c.closeOnce.Do(func() {
		first = true

		c.cancel()

		// Close pipe ends from both sides so neither direction can
		// deadlock the goroutine. Closing a `*io.PipeReader` returns
		// `io.ErrClosedPipe` on subsequent writes; closing a
		// `*io.PipeWriter` returns `io.EOF` on subsequent reads.
		_ = c.clientReader.Close()
		_ = c.clientWriter.Close()
		_ = c.serverReader.Close()
		_ = c.serverWriter.Close()

		<-c.done

		// `context.Canceled` and `io.ErrClosedPipe` are the expected
		// shutdown shapes: the goroutine observed either the cancelled
		// context or the closed pipes the cancel-then-close sequence
		// just installed. Surface only genuinely unexpected errors so
		// `Close` is silent on a clean teardown.
		var errs []error
		if c.serverErr != nil &&
			!errors.Is(c.serverErr, context.Canceled) &&
			!errors.Is(c.serverErr, io.ErrClosedPipe) {
			errs = append(errs, c.serverErr)
		}

		if c.store != nil {
			if err := c.store.Close(); err != nil {
				errs = append(errs, err)
			}
		}
		c.closeErr = errors.Join(errs...)
	})
	if !first {
		return nil
	}
	return c.closeErr
}

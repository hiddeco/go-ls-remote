package ssht

import (
	"context"
	"errors"
	"io"
	"sync"

	"golang.org/x/crypto/ssh"

	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/transport"
)

// Conn is the SSH-backed [transport.Conn]. It is constructed by
// [Transport.Open] once the underlying TCP dial, SSH handshake, and
// channel negotiation have succeeded. Field order clusters the
// pointer-shaped fields ahead of the `sync.Once` and the trailing
// error so the struct packs without padding on 64-bit platforms.
//
// Lifecycle: ownership of `client`, `session`, the stdout-side
// [*pktline.Reader], and the [*pktline.Writer] wrapping the session's
// stdin transfers from [Transport.Open] to the [Conn]; [Conn.Close]
// tears them down in reverse order. A goroutine spawned by
// [Transport.Open] runs `session.Wait` for the lifetime of the [Conn]
// and stores its result in `waitErr`; the goroutine signals
// completion by closing `done`. Stderr is discarded for this
// iteration: in a healthy session the remote `git-upload-pack` writes
// nothing to it, and a diagnostic surfaces on the next read of stdout
// anyway. A future tracer integration may drain stderr into a tracer
// event.
type Conn struct {
	// client is the SSH client connection. Closing it drops the
	// underlying TCP connection.
	client *ssh.Client

	// session is the SSH session channel hosting the remote
	// `git-upload-pack` command. Closing it sends an EOF on stdin and
	// reaps the remote command.
	session *ssh.Session

	// reader decodes pkt-lines streamed from the session's stdout.
	reader *pktline.Reader

	// writer encodes pkt-lines onto the session's stdin. Constructed
	// in [Transport.Open] to emit the initial pkt-line and reused by
	// [Conn.Command] for every subsequent v2 command request. The
	// underlying [io.WriteCloser] is owned by the wrapped
	// [pktline.Writer]; the [Conn] does not retain a separate reference
	// because `session.Close` already closes the channel that backs it.
	writer *pktline.Writer

	// redactedURL is the dial URL with credentials redacted via
	// [transport.RedactURL]. Captured at [Transport.Open] so
	// [Conn.Command] can stamp it on every `*ProtocolError` it
	// returns, mirroring the dial-time error envelope and matching the
	// HTTP transport's `Conn.url` pattern.
	redactedURL string

	// done is closed by the `session.Wait` goroutine once the remote
	// `git-upload-pack` has exited. `waitErr` captures that exit
	// status. Reads MUST go through [Conn.sessionError] (which selects
	// on `done` non-blockingly) or run after [Conn.Close]'s wait has
	// returned to honour the goroutine's happens-before edge.
	done    chan struct{}
	waitErr error

	// closeOnce guards [Conn.Close] so a second or later invocation
	// is a no-op, matching the [transport.Conn] idempotent contract.
	// closeErr captures the result of the first invocation so the
	// very first call surfaces it.
	closeOnce sync.Once
	closeErr  error
}

// Advertisement returns the cached pkt-line reader over the SSH
// session's stdout. The reader is positioned at the first byte of the
// advertisement; for v2 that is the `version 2\n` data packet, for v0
// it is the first ref line.
func (c *Conn) Advertisement() *pktline.Reader { return c.reader }

// Command issues a v2 command and returns a [pktline.Reader] over the
// response. The returned reader is the same one [Conn.Advertisement]
// returns: the SSH transport keeps a single remote `git-upload-pack`
// session attached to one pkt-line pipe pair for the entire
// connection, and every command's response streams back on that pair.
// The caller therefore sees a single persistent stream whose packets
// are segmented by the canonical v2 command-response framing
// (`gitprotocol-v2.adoc` §"Command Response").
//
// # Concurrency
//
// [Conn] is single-flight per the [transport.Conn] contract. Callers
// must drain the response of any prior command — through to its
// trailing flush or response-end — before invoking Command again. The
// SSH session has no natural multiplexing seam (one exec channel, one
// pkt-line pipe pair), so the contract is enforced trivially.
//
// # Errors
//
// body is invoked exactly once against the [Conn]'s [pktline.Writer]
// and must encode the canonical v2 command-request frame. A non-nil
// return aborts the call: errors from a [pktline.Writer.WritePacket]
// payload-cap rejection wrap [pktline.ErrPayloadTooLarge] (the
// canonical cap defined at `pkt-line.h:234`); pipe-write failures
// surface as `*ProtocolError{Op: "command"}`. A write that fails with
// [io.ErrClosedPipe] indicates the remote `git-upload-pack` has shut
// its end of the pipe (or [Conn.Close] has closed it locally); when
// the session has reported an exit status, the wrapped cause is
// upgraded to that exit error so callers can match the root cause via
// [errors.Is]. After any Command error the [Conn] is effectively
// dead and callers must invoke [Conn.Close] to release resources.
func (c *Conn) Command(ctx context.Context, _ string, body transport.CommandBody) (*pktline.Reader, error) {
	// Honour cancellation up-front: the SSH session has no per-write
	// context plumbing, but a caller who has already cancelled before
	// reaching here expects the cancellation to dominate.
	if err := ctx.Err(); err != nil {
		return nil, &ProtocolError{URL: c.redactedURL, Op: "command", Err: err}
	}
	if err := body(c.writer); err != nil {
		return nil, c.wrapWriteError(err)
	}
	return c.reader, nil
}

// wrapWriteError maps a pipe-write error from [Conn.Command]'s request
// emission to a `*ProtocolError{Op: "command"}`. When the underlying
// failure is [io.ErrClosedPipe] — the canonical signal that the
// session's stdin write half has closed — the wrapped cause is
// upgraded to the session-exit error from `session.Wait` if one is
// available.
//
// The session-exit error is read through [Conn.sessionError], whose
// non-blocking select on `c.done` makes the access
// synchronisation-safe. If the session is still running the closed
// pipe is coming from elsewhere (a local [Conn.Close] racing the
// write) and the original write error is surfaced unchanged.
func (c *Conn) wrapWriteError(err error) error {
	if errors.Is(err, io.ErrClosedPipe) {
		if sessErr := c.sessionError(); sessErr != nil {
			return &ProtocolError{URL: c.redactedURL, Op: "command", Err: sessErr}
		}
	}
	return &ProtocolError{URL: c.redactedURL, Op: "command", Err: err}
}

// sessionError returns the error captured from `session.Wait`, if the
// session has exited. The waiter goroutine assigns `c.waitErr` before
// its deferred `close(c.done)` fires; under Go's memory model that
// write happens-before any subsequent `<-c.done` receive, so reading
// the field after observing the closed channel is safe.
//
// The select on `c.done` is non-blocking: if the session is still
// running, the method returns nil without waiting. Callers that need
// to distinguish "session exited" from "session still running" must
// select on `c.done` themselves.
func (c *Conn) sessionError() error {
	select {
	case <-c.done:
		return c.waitErr
	default:
		return nil
	}
}

// Close releases the SSH session and the underlying client. It is
// idempotent: a second or later invocation is a no-op returning nil,
// per the [transport.Conn] contract. Errors from the session and
// client closes are joined; the "normal shutdown" shapes `io.EOF` and
// the literal "session closed" message x/crypto/ssh emits on a
// race-with-EOF are filtered because they are not actionable. The
// session-wait goroutine is awaited so no goroutine survives a
// completed Close.
func (c *Conn) Close() error {
	first := false
	c.closeOnce.Do(func() {
		first = true

		var errs []error
		if c.session != nil {
			if err := c.session.Close(); err != nil && !isExpectedCloseError(err) {
				errs = append(errs, err)
			}
		}
		if c.client != nil {
			if err := c.client.Close(); err != nil && !isExpectedCloseError(err) {
				errs = append(errs, err)
			}
		}
		// Wait for the `session.Wait` goroutine to return so no
		// goroutine survives Close. The session-close above unblocks
		// `Wait` if it has not already returned of its own accord.
		if c.done != nil {
			<-c.done
		}
		c.closeErr = errors.Join(errs...)
	})
	if !first {
		return nil
	}
	return c.closeErr
}

// isExpectedCloseError reports whether err is a known "post-shutdown"
// shape from `*ssh.Session.Close` or `*ssh.Client.Close`. Both can
// return `io.EOF` when the peer has already closed its side, and the
// session's underlying channel returns the literal `"EOF"` message in
// some race shapes. Filtering these here keeps [Conn.Close]'s reported
// error focused on genuinely unexpected failures.
func isExpectedCloseError(err error) bool {
	if err == nil {
		return true
	}
	if errors.Is(err, io.EOF) {
		return true
	}
	return false
}

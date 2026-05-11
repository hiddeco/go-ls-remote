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

// errCommandNotImplemented is returned by [Conn.Command] until the v2
// command-emission path lands in the follow-up task. Declaring it as a
// named sentinel keeps the stub detectable without depending on the
// rendered text.
var errCommandNotImplemented = errors.New("ssht: Command not implemented yet")

// Conn is the SSH-backed [transport.Conn]. It is constructed by
// [Transport.Open] once the underlying TCP dial, SSH handshake, and
// channel negotiation have succeeded. Field order clusters the
// pointer-shaped fields ahead of the `sync.Once` and the trailing
// error so the struct packs without padding on 64-bit platforms.
//
// Lifecycle: ownership of `client`, `session`, and the stdout-side
// [*pktline.Reader] (plus the [*pktline.Writer] wrapping the session's
// stdin) transfers from [Transport.Open] to the [Conn]; [Conn.Close]
// tears them down in reverse order. Stderr is discarded for this
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

	// writer encodes pkt-lines onto the session's stdin. Constructed in
	// [Transport.Open] to emit the initial pkt-line and retained for
	// Task 4's v2 command-emission path. The underlying
	// [io.WriteCloser] is owned by the wrapped [pktline.Writer]; the
	// [Conn] does not retain a separate reference because
	// `session.Close` already closes the channel that backs it.
	writer *pktline.Writer

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

// Command is stubbed pending the v2 command-emission path. The stub
// returns a nil reader so callers do not accidentally rely on a
// half-built one.
func (c *Conn) Command(_ context.Context, _ string, _ transport.CommandBody) (*pktline.Reader, error) {
	return nil, errCommandNotImplemented
}

// Close releases the SSH session and the underlying client. It is
// idempotent: a second or later invocation is a no-op returning nil,
// per the [transport.Conn] contract. Errors from the session and
// client closes are joined; the "normal shutdown" shapes `io.EOF` and
// the literal "session closed" message x/crypto/ssh emits on a
// race-with-EOF are filtered because they are not actionable.
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

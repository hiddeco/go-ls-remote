package gitt

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"

	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/transport"
)

// Conn is the git-daemon-backed [transport.Conn]. It is constructed by
// [Transport.Open] once the TCP dial and initial pkt-line have been
// sent. Field order clusters the pointer-shaped fields ahead of the
// `sync.Once` and trailing error so the struct packs without padding on
// 64-bit platforms.
//
// Lifecycle: ownership of the [net.Conn], the [*pktline.Reader], and
// the [*pktline.Writer] transfers from [Transport.Open] to the [Conn];
// [Conn.Close] tears them down by closing the [net.Conn] (which causes
// both the reader and writer to fail with a net-closed error on the
// next I/O call).
type Conn struct {
	// conn is the underlying TCP connection to the git-daemon.
	conn net.Conn

	// reader decodes pkt-lines streamed from the server. Both the
	// initial advertisement and command responses flow on this reader.
	reader *pktline.Reader

	// writer encodes pkt-lines onto the TCP connection. The initial
	// request pkt-line is written by [Transport.Open]; subsequent
	// command-request frames are written by [Conn.Command].
	writer *pktline.Writer

	// redactedURL is the dial URL with credentials redacted via
	// [transport.RedactURL]. Captured at [Transport.Open] so
	// [Conn.Command] can stamp it on every `*ProtocolError` it
	// returns.
	redactedURL string

	// closeOnce guards [Conn.Close] so a second or later invocation
	// is a no-op, matching the [transport.Conn] idempotent contract.
	// closeErr captures the result of the first invocation so the very
	// first call can surface it.
	closeOnce sync.Once
	closeErr  error
}

// Advertisement returns the cached pkt-line reader over the TCP
// connection. The reader is positioned at the first byte of the
// advertisement; for v2 that is the `version 2\n` data packet, for v0
// it is the first ref line.
func (c *Conn) Advertisement() *pktline.Reader { return c.reader }

// Command issues a v2 command and returns a [pktline.Reader] over the
// response. The single TCP connection carries every command sequentially,
// so callers must drain the response of any prior command before invoking
// Command again.
//
// body is invoked exactly once against the [Conn]'s [pktline.Writer]
// and must encode the canonical v2 command-request frame. On success
// the same reader returned by [Conn.Advertisement] is returned.
//
// Note: the exact error envelope (sentinel mapping, ctx-cancel
// handling) is hardened in the next commit; this stub satisfies the
// [transport.Conn] interface.
func (c *Conn) Command(ctx context.Context, _ string, body transport.CommandBody) (*pktline.Reader, error) {
	if err := ctx.Err(); err != nil {
		return nil, &ProtocolError{URL: c.redactedURL, Op: "command", Err: err}
	}
	if err := body(c.writer); err != nil {
		return nil, &ProtocolError{URL: c.redactedURL, Op: "command", Err: err}
	}
	return c.reader, nil
}

// Close releases the underlying TCP connection. It is idempotent: a
// second or later invocation is a no-op returning nil, per the
// [transport.Conn] contract. [io.EOF] is filtered because the
// peer-closed signal is not actionable.
func (c *Conn) Close() error {
	first := false
	c.closeOnce.Do(func() {
		first = true
		if c.conn != nil {
			if err := c.conn.Close(); err != nil && !errors.Is(err, io.EOF) {
				c.closeErr = err
			}
		}
	})
	if !first {
		return nil
	}
	return c.closeErr
}

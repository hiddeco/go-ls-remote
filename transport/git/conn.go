package gitt

import (
	"context"
	"errors"
	"fmt"
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
// # Concurrency
//
// Conn is single-flight per the [transport.Conn] contract. While a
// [*pktline.Reader] returned from [Conn.Advertisement] or [Conn.Command]
// is still open, callers must not invoke [Conn.Command] again on the
// same Conn: the response of the prior call must be drained through to
// its trailing flush first. The single TCP connection carries every
// command sequentially with no multiplexing seam, so the contract is
// enforced trivially — overlapping reads and writes would interleave
// frames against the same stream.
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
// response. The returned reader is the same one [Conn.Advertisement]
// returns: the git-daemon transport keeps a single TCP connection
// attached to one pkt-line stream for the entire session, and every
// command's response streams back on that connection. The caller
// therefore sees a single persistent stream whose packets are
// segmented by the canonical v2 command-response framing
// (`gitprotocol-v2.adoc` §"Command Response").
//
// # Concurrency
//
// [Conn] is single-flight per the [transport.Conn] contract. Callers
// must drain the response of any prior command — through to its
// trailing flush — before invoking Command again. The TCP connection
// has no multiplexing seam, so the contract is enforced trivially.
//
// # Errors
//
// body is invoked exactly once against the [Conn]'s [pktline.Writer]
// and must encode the canonical v2 command-request frame
// (`gitprotocol-v2.adoc` §"Command Request"). A non-nil return aborts
// the call: all write failures — whether from a payload-cap rejection
// (canonical cap at `pkt-line.h:234`) or a TCP-write error — flow
// through `wrapWriteError` and surface as `*ProtocolError{Op: "command"}`
// with the `"gitt: write command request: ..."` prefix; the
// `errors.Is` chain to `pktline.ErrPayloadTooLarge` is preserved
// through the wrap. After any Command error the [Conn] is effectively
// dead and callers must invoke [Conn.Close] to release resources.
func (c *Conn) Command(ctx context.Context, _ string, body transport.CommandBody) (*pktline.Reader, error) {
	// Honour cancellation up-front: the TCP connection has no
	// per-write context plumbing, but a caller who has already
	// cancelled before reaching here expects the cancellation to
	// dominate.
	if err := ctx.Err(); err != nil {
		return nil, &ProtocolError{URL: c.redactedURL, Op: "command", Err: err}
	}
	if err := body(c.writer); err != nil {
		return nil, c.wrapWriteError(err)
	}
	return c.reader, nil
}

// wrapWriteError maps a TCP-write error from [Conn.Command]'s request
// emission to a `*ProtocolError{Op: "command"}` with a stable
// `"gitt: write command request: ..."` prefix so callers scanning log
// lines see a consistent pattern regardless of the underlying failure
// shape (net-closed, EPIPE, ECONNRESET, payload-too-large).
func (c *Conn) wrapWriteError(err error) error {
	return &ProtocolError{
		URL: c.redactedURL,
		Op:  "command",
		Err: fmt.Errorf("gitt: write command request: %w", err),
	}
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

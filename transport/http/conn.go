package httpt

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"

	"github.com/hiddeco/go-ls-remote/pktline"
)

// Conn is the HTTP-transport [transport.Conn]. It is constructed by
// [Transport.Open] once the smart-probe has succeeded and the
// `# service=git-upload-pack` preamble plus its trailing flush have
// been consumed; the wrapped [pktline.Reader] is positioned at the
// first byte of the actual advertisement.
//
// The command path lands in a follow-up change. The fields needed for
// it (cached capabilities, hash-algo, the originating [http.Client],
// and a raw-capabilities accessor) will join this struct then; for
// now [Conn.Command] returns a placeholder error so a misuse surfaces
// a clear message rather than a nil dereference.
//
// Conn is single-flight per the [transport.Conn] contract: while the
// reader returned from [Conn.Advertisement] is open, callers must not
// invoke [Conn.Command]. The HTTP transport could in principle
// multiplex via parallel requests, but that is a Session-layer
// concern; at this layer Conn matches the cross-transport rule.
type Conn struct {
	// body is the response body the probe handed off. Closing the
	// connection drains and closes it; see [Conn.Close].
	body io.ReadCloser

	// reader decodes pkt-lines from body and is positioned past the
	// smart preamble.
	reader *pktline.Reader

	// client is the [http.Client] the probe used. It is retained so
	// the command path (follow-up change) can reuse the same client
	// — and any cookies, transport-level redirect policy, or test
	// hooks attached to it.
	client *http.Client

	// url is the final request URL the probe resolved to (after any
	// redirects). It is retained for tracing and for diagnostic
	// output in errors raised by the command path.
	url string

	// closeOnce guards the [Conn.Close] body so a second or later
	// invocation is a no-op, matching the [transport.Conn]
	// idempotent-close contract.
	closeOnce sync.Once

	// closeErr stores the result of the first [Conn.Close] body so
	// the very first invocation returns it. Subsequent invocations
	// return nil unconditionally to honour the [transport.Conn]
	// no-op-after-first contract.
	closeErr error
}

// Advertisement returns the cached pkt-line reader. The reader is
// positioned at the first byte of the advertisement proper, with
// the `# service=git-upload-pack` preamble and its trailing flush
// already consumed.
func (c *Conn) Advertisement() *pktline.Reader {
	return c.reader
}

// Command is a placeholder. The command path lands in a follow-up
// change; calling Command today returns a descriptive error so a
// misuse surfaces a clear message.
func (c *Conn) Command(_ context.Context, _ string, _, _ []string) (*pktline.Reader, error) {
	return nil, errors.New("transport/http: Command not yet implemented")
}

// Close drains and closes the underlying response body exactly once.
// Subsequent calls are no-ops and return nil, matching the
// [transport.Conn] contract.
func (c *Conn) Close() error {
	first := false
	c.closeOnce.Do(func() {
		first = true
		if c.body == nil {
			return
		}
		// Drain whatever bytes remain so the underlying connection
		// can be reused by the [http.Client]'s connection pool. Cap
		// the drain to avoid pinning memory on a misbehaving server
		// that streams indefinitely; the cap matches what
		// `net/http` itself uses for its discard heuristic.
		_, _ = io.Copy(io.Discard, io.LimitReader(c.body, 1<<16))
		c.closeErr = c.body.Close()
	})
	if !first {
		return nil
	}
	return c.closeErr
}

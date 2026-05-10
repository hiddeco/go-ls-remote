package httpt

import (
	"io"
	"net/http"
	"net/url"
	"sync"

	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/trace"
)

// Conn is the HTTP-transport [transport.Conn]. It is constructed by
// [Transport.Open] once the discovery probe has succeeded: on the
// smart path the `# service=git-upload-pack` preamble plus its
// trailing flush have already been consumed, leaving the wrapped
// [pktline.Reader] positioned at the first byte of the actual
// advertisement; on the dumb path the [pktline.Reader] is the
// synthetic v0-shaped stream produced by `internal/dumbhttp`.
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

	// reader decodes pkt-lines from body. On the smart branch it is
	// positioned past the preamble; on the dumb branch it is the
	// synthesised v0-shaped stream from `internal/dumbhttp`.
	reader *pktline.Reader

	// client is the [http.Client] the probe used. It is retained so
	// command POSTs reuse the same client — and any cookies,
	// transport-level redirect policy, or test hooks attached to it.
	client *http.Client

	// url is the final request URL the probe resolved to (after any
	// redirects). It is retained both for tracing and as the base
	// from which [Conn.Command] derives the per-command POST URL by
	// rewriting the trailing `/info/refs` to `/git-upload-pack`.
	url *url.URL

	// creds resolves credentials for command-time POSTs. nil means
	// anonymous: the command path will not consult a resolver. Captured
	// at [Transport.open] so [Conn.Command] does not have to walk back
	// to a `*Transport` reference.
	creds CredentialResolver

	// userAgent is the User-Agent string command POSTs send, resolved
	// at [Transport.open] from the per-call, per-Transport, and package
	// defaults so [Conn.Command] does not re-resolve.
	userAgent string

	// gitProtocolHeader is the `Git-Protocol` header value command
	// POSTs send, captured at [Transport.open] from the negotiated
	// protocol. Today this is always `version=2`: the v0 dumb path
	// short-circuits before any POST, and the smart path advertises
	// the version pinned at probe time.
	gitProtocolHeader string

	// closeOnce guards the [Conn.Close] body so a second or later
	// invocation is a no-op, matching the [transport.Conn]
	// idempotent-close contract.
	closeOnce sync.Once

	// closeErr stores the result of the first [Conn.Close] body so
	// the very first invocation returns it. Subsequent invocations
	// return nil unconditionally to honour the [transport.Conn]
	// no-op-after-first contract.
	closeErr error

	// redir is the [probeRedirector] the connection's [http.Client]
	// installed as `CheckRedirect`. The command path retains a pointer
	// to it so [classifyRedirectError] can read `resolveErr` on the
	// command POST's redirect chain — exactly the same mechanism the
	// probe uses, so the cross-origin auth-strip and resolver-error
	// surfacing apply uniformly across GET and POST.
	redir *probeRedirector

	// tracer is the [trace.Tracer] captured at probe time from
	// [transport.OpenOptions]. It is consulted on every command POST
	// and threaded into the per-request [pktline.Reader] / [pktline.Writer]
	// so HTTP and packet events surface uniformly across the probe and
	// command paths. nil means tracing is disabled.
	tracer trace.Tracer

	// dumb is true when the connection came up via the dumb-HTTP
	// adapter. A dumb [Conn] short-circuits [Conn.Command] to
	// [ErrUnsupportedProtocol]: the server has no v2 command endpoint
	// to POST to.
	dumb bool

	// cmdBody tracks the [http.Response.Body] of the most recent
	// successful command POST so [Conn.Close] can release it if the
	// caller abandoned its [pktline.Reader] without draining. The field
	// is overwritten on each successful POST after draining and closing
	// the previous value, so a long-lived [Conn] does not accumulate
	// superseded bodies; the single-flight contract guarantees at most
	// one body is outstanding at a time.
	cmdBody io.ReadCloser
}

// Advertisement returns the cached pkt-line reader. On the smart
// branch the reader is positioned at the first byte of the
// advertisement proper, with the `# service=git-upload-pack` preamble
// and its trailing flush already consumed. On the dumb branch the
// reader is the synthetic v0-shaped stream from `internal/dumbhttp`.
func (c *Conn) Advertisement() *pktline.Reader {
	return c.reader
}

// Close drains and closes the probe response body and any in-flight
// command response body that has not been drained, exactly once.
// Subsequent calls are no-ops and return nil, matching the
// [transport.Conn] contract.
//
// The probe body's close error is the one returned; errors from the
// command-body cleanup are intentionally swallowed so a misbehaving
// late-arriving command body does not mask the canonical close error.
func (c *Conn) Close() error {
	first := false
	c.closeOnce.Do(func() {
		first = true
		if c.body != nil {
			// Drain whatever bytes remain so the underlying connection
			// can be reused by the [http.Client]'s connection pool. The
			// drain shape matches [drainAndClose] but the close error
			// is captured here rather than swallowed: it is what
			// [Conn.Close] surfaces to the caller.
			_, _ = io.Copy(io.Discard, io.LimitReader(c.body, drainCap))
			c.closeErr = c.body.Close()
		}
		if c.cmdBody != nil {
			drainAndClose(c.cmdBody)
			c.cmdBody = nil
		}
	})
	if !first {
		return nil
	}
	return c.closeErr
}

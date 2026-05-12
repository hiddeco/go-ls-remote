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
// Conn supports concurrent [Conn.Command] calls. Each command is an
// independent POST against the upload-pack endpoint, so multiple
// commands may be in flight at once and each returned [pktline.Reader]
// is independent of every other. Callers do not need to drain one
// reader before issuing the next command, nor to serialise calls
// against the same [Conn] across goroutines.
//
// Well-behaved callers drain each returned reader; the wrapper
// registered for tracking removes itself from the [Conn]'s
// in-flight set on the read-side terminal outcome (EOF or any
// other error) so the set's size tracks only commands whose
// reader is still live. A caller that abandons a reader without
// draining leaves its entry in the set until [Conn.Close], which
// drains and releases every still-tracked body so a long-lived
// [Conn] remains bounded.
//
// The advertisement reader returned from [Conn.Advertisement] runs
// over a separate response body owned by the [Conn] itself; reading
// it does not interact with the command-body tracking set.
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

	// inflightMu guards inflight. It is held only across set updates;
	// HTTP I/O runs without holding the mutex so concurrent commands
	// do not serialise on each other.
	inflightMu sync.Mutex

	// inflight is the set of in-flight [trackedBody] wrappers the
	// [Conn] still tracks. Each successful command POST registers its
	// wrapper here; the wrapper's cleanup callback deregisters when
	// the caller drains the response reader to EOF (or closes the
	// body explicitly), so the map's size reflects only commands
	// whose reader has not yet been finished. [Conn.Close] drains
	// and closes every still-tracked wrapper so an abandoned reader
	// is recovered at the latest by [Conn.Close].
	inflight map[*trackedBody]struct{}
}

// Advertisement returns the cached pkt-line reader. On the smart
// branch the reader is positioned at the first byte of the
// advertisement proper, with the `# service=git-upload-pack` preamble
// and its trailing flush already consumed. On the dumb branch the
// reader is the synthetic v0-shaped stream from `internal/dumbhttp`.
func (c *Conn) Advertisement() *pktline.Reader {
	return c.reader
}

// Close drains and closes the probe response body and every still-
// tracked command response body, exactly once. Subsequent calls are
// no-ops and return nil, matching the [transport.Conn] contract.
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
		c.inflightMu.Lock()
		bodies := c.inflight
		c.inflight = nil
		c.inflightMu.Unlock()
		for body := range bodies {
			// `body.Close` runs the cleanup (a no-op here because we
			// already cleared the map snapshot) and closes the inner
			// body. Drain first so the connection returns to the pool.
			_, _ = io.Copy(io.Discard, io.LimitReader(body, drainCap))
			_ = body.Close()
		}
	})
	if !first {
		return nil
	}
	return c.closeErr
}

// trackCommandBody registers body in the in-flight set. The
// [trackedBody]'s cleanup callback (configured at construction in
// `Conn.Command`) removes the entry when the caller drains the
// reader to EOF or closes the body explicitly.
func (c *Conn) trackCommandBody(body *trackedBody) {
	c.inflightMu.Lock()
	if c.inflight == nil {
		c.inflight = make(map[*trackedBody]struct{})
	}
	c.inflight[body] = struct{}{}
	c.inflightMu.Unlock()
}

// untrackCommandBody removes body from the in-flight set. Called
// from the [trackedBody] cleanup once per body.
func (c *Conn) untrackCommandBody(body *trackedBody) {
	c.inflightMu.Lock()
	delete(c.inflight, body)
	c.inflightMu.Unlock()
}

package httpt

import (
	"net/http"
	"time"

	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/trace"
	"github.com/hiddeco/go-ls-remote/transport"
)

// emitHTTPEvent records the outcome of one `client.Do` invocation on
// the supplied [trace.Tracer]. It is a no-op when the tracer is nil
// (the documented "tracing disabled" signal).
//
// The shape conforms to [trace.HTTPEvent]: Status is the HTTP status
// code from the response (or 0 when no response was received), Err
// is non-nil iff the request did not produce a response, and
// Duration is wall-clock time from start to response headers (or to
// the error). A 4xx/5xx response counts as "produced a response":
// Status carries the code and Err is nil. URL is redacted via
// [transport.RedactURL] so password components do not leak.
//
// One emission per `client.Do` call, regardless of how many redirect
// hops the chain followed: per-hop events would require either a
// custom `*http.Transport.RoundTrip` wrapper or threading the tracer
// through `*http.Client.CheckRedirect`, neither of which the current
// design takes on.
func emitHTTPEvent(t trace.Tracer, req *http.Request, resp *http.Response, err error, start time.Time) {
	if !trace.IsEnabled(t) {
		return
	}
	status := 0
	finalURL := req.URL.String()
	if resp != nil {
		status = resp.StatusCode
		// `resp.Request.URL` is the URL of the final hop after a
		// redirect chain; pin the event to that URL so logs can
		// correlate the recorded outcome with what the server
		// actually answered. Falling back to `req.URL` covers the
		// no-redirect path and the rare case where stdlib leaves
		// `resp.Request` unset.
		if resp.Request != nil && resp.Request.URL != nil {
			finalURL = resp.Request.URL.String()
		}
	}
	t.OnEvent(trace.HTTPEvent{
		Time:     time.Now(),
		Method:   req.Method,
		URL:      transport.RedactURL(finalURL),
		Status:   status,
		Duration: time.Since(start),
		Err:      err,
	})
}

// inboundReaderOpts returns the [pktline.ReaderOption] slice that wires
// t into a freshly constructed [pktline.Reader] for an inbound (server-
// to-client) HTTP response body. The redacted URL is attached so each
// emitted [trace.PacketEvent] is correlatable to the request that
// produced it; a fresh option slice per request is what lets the HTTP
// transport pin the URL even though redirects mean a single Conn-wide
// URL would be wrong.
//
// Returns nil when the tracer is disabled, so the per-call option
// slice is allocation-free on the no-tracer path.
func inboundReaderOpts(t trace.Tracer, redactedURL string) []pktline.ReaderOption {
	if !trace.IsEnabled(t) {
		return nil
	}
	return []pktline.ReaderOption{
		pktline.WithReaderTracerURL(t, trace.DirectionInbound, redactedURL),
	}
}

// outboundWriterOpts is the [pktline.WriterOption] counterpart to
// [inboundReaderOpts] for outbound (client-to-server) request bodies
// such as the v2 command POST.
func outboundWriterOpts(t trace.Tracer, redactedURL string) []pktline.WriterOption {
	if !trace.IsEnabled(t) {
		return nil
	}
	return []pktline.WriterOption{
		pktline.WithWriterTracerURL(t, trace.DirectionOutbound, redactedURL),
	}
}

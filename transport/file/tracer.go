package filet

import (
	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/trace"
)

// inboundReaderOpts returns the [pktline.ReaderOption] slice that wires
// t into a freshly constructed [pktline.Reader] reading bytes that flow
// inbound relative to the local endpoint. The redacted URL is attached
// so emitted [trace.PacketEvent] values carry the file:// URL the [Conn]
// was opened against.
//
// Returns nil when the tracer is disabled, so the no-tracer path is
// allocation-free: no slice header, no option closure, no per-call
// option apply.
//
// # Endpoint wiring
//
// The file transport runs both halves of the protocol in-process. By
// default, [transport.OpenOptions.Tracer] is wired only at the
// client-side reader and writer, so each pkt-line crossing the pipe
// pair produces a single [trace.PacketEvent] — matching the HTTP
// transport's one-event-per-pkt-line shape. Callers that want the
// in-process server's view too opt in via [WithEndpointTrace], which
// additionally wires the same tracer at the server-side reader and
// writer; each pkt-line then produces two events (one
// [trace.DirectionOutbound] from the writing side, one
// [trace.DirectionInbound] from the reading side). The doubling is
// intentional under that opt-in — a single tracer sees the full
// causal chain of a request and its response from both endpoints'
// perspectives.
func inboundReaderOpts(t trace.Tracer, redactedURL string) []pktline.ReaderOption {
	if !trace.IsEnabled(t) {
		return nil
	}
	return []pktline.ReaderOption{
		pktline.WithReaderTracerURL(t, trace.DirectionInbound, redactedURL),
	}
}

// outboundWriterOpts is the [pktline.WriterOption] counterpart to
// [inboundReaderOpts] for the writing side of an endpoint.
//
// Returns nil when the tracer is disabled, so the no-tracer path is
// allocation-free. See [inboundReaderOpts] for the rationale on which
// endpoints get wired by default and how [WithEndpointTrace] changes
// the event volume.
func outboundWriterOpts(t trace.Tracer, redactedURL string) []pktline.WriterOption {
	if !trace.IsEnabled(t) {
		return nil
	}
	return []pktline.WriterOption{
		pktline.WithWriterTracerURL(t, trace.DirectionOutbound, redactedURL),
	}
}

// serverEndpointTracer returns the [trace.Tracer] the in-process
// server's pkt-line reader and writer should be wired with. It is
// `tracer` when the [Transport] has [WithEndpointTrace] set, and nil
// otherwise — which causes [inboundReaderOpts] and
// [outboundWriterOpts] to short-circuit, leaving the server-side
// pkt-line readers/writers free of trace plumbing on the default
// path.
func serverEndpointTracer(t *Transport, tracer trace.Tracer) trace.Tracer {
	if !t.endpointTrace {
		return nil
	}
	return tracer
}

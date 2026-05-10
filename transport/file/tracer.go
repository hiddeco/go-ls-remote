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
// # Endpoint doubling
//
// The file transport runs both halves of the protocol in-process, so
// the same [trace.Tracer] is wired into the client-side reader/writer
// AND the server-side reader/writer the in-process emulator runs
// against. Each pkt-line on the pipe pair therefore produces two
// [trace.PacketEvent] values: one from the writing side as
// [trace.DirectionOutbound], one from the reading side as
// [trace.DirectionInbound]. The doubling is intentional — a single
// tracer sees the full causal chain of a request and its response from
// both endpoints' perspectives. Callers that want only the client's
// view supply distinct tracers; the transport itself does not
// deduplicate.
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
// allocation-free. See [inboundReaderOpts] for the rationale on why the
// file transport wires the tracer at every endpoint and what that means
// for event volume.
func outboundWriterOpts(t trace.Tracer, redactedURL string) []pktline.WriterOption {
	if !trace.IsEnabled(t) {
		return nil
	}
	return []pktline.WriterOption{
		pktline.WithWriterTracerURL(t, trace.DirectionOutbound, redactedURL),
	}
}

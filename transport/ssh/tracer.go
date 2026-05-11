package ssht

import (
	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/trace"
)

// inboundReaderOpts returns the [pktline.ReaderOption] slice that wires
// t into a freshly constructed [pktline.Reader] reading the SSH
// session's stdout — i.e. bytes flowing inbound (server-to-client)
// across the SSH channel. The redacted URL is attached so emitted
// [trace.PacketEvent] values carry the `ssh://` URL the [Conn] was
// opened against.
//
// Returns nil when the tracer is disabled, so the no-tracer path is
// allocation-free: no slice header, no option closure, no per-call
// option apply. The shape mirrors `transport/file/tracer.go` and
// `transport/http/tracer.go` so a tracer wired through
// `transport.OpenOptions` produces consistent [trace.PacketEvent]
// volume regardless of transport.
func inboundReaderOpts(t trace.Tracer, redactedURL string) []pktline.ReaderOption {
	if !trace.IsEnabled(t) {
		return nil
	}
	return []pktline.ReaderOption{
		pktline.WithReaderTracerURL(t, trace.DirectionInbound, redactedURL),
	}
}

// outboundWriterOpts is the [pktline.WriterOption] counterpart to
// [inboundReaderOpts] for the writing side of the SSH session — the
// stream feeding `git-upload-pack`'s stdin on the remote, i.e. bytes
// flowing outbound (client-to-server).
//
// Returns nil when the tracer is disabled, so the no-tracer path is
// allocation-free.
func outboundWriterOpts(t trace.Tracer, redactedURL string) []pktline.WriterOption {
	if !trace.IsEnabled(t) {
		return nil
	}
	return []pktline.WriterOption{
		pktline.WithWriterTracerURL(t, trace.DirectionOutbound, redactedURL),
	}
}

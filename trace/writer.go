package trace

import (
	"fmt"
	"io"
	"time"
)

// NewWriterTracer returns a [Tracer] that pretty-prints events to w in a
// human-readable text format suitable for diagnostic dumps — comparable
// to canonical Git's `GIT_TRACE_PACKET` output.
//
// The output format is not a stable contract; it exists for human
// consumption. Tests, scrapers, or other automated readers must not
// pattern-match against it. Programs that need machine-readable event
// data should implement their own [Tracer].
//
// The returned Tracer is safe for concurrent use only if w itself is
// safe for concurrent use; the writer call is otherwise unsynchronised.
func NewWriterTracer(w io.Writer) Tracer {
	return &writerTracer{w: w}
}

type writerTracer struct {
	w io.Writer
}

// OnPacketEvent renders a single pkt-line event to the underlying
// writer. Implements the typed-dispatch fast path on [Tracer]; called
// directly by `pktline.Reader` / `pktline.Writer` without going
// through OnEvent's polymorphic switch.
func (t *writerTracer) OnPacketEvent(e *PacketEvent) {
	_, _ = fmt.Fprintf(t.w, "%s %s packet %s %d bytes\n",
		e.Time.Format(time.RFC3339Nano),
		directionGlyph(e.Direction),
		kindLabel(e.Kind),
		len(e.Bytes))
}

// OnEvent dispatches on the dynamic type of e and renders a single line
// per event to the underlying writer. PacketEvent is handled via
// [writerTracer.OnPacketEvent] and does not flow through this method
// from library-internal emitters.
func (t *writerTracer) OnEvent(e Event) {
	switch ev := e.(type) {
	case HTTPEvent:
		_, _ = fmt.Fprintf(t.w, "%s http %s %s -> %d (%s)\n",
			ev.Time.Format(time.RFC3339Nano),
			ev.Method, ev.URL, ev.Status, ev.Duration)
	case NegotiateEvent:
		_, _ = fmt.Fprintf(t.w, "%s negotiate v=%d agent=%q caps=%v\n",
			ev.Time.Format(time.RFC3339Nano),
			ev.Version, ev.ServerAgent, ev.Capabilities)
	case CommandEvent:
		phase := "start"
		if ev.Phase == CommandEnd {
			phase = "end"
		}
		_, _ = fmt.Fprintf(t.w, "%s command %s %s dur=%s err=%v\n",
			ev.Time.Format(time.RFC3339Nano),
			ev.Name, phase, ev.Duration, ev.Err)
	default:
		// Forward-compatible default: prints any third-party Event type
		// using its Go-syntax representation.
		_, _ = fmt.Fprintf(t.w, "%s event %T %+v\n",
			ev.When().Format(time.RFC3339Nano), ev, ev)
	}
}

// directionGlyph returns a single-character glyph indicating direction
// of flow: `<` for inbound, `>` for outbound, `?` for unknown.
func directionGlyph(d Direction) string {
	switch d {
	case DirectionInbound:
		return "<"
	case DirectionOutbound:
		return ">"
	default:
		return "?"
	}
}

// kindLabel returns a lowercase short-form name for k. The trailing
// `unknown` arm exists to remain forward-compatible with new
// [PacketKind] values.
func kindLabel(k PacketKind) string {
	switch k {
	case PacketData:
		return "data"
	case PacketFlush:
		return "flush"
	case PacketDelim:
		return "delim"
	case PacketResponseEnd:
		return "response-end"
	default:
		return "unknown"
	}
}

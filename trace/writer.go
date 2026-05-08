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

// OnEvent dispatches on the dynamic type of e and renders a single line
// per event to the underlying writer.
func (t *writerTracer) OnEvent(e Event) {
	switch ev := e.(type) {
	case PacketEvent:
		fmt.Fprintf(t.w, "%s %s packet %s %d bytes\n",
			ev.Time.Format(time.RFC3339Nano),
			directionGlyph(ev.Direction),
			kindLabel(ev.Kind),
			len(ev.Bytes))
	case HTTPEvent:
		fmt.Fprintf(t.w, "%s http %s %s -> %d (%s)\n",
			ev.Time.Format(time.RFC3339Nano),
			ev.Method, ev.URL, ev.Status, ev.Duration)
	case NegotiateEvent:
		fmt.Fprintf(t.w, "%s negotiate v=%d agent=%q caps=%v\n",
			ev.Time.Format(time.RFC3339Nano),
			ev.Version, ev.ServerAgent, ev.Capabilities)
	case CommandEvent:
		phase := "start"
		if ev.Phase == CommandEnd {
			phase = "end"
		}
		fmt.Fprintf(t.w, "%s command %s %s dur=%s err=%v\n",
			ev.Time.Format(time.RFC3339Nano),
			ev.Name, phase, ev.Duration, ev.Err)
	default:
		// Forward-compatible default: prints any third-party Event type
		// using its Go-syntax representation.
		fmt.Fprintf(t.w, "%s event %T %+v\n",
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

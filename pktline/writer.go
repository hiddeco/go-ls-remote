package pktline

import (
	"fmt"
	"io"
	"time"

	"github.com/hiddeco/go-ls-remote/trace"
)

// Writer encodes pkt-lines to an underlying [io.Writer].
//
// Writer is not safe for concurrent use. Each public Write method
// emits its packet with a single underlying [io.Writer.Write] call so
// that partial-write errors propagate to the caller rather than
// leaving the wire in an indeterminate half-packet state.
//
// # Tracing
//
// A Writer wired up via [WithWriterTracer] emits a [trace.PacketEvent]
// after every successful write. Without a tracer, the Write methods
// perform no instrumentation work.
type Writer struct {
	dst io.Writer

	// out is a scratch buffer combining the 4-byte length prefix and
	// payload to issue exactly one Write call. It is reused across
	// calls and grown as needed up to [MaxPayload] + 4.
	out []byte

	// Tracer fields are nil/zero unless a [WithWriterTracer] or
	// [WithWriterTracerURL] option is applied.
	tracer   trace.Tracer
	traceDir trace.Direction
	traceURL string
}

// NewWriter returns a [Writer] that encodes pkt-lines to dst, applying
// the given options.
func NewWriter(dst io.Writer, opts ...WriterOption) *Writer {
	w := &Writer{dst: dst}
	for _, o := range opts {
		o.applyWriter(w)
	}
	return w
}

// WritePacket emits a single data packet whose payload is p. The empty
// payload is permitted and emits the on-wire bytes `0004`.
//
// Returns an error if `len(p) > [MaxPayload]`. The underlying writer's
// error is returned unchanged on a write failure; in either error
// case, no [trace.PacketEvent] is emitted.
func (w *Writer) WritePacket(p []byte) error {
	if len(p) > MaxPayload {
		return fmt.Errorf("pktline: payload length %d exceeds MaxPayload (%d)", len(p), MaxPayload)
	}
	total := 4 + len(p)
	if cap(w.out) < total {
		w.out = make([]byte, total)
	} else {
		w.out = w.out[:total]
	}
	encodeHexLength(w.out[:4], total)
	copy(w.out[4:], p)
	if _, err := w.dst.Write(w.out); err != nil {
		return err
	}
	w.emit(Data, p)
	return nil
}

// WriteFlush emits the flush control packet, on-wire `0000`.
func (w *Writer) WriteFlush() error { return w.writeControl("0000", Flush) }

// WriteDelim emits the delimiter control packet, on-wire `0001`.
func (w *Writer) WriteDelim() error { return w.writeControl("0001", Delim) }

// WriteResponseEnd emits the response-end control packet, on-wire
// `0002`. This is server-side usage; clients typically do not emit
// response-end markers. The method is provided for symmetry and for
// the in-process server emulator that lives in `internal/server`.
func (w *Writer) WriteResponseEnd() error { return w.writeControl("0002", ResponseEnd) }

func (w *Writer) writeControl(s string, k Kind) error {
	if _, err := io.WriteString(w.dst, s); err != nil {
		return err
	}
	w.emit(k, nil)
	return nil
}

// emit reports the just-written packet to the configured tracer (if
// any). Called only on successful writes.
func (w *Writer) emit(k Kind, payload []byte) {
	if w.tracer != nil {
		w.tracer.OnEvent(trace.PacketEvent{
			Time:      time.Now(),
			Direction: w.traceDir,
			URL:       w.traceURL,
			Bytes:     payload,
			Kind:      kindToTracerKind(k),
		})
	}
}

// encodeHexLength writes 4 lowercase ASCII hex digits representing v
// into b (which must be exactly 4 bytes long). Lowercase matches
// canonical Git's `pkt-line.c` output.
func encodeHexLength(b []byte, v int) {
	const hex = "0123456789abcdef"
	b[0] = hex[(v>>12)&0xf]
	b[1] = hex[(v>>8)&0xf]
	b[2] = hex[(v>>4)&0xf]
	b[3] = hex[v&0xf]
}

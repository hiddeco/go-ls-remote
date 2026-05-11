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
// after every successful write. The same `*trace.PacketEvent` is
// reused across emits — see the lifetime contract on [trace.PacketEvent]
// for what callers may and may not retain. Without a tracer, the Write
// methods perform no instrumentation work.
type Writer struct {
	dst io.Writer

	// out is a scratch buffer combining the 4-byte length prefix and
	// payload to issue exactly one Write call. It is reused across
	// calls and grown as needed up to [MaxPayload] + 4.
	out []byte

	// Tracer is nil unless a [WithWriterTracer] or [WithWriterTracerURL]
	// option is applied. event is heap-allocated alongside the tracer
	// option and reused across emits: emit mutates Time, Bytes and Kind
	// per call, then passes the pointer to OnEvent. Pre-allocating
	// avoids an escape that would otherwise box a fresh PacketEvent
	// value into the Tracer.OnEvent(Event) interface argument once per
	// pkt-line.
	tracer trace.Tracer
	event  *trace.PacketEvent
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
// Returns a wrapped [ErrPayloadTooLarge] if `len(p) > [MaxPayload]`.
// The underlying writer's error is returned unchanged on a write
// failure; in either error case, no [trace.PacketEvent] is emitted.
func (w *Writer) WritePacket(p []byte) error {
	if len(p) > MaxPayload {
		return fmt.Errorf("%w: %d > %d", ErrPayloadTooLarge, len(p), MaxPayload)
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

// WriteLine emits a data packet whose payload is s followed by a
// trailing `'\n'`. The string is copied directly into the writer's
// reused scratch buffer, avoiding the string-concatenation and
// `[]byte` conversion a caller would otherwise need to feed
// [Writer.WritePacket]. The empty string yields the on-wire bytes
// `0005\n`.
//
// The cap check, tracer emission, and underlying-write error
// propagation match [Writer.WritePacket]: a wrapped [ErrPayloadTooLarge]
// is returned if `len(s) + 1 > [MaxPayload]`, and on success the same
// reused `*trace.PacketEvent` is delivered to the configured tracer
// with Bytes pointing at the payload slice (including the trailing
// newline) — see the lifetime contract on [trace.PacketEvent].
func (w *Writer) WriteLine(s string) error {
	payloadLen := len(s) + 1
	if payloadLen > MaxPayload {
		return fmt.Errorf("%w: %d > %d", ErrPayloadTooLarge, payloadLen, MaxPayload)
	}
	total := 4 + payloadLen
	if cap(w.out) < total {
		w.out = make([]byte, total)
	} else {
		w.out = w.out[:total]
	}
	encodeHexLength(w.out[:4], total)
	copy(w.out[4:], s)
	w.out[total-1] = '\n'
	if _, err := w.dst.Write(w.out); err != nil {
		return err
	}
	w.emit(Data, w.out[4:total])
	return nil
}

// WriteLineParts emits a data packet whose payload is the concatenation
// of parts followed by a trailing `'\n'`. Each part is copied into
// successive offsets of the writer's reused scratch buffer, avoiding
// the intermediate string the caller would otherwise build to feed
// [Writer.WritePacket]. An empty parts list yields the on-wire bytes
// `0005\n`.
//
// The cap check, tracer emission, and underlying-write error
// propagation match [Writer.WritePacket]: a wrapped [ErrPayloadTooLarge]
// is returned if the sum of part lengths plus one exceeds [MaxPayload],
// and on success the configured tracer receives a [trace.PacketEvent]
// whose Bytes is the concatenated payload (including the trailing
// newline).
//
// Callers should pass a fixed, small number of parts; the Go compiler
// stack-allocates the variadic backing array for fixed call sites,
// keeping the call alloc-free. An unbounded `parts...` explosion forces
// a heap allocation for the slice header and defeats the point of this
// primitive.
func (w *Writer) WriteLineParts(parts ...string) error {
	payloadLen := 1 // trailing '\n'
	for _, p := range parts {
		payloadLen += len(p)
	}
	if payloadLen > MaxPayload {
		return fmt.Errorf("%w: %d > %d", ErrPayloadTooLarge, payloadLen, MaxPayload)
	}
	total := 4 + payloadLen
	if cap(w.out) < total {
		w.out = make([]byte, total)
	} else {
		w.out = w.out[:total]
	}
	encodeHexLength(w.out[:4], total)
	off := 4
	for _, p := range parts {
		off += copy(w.out[off:], p)
	}
	w.out[total-1] = '\n'
	if _, err := w.dst.Write(w.out); err != nil {
		return err
	}
	w.emit(Data, w.out[4:total])
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
//
// emit mutates the pre-allocated `w.event` rather than constructing a
// fresh `trace.PacketEvent`: the long-lived pointer lets the
// `Tracer.OnEvent(Event)` interface argument be boxed without a heap
// allocation per pkt-line. The `Direction` and `URL` fields are set
// once when the tracer option was applied and never change.
func (w *Writer) emit(k Kind, payload []byte) {
	if !trace.IsEnabled(w.tracer) {
		return
	}
	w.event.Time = time.Now()
	w.event.Bytes = payload
	w.event.Kind = kindToTracerKind(k)
	w.tracer.OnEvent(w.event)
}

// encodeHexLength writes 4 lowercase ASCII hex digits representing v
// into b, which must be exactly 4 bytes long. The implementation is
// hand-rolled rather than `fmt.Sprintf("%04x", v)` to avoid the
// per-call string allocation; pkt-line streams call this once per
// packet on a hot path. Lowercase matches canonical Git's
// `pkt-line.c` output.
func encodeHexLength(b []byte, v int) {
	const hex = "0123456789abcdef"
	b[0] = hex[(v>>12)&0xf]
	b[1] = hex[(v>>8)&0xf]
	b[2] = hex[(v>>4)&0xf]
	b[3] = hex[v&0xf]
}

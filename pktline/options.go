package pktline

import (
	"fmt"

	"github.com/hiddeco/go-ls-remote/trace"
)

// ReaderOption configures a [Reader] at construction time. Apply
// options by passing them to [NewReader].
type ReaderOption interface {
	applyReader(*Reader)
}

// WriterOption configures a [Writer] at construction time. Apply
// options by passing them to [NewWriter].
type WriterOption interface {
	applyWriter(*Writer)
}

type readerOptionFunc func(*Reader)

func (f readerOptionFunc) applyReader(r *Reader) { f(r) }

type writerOptionFunc func(*Writer)

func (f writerOptionFunc) applyWriter(w *Writer) { f(w) }

// WithReaderTracer wires t into the [Reader]. Each pkt-line read emits
// a [trace.PacketEvent] with the given [trace.Direction]. The
// `*trace.PacketEvent` passed to `Tracer.OnEvent` is reused across
// emits, and so are its fields — see the lifetime contract on
// [trace.PacketEvent] for what callers may and may not retain.
//
// Passing a nil Tracer is equivalent to not configuring one: the
// option becomes a no-op.
func WithReaderTracer(t trace.Tracer, dir trace.Direction) ReaderOption {
	return readerOptionFunc(func(r *Reader) {
		r.tracer = t
		r.event = &trace.PacketEvent{Direction: dir}
	})
}

// WithReaderTracerURL is like [WithReaderTracer] but additionally sets
// the [trace.PacketEvent.URL] field on emitted events. Useful when a
// Reader is associated with a known remote URL for diagnostics.
func WithReaderTracerURL(t trace.Tracer, dir trace.Direction, url string) ReaderOption {
	return readerOptionFunc(func(r *Reader) {
		r.tracer = t
		r.event = &trace.PacketEvent{Direction: dir, URL: url}
	})
}

// WithWriterTracer is the [Writer] counterpart to [WithReaderTracer].
func WithWriterTracer(t trace.Tracer, dir trace.Direction) WriterOption {
	return writerOptionFunc(func(w *Writer) {
		w.tracer = t
		w.event = &trace.PacketEvent{Direction: dir}
	})
}

// WithWriterTracerURL is the [Writer] counterpart to [WithReaderTracerURL].
func WithWriterTracerURL(t trace.Tracer, dir trace.Direction, url string) WriterOption {
	return writerOptionFunc(func(w *Writer) {
		w.tracer = t
		w.event = &trace.PacketEvent{Direction: dir, URL: url}
	})
}

// kindToTracerKind maps a pktline [Kind] to the corresponding
// [trace.PacketKind]. The two enums are deliberately separate to keep
// the trace package free of pktline imports (which would create a
// cycle); this helper is the boundary at which the conversion happens.
//
// kindToTracerKind panics on a Kind value that is not one of the
// constants defined in this package. Callers within the package only
// pass internally-produced Kind values, so a panic here indicates a
// programming error: a new Kind constant added without extending this
// switch. An exhaustive test in [Test_kindToTracerKind_exhaustive]
// catches that omission at build time.
func kindToTracerKind(k Kind) trace.PacketKind {
	switch k {
	case Data:
		return trace.PacketData
	case Flush:
		return trace.PacketFlush
	case Delim:
		return trace.PacketDelim
	case ResponseEnd:
		return trace.PacketResponseEnd
	}
	panic(fmt.Sprintf("pktline: unhandled Kind %d", k))
}

package pktline

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/hiddeco/go-ls-remote/trace"
)

// Reader decodes pkt-lines from an underlying [io.Reader].
//
// Reader is not safe for concurrent use. Each [Reader.ReadPacket] call
// reuses an internal buffer; the [Packet.Data] slice returned from one
// call aliases that buffer and is invalidated by the next call.
// Callers retaining the bytes across calls must copy them, for example
// with [bytes.Clone].
//
// # End of stream
//
// On clean stream termination — the underlying reader returns EOF
// before any byte of a length prefix has been read — ReadPacket
// returns [io.EOF]. A truncated length prefix or payload returns
// [io.ErrUnexpectedEOF]. A malformed prefix returns a wrapped error.
//
// # Tracing
//
// A Reader wired up via [WithReaderTracer] emits a [trace.PacketEvent]
// for every packet read. Without a tracer, ReadPacket performs no
// instrumentation work.
type Reader struct {
	src io.Reader

	// buf holds the most recently read packet payload. It is reused
	// across reads and grown as needed up to [MaxPayload].
	buf []byte

	// hdr is a fixed scratch buffer for the 4-byte length prefix.
	hdr [4]byte

	// Tracer fields are nil/zero unless a [WithReaderTracer] or
	// [WithReaderTracerURL] option is applied.
	tracer   trace.Tracer
	traceDir trace.Direction
	traceURL string
}

// NewReader returns a [Reader] that decodes pkt-lines from src,
// applying the given options.
func NewReader(src io.Reader, opts ...ReaderOption) *Reader {
	r := &Reader{src: src}
	for _, o := range opts {
		o.applyReader(r)
	}
	return r
}

// ReadPacket reads the next pkt-line from the underlying reader.
//
// On a normal data packet, the returned [Packet] has Kind [Data] and
// Data populated with payload bytes that alias the Reader's internal
// buffer; see [Reader] for the buffer-lifetime contract. On a control
// packet, the returned Packet has the matching Kind ([Flush], [Delim],
// or [ResponseEnd]) and a nil Data slice.
//
// At clean end-of-stream ReadPacket returns [io.EOF]. A truncated
// length prefix or payload returns [io.ErrUnexpectedEOF]. A malformed
// length prefix or out-of-range length returns a wrapped error.
//
// When a tracer is wired in via [WithReaderTracer], a successful read
// emits a [trace.PacketEvent] before the packet is returned. Errors
// do not emit events.
func (r *Reader) ReadPacket() (Packet, error) {
	if _, err := io.ReadFull(r.src, r.hdr[:]); err != nil {
		return Packet{}, err
	}

	length, err := parseHexLength(r.hdr[:])
	if err != nil {
		return Packet{}, err
	}

	switch length {
	case 0:
		return r.emit(Packet{Kind: Flush}), nil
	case 1:
		return r.emit(Packet{Kind: Delim}), nil
	case 2:
		return r.emit(Packet{Kind: ResponseEnd}), nil
	}
	if length < 4 {
		return Packet{}, fmt.Errorf("pktline: invalid length %04x", length)
	}
	payloadLen := length - 4
	if payloadLen > MaxPayload {
		return Packet{}, fmt.Errorf("pktline: payload length %d exceeds MaxPayload (%d)", payloadLen, MaxPayload)
	}

	if cap(r.buf) < payloadLen {
		r.buf = make([]byte, payloadLen)
	} else {
		r.buf = r.buf[:payloadLen]
	}
	if _, err := io.ReadFull(r.src, r.buf); err != nil {
		if errors.Is(err, io.EOF) {
			return Packet{}, io.ErrUnexpectedEOF
		}
		return Packet{}, err
	}
	return r.emit(Packet{Kind: Data, Data: r.buf}), nil
}

// emit reports p to the configured tracer (if any) and returns p
// unchanged so call sites can pass it through inline.
func (r *Reader) emit(p Packet) Packet {
	if r.tracer != nil {
		r.tracer.OnEvent(trace.PacketEvent{
			Time:      time.Now(),
			Direction: r.traceDir,
			URL:       r.traceURL,
			Bytes:     p.Data,
			Kind:      kindToTracerKind(p.Kind),
		})
	}
	return p
}

// parseHexLength decodes 4 ASCII hex digits to an integer. Canonical
// Git's `pkt-line.c` writes the length prefix in lowercase but accepts
// either case on read; we match that.
func parseHexLength(b []byte) (int, error) {
	if len(b) != 4 {
		return 0, fmt.Errorf("pktline: header is %d bytes, want 4", len(b))
	}
	v := 0
	for _, c := range b {
		var n int
		switch {
		case c >= '0' && c <= '9':
			n = int(c - '0')
		case c >= 'a' && c <= 'f':
			n = int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			n = int(c-'A') + 10
		default:
			return 0, fmt.Errorf("pktline: invalid hex byte 0x%02x in length prefix", c)
		}
		v = v<<4 | n
	}
	return v, nil
}

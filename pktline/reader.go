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
// # Buffering
//
// Reader does not buffer src. Each [Reader.ReadPacket] call issues up
// to two reads on the underlying source — one for the 4-byte length
// prefix and one for the payload — so an unbuffered network source
// (e.g., a [net.Conn]) costs two syscalls per packet. Wrap such
// sources in a [bufio.Reader] before constructing the Reader.
//
// # End of stream
//
// On clean stream termination — the underlying reader returns EOF
// before any byte of a length prefix has been read — ReadPacket
// returns [io.EOF]. A truncated length prefix or payload returns
// [io.ErrUnexpectedEOF]. A malformed prefix wraps one of the codec
// sentinels declared in this package.
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
// length prefix or payload returns [io.ErrUnexpectedEOF]. Codec-level
// failures wrap one of [ErrInvalidHex], [ErrInvalidLength], or
// [ErrPayloadTooLarge]; callers match with [errors.Is].
//
// When a tracer is wired in via [WithReaderTracer], a successful read
// emits a [trace.PacketEvent] before the packet is returned. Errors
// do not emit events.
func (r *Reader) ReadPacket() (Packet, error) {
	p, err := r.readPacket()
	if err != nil {
		return Packet{}, err
	}
	r.emit(p)
	return p, nil
}

// readPacket performs the codec work and returns the decoded packet
// without emitting tracer events; [Reader.ReadPacket] is the public
// wrapper that adds tracing on success.
func (r *Reader) readPacket() (Packet, error) {
	if _, err := io.ReadFull(r.src, r.hdr[:]); err != nil {
		return Packet{}, err
	}

	length, err := parseHexLength(r.hdr)
	if err != nil {
		return Packet{}, err
	}

	switch length {
	case 0:
		return Packet{Kind: Flush}, nil
	case 1:
		return Packet{Kind: Delim}, nil
	case 2:
		return Packet{Kind: ResponseEnd}, nil
	}
	if length < 4 {
		return Packet{}, fmt.Errorf("%w: %04x", ErrInvalidLength, length)
	}
	payloadLen := length - 4
	if payloadLen > MaxPayload {
		return Packet{}, fmt.Errorf("%w: %d > %d", ErrPayloadTooLarge, payloadLen, MaxPayload)
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
	return Packet{Kind: Data, Data: r.buf}, nil
}

// emit reports p to the configured tracer, if one is wired in.
func (r *Reader) emit(p Packet) {
	if r.tracer != nil {
		r.tracer.OnEvent(trace.PacketEvent{
			Time:      time.Now(),
			Direction: r.traceDir,
			URL:       r.traceURL,
			Bytes:     p.Data,
			Kind:      kindToTracerKind(p.Kind),
		})
	}
}

// parseHexLength decodes 4 ASCII hex digits to an integer. The
// implementation is hand-rolled rather than [strconv.ParseUint] to
// avoid the per-call `string(b)` allocation; pkt-line streams call
// this once per packet on a hot path. Canonical Git's `pkt-line.c`
// writes the prefix in lowercase but accepts either case on read,
// and we match that.
func parseHexLength(b [4]byte) (int, error) {
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
			return 0, fmt.Errorf("%w: 0x%02x", ErrInvalidHex, c)
		}
		v = v<<4 | n
	}
	return v, nil
}

package pktline

import (
	"bytes"
	"errors"
	"testing"
)

// FuzzReader_ReadPacket asserts that [Reader.ReadPacket] never panics
// on arbitrary input and that, on a successful data-packet return, the
// payload size never exceeds [MaxPayload].
//
// Run with `go test -fuzz=FuzzReader_ReadPacket ./pktline/...`. The
// seed corpus exercises every control-packet length, valid data
// packets, invalid hex prefixes, empty input, and a multi-packet
// sequence so the fuzz engine starts from interesting structure.
func FuzzReader_ReadPacket(f *testing.F) {
	seeds := [][]byte{
		[]byte(""),                 // empty stream
		[]byte("0000"),             // flush
		[]byte("0001"),             // delim
		[]byte("0002"),             // response-end
		[]byte("0003"),             // reserved/invalid
		[]byte("0004"),             // empty data packet
		[]byte("0007hi\n"),         // small data packet
		[]byte("zzzz"),             // non-hex prefix
		[]byte("ffff"),             // length > MaxPayload+4 (rejected)
		[]byte("0008x"),            // truncated payload
		[]byte("00010007hi\n0000"), // delim, data, flush in sequence
	}
	for _, s := range seeds {
		f.Add(s)
	}

	// maxIters bounds how many packets we attempt per fuzz input. The
	// reader is naturally bounded by stream length, so this is a
	// belt-and-braces guard against future input shapes (or codec
	// changes) that might admit a busy loop. 100 comfortably covers
	// every shape the seed corpus exercises.
	const maxIters = 100

	f.Fuzz(func(t *testing.T, in []byte) {
		r := NewReader(bytes.NewReader(in))
		for range maxIters {
			pkt, err := r.ReadPacket()
			if err != nil {
				return
			}
			if pkt.Kind == Data && len(pkt.Data) > MaxPayload {
				t.Errorf("ReadPacket returned data of size %d, exceeds MaxPayload %d",
					len(pkt.Data), MaxPayload)
			}
		}
	})
}

// FuzzPktline_RoundTrip writes a payload through a [Writer] and reads
// it back through a [Reader], asserting the codec is lossless on
// arbitrary input.
//
// Inputs larger than [MaxPayload] exercise the writer's overflow
// guard: WritePacket must reject them with [ErrPayloadTooLarge] and
// must not emit any bytes. Inputs at the boundary length and
// arbitrary binary payloads exercise the round-trip path.
//
// Run with `go test -fuzz=FuzzPktline_RoundTrip ./pktline/...`.
func FuzzPktline_RoundTrip(f *testing.F) {
	seeds := [][]byte{
		{},                                      // empty data packet
		[]byte("hi\n"),                          // small ASCII
		[]byte("command=ls-refs\n"),             // representative v2 command
		{0x00, 0xff, 0x01, 0x02, 0x03},          // binary including NUL
		bytes.Repeat([]byte{'x'}, MaxPayload),   // boundary: largest valid
		bytes.Repeat([]byte{'x'}, MaxPayload+1), // boundary: rejected
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, payload []byte) {
		var buf bytes.Buffer
		w := NewWriter(&buf)

		err := w.WritePacket(payload)
		if len(payload) > MaxPayload {
			if !errors.Is(err, ErrPayloadTooLarge) {
				t.Fatalf("WritePacket(%d bytes): err=%v, want ErrPayloadTooLarge",
					len(payload), err)
			}
			if buf.Len() != 0 {
				t.Fatalf("WritePacket(%d bytes) wrote %d bytes despite overflow",
					len(payload), buf.Len())
			}
			return
		}
		if err != nil {
			t.Fatalf("WritePacket(%d bytes): unexpected err=%v", len(payload), err)
		}
		if err := w.WriteFlush(); err != nil {
			t.Fatalf("WriteFlush: %v", err)
		}

		r := NewReader(&buf)
		got, err := r.ReadPacket()
		if err != nil {
			t.Fatalf("ReadPacket (data): %v", err)
		}
		if got.Kind != Data {
			t.Fatalf("ReadPacket data: kind=%v, want Data", got.Kind)
		}
		if !bytes.Equal(got.Data, payload) {
			t.Fatalf("payload mismatch:\n got: %q\nwant: %q", got.Data, payload)
		}

		flush, err := r.ReadPacket()
		if err != nil {
			t.Fatalf("ReadPacket (flush): %v", err)
		}
		if flush.Kind != Flush {
			t.Fatalf("ReadPacket flush: kind=%v, want Flush", flush.Kind)
		}
	})
}

package pktline

import (
	"bytes"
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

	const maxIters = 100 // bound to avoid runaway loops on adversarial input

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

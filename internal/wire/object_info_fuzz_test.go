package wire

import (
	"bytes"
	"testing"

	"github.com/hiddeco/go-ls-remote/pktline"
)

// FuzzDecodeObjectInfo exercises [DecodeObjectInfo] with arbitrary byte
// streams. The contract under test is that the decoder never panics
// and that, on a successful return, every [RawObjectInfo] in the
// returned slice has a non-empty OID — the missing-object shape
// (`<oid> ` with trailing space) is dropped by `parseObjectInfoLine`.
// Seeds cover the canonical attrs-only shape, multi-row responses with
// sizes, the trailing-space drop case, and an in-stream `ERR` packet.
func FuzzDecodeObjectInfo(f *testing.F) {
	for _, seed := range objectInfoFuzzSeeds() {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		r := pktline.NewReader(bytes.NewReader(data))
		infos, err := DecodeObjectInfo(r)
		if err != nil {
			return
		}
		for i, info := range infos {
			if info.OID == "" {
				t.Fatalf("DecodeObjectInfo[%d] returned empty OID: %#v", i, info)
			}
		}
	})
}

// objectInfoFuzzSeeds returns the seed corpus for
// [FuzzDecodeObjectInfo].
func objectInfoFuzzSeeds() [][]byte {
	return [][]byte{
		// Empty stream.
		nil,
		// Attrs-only response: `size` line then flush, no rows.
		buildPktBytes(func(w *pktline.Writer) {
			_ = w.WritePacket([]byte("size\n"))
			_ = w.WriteFlush()
		}),
		// Multi-row: attrs `size`, three `<oid> <size>` rows, flush.
		buildPktBytes(func(w *pktline.Writer) {
			_ = w.WritePacket([]byte("size\n"))
			_ = w.WritePacket([]byte(
				"0123456789abcdef0123456789abcdef01234567 1234\n"))
			_ = w.WritePacket([]byte(
				"89abcdef0123456789abcdef0123456789abcdef 5678\n"))
			_ = w.WritePacket([]byte(
				"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef 90\n"))
			_ = w.WriteFlush()
		}),
		// Missing-object row: `<oid> ` trailing space, dropped by decoder.
		buildPktBytes(func(w *pktline.Writer) {
			_ = w.WritePacket([]byte("size\n"))
			_ = w.WritePacket([]byte(
				"0123456789abcdef0123456789abcdef01234567 \n"))
			_ = w.WriteFlush()
		}),
		// `ERR` packet mid-stream.
		buildPktBytes(func(w *pktline.Writer) {
			_ = w.WritePacket([]byte("size\n"))
			_ = w.WritePacket([]byte(
				"0123456789abcdef0123456789abcdef01234567 1234\n"))
			_ = w.WritePacket([]byte("ERR object-info: aborting\n"))
			_ = w.WriteFlush()
		}),
	}
}

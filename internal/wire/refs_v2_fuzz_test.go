package wire

import (
	"bytes"
	"testing"

	"github.com/hiddeco/go-ls-remote/pktline"
)

// FuzzDecodeLSRefs exercises [DecodeLSRefs] with arbitrary byte streams.
// The contract under test is that the iterator never panics, terminates
// for every input, and that any [RawRef] yielded without an error has
// either a non-empty OID or the unborn flag set — the two shapes
// `parseLSRefsLine` is allowed to emit. Seeds cover the canonical
// success cases (single ref, peeled+symref attributes, unborn HEAD) and
// an in-stream `ERR` packet.
func FuzzDecodeLSRefs(f *testing.F) {
	for _, seed := range lsRefsFuzzSeeds() {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		r := pktline.NewReader(bytes.NewReader(data))
		for ref, err := range DecodeLSRefs(r) {
			if err != nil {
				continue
			}
			if ref.OID == "" && !ref.Unborn {
				t.Fatalf("DecodeLSRefs yielded ref with empty OID and Unborn=false: %#v", ref)
			}
		}
	})
}

// lsRefsFuzzSeeds returns the seed corpus for [FuzzDecodeLSRefs].
func lsRefsFuzzSeeds() [][]byte {
	return [][]byte{
		// Empty stream.
		nil,
		// Single concrete ref then flush.
		buildPktBytes(func(w *pktline.Writer) {
			_ = w.WritePacket([]byte(
				"89abcdef0123456789abcdef0123456789abcdef refs/heads/main\n"))
			_ = w.WriteFlush()
		}),
		// Multi-ref: HEAD with symref-target, tag with peeled, plain ref.
		buildPktBytes(func(w *pktline.Writer) {
			_ = w.WritePacket([]byte(
				"0123456789abcdef0123456789abcdef01234567 HEAD " +
					"symref-target:refs/heads/main\n"))
			_ = w.WritePacket([]byte(
				"89abcdef0123456789abcdef0123456789abcdef refs/heads/main\n"))
			_ = w.WritePacket([]byte(
				"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef refs/tags/v1.0 " +
					"peeled:cafebabecafebabecafebabecafebabecafebabe\n"))
			_ = w.WriteFlush()
		}),
		// Unborn HEAD pointing at refs/heads/main via symref-target.
		buildPktBytes(func(w *pktline.Writer) {
			_ = w.WritePacket([]byte(
				"unborn HEAD symref-target:refs/heads/main\n"))
			_ = w.WriteFlush()
		}),
		// `ERR` packet mid-stream.
		buildPktBytes(func(w *pktline.Writer) {
			_ = w.WritePacket([]byte(
				"89abcdef0123456789abcdef0123456789abcdef refs/heads/main\n"))
			_ = w.WritePacket([]byte("ERR upload-pack: aborting\n"))
			_ = w.WriteFlush()
		}),
	}
}

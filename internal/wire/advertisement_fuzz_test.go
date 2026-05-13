package wire

import (
	"bytes"
	"testing"

	"github.com/hiddeco/go-ls-remote/pktline"
)

// FuzzParseAdvertisement exercises [ParseAdvertisement] with arbitrary
// byte streams. The contract under test is that the parser never panics
// and only ever returns a Go error for malformed input. Seeds cover the
// happy-path discriminator branches (v0/v1/v2), control-only streams,
// and an `ERR` packet at the head so the engine starts from realistic
// pkt-line framing rather than random bytes.
func FuzzParseAdvertisement(f *testing.F) {
	for _, seed := range advertisementFuzzSeeds() {
		f.Add(seed)
	}

	f.Fuzz(func(_ *testing.T, data []byte) {
		r := pktline.NewReader(bytes.NewReader(data))
		_, _ = ParseAdvertisement(r, nil)
	})
}

// advertisementFuzzSeeds returns the seed corpus for
// [FuzzParseAdvertisement]. Each seed is built through [pktline.Writer]
// so the framing length prefixes are correct without hand-encoding.
func advertisementFuzzSeeds() [][]byte {
	return [][]byte{
		// Empty stream — parser should EOF cleanly.
		nil,
		// Flush-only — short-circuits to v0 with empty refs/caps.
		buildPktBytes(func(w *pktline.Writer) {
			_ = w.WriteFlush()
		}),
		// v2 advertisement: version line, a few caps, flush.
		buildPktBytes(func(w *pktline.Writer) {
			_ = w.WritePacket([]byte("version 2\n"))
			_ = w.WritePacket([]byte("agent=git/2.45.0\n"))
			_ = w.WritePacket([]byte("object-format=sha1\n"))
			_ = w.WritePacket([]byte("ls-refs=unborn\n"))
			_ = w.WriteFlush()
		}),
		// v0 cap-bearing first ref line followed by a peer ref and flush.
		buildPktBytes(func(w *pktline.Writer) {
			_ = w.WritePacket([]byte(
				"0123456789abcdef0123456789abcdef01234567 HEAD\x00" +
					"agent=git symref=HEAD:refs/heads/main\n"))
			_ = w.WritePacket([]byte(
				"89abcdef0123456789abcdef0123456789abcdef refs/heads/main\n"))
			_ = w.WriteFlush()
		}),
		// v1: explicit `version 1` followed by a v0-shaped body.
		buildPktBytes(func(w *pktline.Writer) {
			_ = w.WritePacket([]byte("version 1\n"))
			_ = w.WritePacket([]byte(
				"0123456789abcdef0123456789abcdef01234567 HEAD\x00agent=git\n"))
			_ = w.WriteFlush()
		}),
		// `ERR ` packet at the head — the v0 ref-line parser flags it as
		// malformed today, exercising the error path.
		buildPktBytes(func(w *pktline.Writer) {
			_ = w.WritePacket([]byte("ERR access denied\n"))
			_ = w.WriteFlush()
		}),
	}
}

// buildPktBytes runs fn against a fresh [pktline.Writer] backed by a
// [bytes.Buffer] and returns the framed bytes. The helper lets seed
// constructors read like the wire shape they intend.
func buildPktBytes(fn func(w *pktline.Writer)) []byte {
	var buf bytes.Buffer
	fn(pktline.NewWriter(&buf))
	return buf.Bytes()
}

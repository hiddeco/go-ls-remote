package objfmt

import (
	"strings"
	"testing"
)

// benchSHA1Sink and benchSHA256Sink defeat DCE on the typed-parser
// micro-benchmarks: without them the compiler can erase the loop body
// once it sees the result is unused.
var (
	benchSHA1Sink   SHA1Hash
	benchSHA256Sink SHA256Hash
)

func BenchmarkParseHex_SHA1(b *testing.B) {
	in := strings.Repeat("a", 40)

	b.ReportAllocs()
	for b.Loop() {
		h, err := ParseSHA1Hex(in)
		if err != nil {
			b.Fatal(err)
		}
		benchSHA1Sink = h
	}
}

func BenchmarkParseHex_SHA256(b *testing.B) {
	in := strings.Repeat("a", 64)

	b.ReportAllocs()
	for b.Loop() {
		h, err := ParseSHA256Hex(in)
		if err != nil {
			b.Fatal(err)
		}
		benchSHA256Sink = h
	}
}

// BenchmarkAppendHex_SHA1 characterises the steady-state cost of the
// append-style sibling on [SHA1Hash]. The bench reuses one growable
// scratch slice across iterations so the per-call shape isolates the
// encoding cost from the buffer growth.
func BenchmarkAppendHex_SHA1(b *testing.B) {
	var h SHA1Hash
	for i := range h {
		h[i] = byte(i*13 + 5)
	}
	dst := make([]byte, 0, 40)
	b.ReportAllocs()
	for b.Loop() {
		dst = h.AppendHex(dst[:0])
	}
}

// BenchmarkAppendHex_SHA256 is the [SHA256Hash] sibling of
// [BenchmarkAppendHex_SHA1].
func BenchmarkAppendHex_SHA256(b *testing.B) {
	var h SHA256Hash
	for i := range h {
		h[i] = byte(i*13 + 5)
	}
	dst := make([]byte, 0, 64)
	b.ReportAllocs()
	for b.Loop() {
		dst = h.AppendHex(dst[:0])
	}
}

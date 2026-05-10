package objfmt

import (
	"strings"
	"testing"
)

// benchHashSink defeats DCE on ParseHex micro-benchmarks: without it
// the compiler can erase the loop body once it sees the result is
// unused.
var benchHashSink Hash

func BenchmarkParseHex_SHA1(b *testing.B) {
	in := strings.Repeat("a", 40)

	b.ReportAllocs()
	for b.Loop() {
		h, err := ParseHex(in, SHA1)
		if err != nil {
			b.Fatal(err)
		}
		benchHashSink = h
	}
}

func BenchmarkParseHex_SHA256(b *testing.B) {
	in := strings.Repeat("a", 64)

	b.ReportAllocs()
	for b.Loop() {
		h, err := ParseHex(in, SHA256)
		if err != nil {
			b.Fatal(err)
		}
		benchHashSink = h
	}
}

// BenchmarkAppendHex characterises the steady-state cost of the
// append-style sibling. The bench reuses one growable scratch slice
// across iterations so the per-call shape isolates the encoding cost
// from the buffer growth.
func BenchmarkAppendHex(b *testing.B) {
	var h Hash
	for i := range 32 {
		h[i] = byte(i*13 + 5)
	}
	for _, tc := range []struct {
		name string
		a    Algo
	}{
		{"SHA1", SHA1},
		{"SHA256", SHA256},
	} {
		b.Run(tc.name, func(b *testing.B) {
			dst := make([]byte, 0, 64)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				dst = h.AppendHex(dst[:0], tc.a)
			}
		})
	}
}

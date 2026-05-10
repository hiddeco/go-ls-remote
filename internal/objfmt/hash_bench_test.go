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

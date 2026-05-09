package reftable

import "testing"

// benchBlockSink defeats DCE on parseBlock micro-benchmarks.
var benchBlockSink block

func BenchmarkParseBlock_RefBlock(b *testing.B) {
	// Steady-state block parse: a ref block at firstByteOffset=0 with
	// a small restart table. parseBlock allocates restartOffsets once
	// per call (length = restartCount); the bench reflects that
	// allocation in `B/op`.
	buf := buildBlock('r', 4096, []uint32{4, 256, 512, 1024, 2048})

	b.ReportAllocs()
	for b.Loop() {
		blk, err := parseBlock(buf, 0)
		if err != nil {
			b.Fatal(err)
		}
		benchBlockSink = blk
	}
}

func BenchmarkParseBlock_FirstRefBlock(b *testing.B) {
	// First ref block in the file: firstByteOffset=24 (v1 header
	// size) shifts the restart-offset rebase. This path runs on
	// every IterRefs and on every FindRef that lands on the first
	// block, so it's worth measuring distinctly.
	buf := buildFirstRefBlock(4096, 24, []uint32{28, 256, 512, 1024, 2048})

	b.ReportAllocs()
	for b.Loop() {
		blk, err := parseBlock(buf, 24)
		if err != nil {
			b.Fatal(err)
		}
		benchBlockSink = blk
	}
}

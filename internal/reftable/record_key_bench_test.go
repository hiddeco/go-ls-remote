package reftable

import "testing"

// benchKeySink defeats DCE on decodeKey micro-benchmarks: without it
// the compiler can erase the loop body once it sees the result is
// unused.
var benchKeySink []byte

func BenchmarkDecodeKey_NoPrefix(b *testing.B) {
	// Restart-point shape: prefix_length=0, suffix is the full key.
	// Every restart-table cmp() in [block.seekRestart] decodes a key
	// in this shape, so the cost compounds across an index descent.
	raw := encodeKey(nil, []byte("refs/heads/main"), 1)

	b.ReportAllocs()
	for b.Loop() {
		key, _, _, err := decodeKey(raw, nil)
		if err != nil {
			b.Fatal(err)
		}
		benchKeySink = key
	}
}

func BenchmarkDecodeKey_Prefixed(b *testing.B) {
	// Typical record shape: most of the key is shared with prevKey,
	// only the suffix is on disk. This is the steady-state cost of
	// walking a block forward via [decodeRefRecord].
	prev := []byte("refs/heads/branch-100")
	raw := encodeKey(prev, []byte("refs/heads/branch-101"), 1)

	b.ReportAllocs()
	for b.Loop() {
		key, _, _, err := decodeKey(raw, prev)
		if err != nil {
			b.Fatal(err)
		}
		benchKeySink = key
	}
}

package reftable

import "testing"

// benchVarintSink defeats dead-code elimination on the varint
// micro-benchmarks: without it the compiler can erase loops whose
// only side effect is a value the caller never reads.
var (
	benchVarintSink uint64
	benchVarintN    int
)

func BenchmarkDecodeVarint_OneByte(b *testing.B) {
	// 1-byte fast path: values 0..0x7f never set the continuation bit.
	// This is the cost a record-decoder pays when prefix_length is
	// small (< 128, which covers every realistic ref-name shared
	// prefix) and update_index_delta is small.
	buf := []byte{0x42}

	b.ReportAllocs()
	for b.Loop() {
		v, n, err := decodeVarint(buf)
		if err != nil {
			b.Fatal(err)
		}
		benchVarintSink = v
		benchVarintN = n
	}
}

func BenchmarkDecodeVarint_TwoByte(b *testing.B) {
	// 2-byte common case: hits the continuation loop exactly once.
	// Two bytes encode values 0x80..0x407f, which spans the suffix
	// lengths of every ref name longer than the shared prefix in a
	// typical block.
	buf := []byte{0xff, 0x7f}

	b.ReportAllocs()
	for b.Loop() {
		v, n, err := decodeVarint(buf)
		if err != nil {
			b.Fatal(err)
		}
		benchVarintSink = v
		benchVarintN = n
	}
}

func BenchmarkDecodeVarint_LongEncoding(b *testing.B) {
	// Many-byte worst case: the value is encoded by the test-side
	// `encodeVarint` helper to guarantee a well-formed input. Picking
	// `1 << 56` puts the running value safely under the decoder's
	// overflow guard while still forcing the continuation loop through
	// most of its iterations — close to the worst-case decode cost.
	buf := encodeVarint(uint64(1) << 56)

	b.ReportAllocs()
	for b.Loop() {
		v, n, err := decodeVarint(buf)
		if err != nil {
			b.Fatal(err)
		}
		benchVarintSink = v
		benchVarintN = n
	}
}

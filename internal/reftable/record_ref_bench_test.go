package reftable

import (
	"testing"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
)

// benchRefRecordSHA1Sink and benchRefRecordSHA256Sink defeat DCE on
// decodeRefRecord micro-benchmarks. Two sinks because the typed
// decoder yields concrete `refRecord[H]` per instantiation; a single
// `any` would re-introduce the boxing the typed shape eliminates.
var (
	benchRefRecordSHA1Sink   refRecord[objfmt.SHA1Hash]
	benchRefRecordSHA256Sink refRecord[objfmt.SHA256Hash]
)

func BenchmarkDecodeRefRecord_Single_SHA1(b *testing.B) {
	// value_type=1: a single 20-byte OID. The most common record
	// shape in any reftable — every value-record ref hits this path.
	oid := make([]byte, 20)
	for i := range oid {
		oid[i] = byte(i + 1)
	}
	raw := encodeRefRecord(nil, "refs/heads/main", 1, 7, oid, nil, "", 20)

	b.ReportAllocs()
	for b.Loop() {
		rec, _, _, err := decodeRefRecord[objfmt.SHA1Hash](raw, nil, 100)
		if err != nil {
			b.Fatal(err)
		}
		benchRefRecordSHA1Sink = rec
	}
}

func BenchmarkDecodeRefRecord_Peeled_SHA1(b *testing.B) {
	// value_type=2: tag with peeled OID. Two hash-sized copies plus a
	// no-prefix key. Worth measuring distinctly because tag-heavy
	// repos (many releases) skew records toward this shape.
	val := make([]byte, 20)
	peel := make([]byte, 20)
	for i := range 20 {
		val[i] = 0xAA
		peel[i] = 0x55
	}
	raw := encodeRefRecord(nil, "refs/tags/v1", 2, 0, val, peel, "", 20)

	b.ReportAllocs()
	for b.Loop() {
		rec, _, _, err := decodeRefRecord[objfmt.SHA1Hash](raw, nil, 50)
		if err != nil {
			b.Fatal(err)
		}
		benchRefRecordSHA1Sink = rec
	}
}

func BenchmarkDecodeRefRecord_Symref(b *testing.B) {
	// value_type=3: symref target. Costs a varint(target_len) and a
	// short string copy instead of a hash copy. HEAD is the canonical
	// example; every reftable carries one.
	raw := encodeRefRecord(nil, "HEAD", 3, 1, nil, nil, "refs/heads/main", 20)

	b.ReportAllocs()
	for b.Loop() {
		rec, _, _, err := decodeRefRecord[objfmt.SHA1Hash](raw, nil, 10)
		if err != nil {
			b.Fatal(err)
		}
		benchRefRecordSHA1Sink = rec
	}
}

func BenchmarkDecodeRefRecord_Single_SHA256(b *testing.B) {
	// 32-byte hash size: the value copy costs 32 bytes per record
	// instead of 20, the only material difference from the SHA-1 path.
	// Kept distinct so the SHA-256 transition's per-record cost is
	// visible alongside the SHA-1 baseline.
	oid := make([]byte, 32)
	for i := range oid {
		oid[i] = byte(i + 1)
	}
	raw := encodeRefRecord(nil, "refs/heads/main", 1, 0, oid, nil, "", 32)

	b.ReportAllocs()
	for b.Loop() {
		rec, _, _, err := decodeRefRecord[objfmt.SHA256Hash](raw, nil, 5)
		if err != nil {
			b.Fatal(err)
		}
		benchRefRecordSHA256Sink = rec
	}
}

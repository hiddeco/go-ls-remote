package objfmt

import (
	"crypto/sha1"
	"encoding/binary"
	"testing"
)

// makeIdxBenchEntries synthesises n distinct sorted-OID v2Entry
// records: each OID is `sha1(uint64(i))` so the byte distribution is
// uniform across the 256 fanout buckets.
func makeIdxBenchEntries(n int) []v2Entry {
	out := make([]v2Entry, n)
	var k [8]byte
	for i := range n {
		binary.BigEndian.PutUint64(k[:], uint64(i))
		sum := sha1.Sum(k[:])
		var h Hash
		copy(h[:20], sum[:])
		out[i] = v2Entry{oid: h, offset: uint64(12 + i*64), crc: uint32(i)}
	}
	return out
}

func BenchmarkIdx_FindOffset_v2_hit(b *testing.B) {
	const n = 10_000
	entries := makeIdxBenchEntries(n)
	path := writeV2Idx(b, b.TempDir(), entries)
	idx, err := OpenIdx(path, SHA1)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = idx.Close() })

	// Probe every 7th entry — coprime with `n` so the access
	// pattern walks the OID space without short-cycling, exercising
	// fanout buckets across the whole 256 range.
	probes := make([]Hash, 0, n/7+1)
	for i := 0; i < n; i += 7 {
		probes = append(probes, entries[i].oid)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		off, ok := idx.FindOffset(probes[i%len(probes)])
		if !ok {
			b.Fatalf("miss at probe %d", i%len(probes))
		}
		_ = off
	}
}

func BenchmarkIdx_FindOffset_v2_miss(b *testing.B) {
	const n = 10_000
	entries := makeIdxBenchEntries(n)
	path := writeV2Idx(b, b.TempDir(), entries)
	idx, err := OpenIdx(path, SHA1)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = idx.Close() })

	// Probe OIDs derived from out-of-range keys: the fanout still
	// narrows but the bsearch always exhausts its window.
	probes := make([]Hash, 0, 1024)
	var k [8]byte
	for i := n; i < n+1024; i++ {
		binary.BigEndian.PutUint64(k[:], uint64(i))
		sum := sha1.Sum(k[:])
		var h Hash
		copy(h[:20], sum[:])
		probes = append(probes, h)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		_, ok := idx.FindOffset(probes[i%len(probes)])
		if ok {
			b.Fatalf("unexpected hit at probe %d", i%len(probes))
		}
	}
}

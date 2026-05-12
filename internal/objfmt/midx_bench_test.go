package objfmt

import (
	"crypto/sha1"
	"encoding/binary"
	"testing"
)

// makeMidxBenchObjs synthesises n distinct midxObj records spread
// across packCount packs. Same OID derivation as the idx bench so
// fanout buckets fill uniformly.
func makeMidxBenchObjs(n, packCount int, baseOffset uint64) []midxObj {
	out := make([]midxObj, n)
	var k [8]byte
	for i := range n {
		binary.BigEndian.PutUint64(k[:], uint64(i))
		sum := sha1.Sum(k[:])
		var h SHA1Hash
		copy(h[:], sum[:])
		out[i] = midxObj{
			oid:     h,
			packIdx: uint32(i % packCount),
			offset:  baseOffset + uint64(i*64),
		}
	}
	return out
}

func BenchmarkMidx_Find_hit(b *testing.B) {
	const (
		n     = 10_000
		packs = 4
	)
	objs := makeMidxBenchObjs(n, packs, 12)
	packNames := []string{"pack-1.idx", "pack-2.idx", "pack-3.idx", "pack-4.idx"}
	path := writeMidx(b, b.TempDir(), midxFixture{
		algo:  SHA1,
		packs: packNames,
		objs:  objs,
	})
	m, err := OpenMidx[SHA1Hash](path, SHA1)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = m.Close() })

	probes := make([]SHA1Hash, 0, n/7+1)
	for i := 0; i < n; i += 7 {
		probes = append(probes, objs[i].oid)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		_, off, ok := m.Find(probes[i%len(probes)])
		if !ok {
			b.Fatalf("miss at probe %d", i%len(probes))
		}
		_ = off
	}
}

func BenchmarkMidx_Find_miss(b *testing.B) {
	const (
		n     = 10_000
		packs = 4
	)
	objs := makeMidxBenchObjs(n, packs, 12)
	path := writeMidx(b, b.TempDir(), midxFixture{
		algo:  SHA1,
		packs: []string{"pack-1.idx", "pack-2.idx", "pack-3.idx", "pack-4.idx"},
		objs:  objs,
	})
	m, err := OpenMidx[SHA1Hash](path, SHA1)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = m.Close() })

	probes := make([]SHA1Hash, 0, 1024)
	var k [8]byte
	for i := n; i < n+1024; i++ {
		binary.BigEndian.PutUint64(k[:], uint64(i))
		sum := sha1.Sum(k[:])
		var h SHA1Hash
		copy(h[:], sum[:])
		probes = append(probes, h)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		_, _, ok := m.Find(probes[i%len(probes)])
		if ok {
			b.Fatalf("unexpected hit at probe %d", i%len(probes))
		}
	}
}

func BenchmarkMidx_Find_loff(b *testing.B) {
	// Every offset > 2 GiB so every Find traverses the LOFF table.
	// `1<<31` is the threshold the writer uses to flag a slot as a
	// LOFF index per [midx-write.c::write_midx_object_offsets].
	//
	// [midx-write.c::write_midx_object_offsets]: https://github.com/git/git/blob/v2.54.0/midx-write.c#L562
	const (
		n     = 10_000
		packs = 4
	)
	objs := makeMidxBenchObjs(n, packs, 1<<31)
	path := writeMidx(b, b.TempDir(), midxFixture{
		algo:  SHA1,
		packs: []string{"pack-1.idx", "pack-2.idx", "pack-3.idx", "pack-4.idx"},
		objs:  objs,
	})
	m, err := OpenMidx[SHA1Hash](path, SHA1)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = m.Close() })

	probes := make([]SHA1Hash, 0, n/7+1)
	for i := 0; i < n; i += 7 {
		probes = append(probes, objs[i].oid)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		_, off, ok := m.Find(probes[i%len(probes)])
		if !ok {
			b.Fatalf("miss at probe %d", i%len(probes))
		}
		_ = off
	}
}

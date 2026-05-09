package objfmt

import (
	"path/filepath"
	"testing"
)

func benchPackFixture(name string) string {
	return filepath.Join("..", "..", "testdata", "objfmt", name)
}

func BenchmarkPack_ReadHeader_SHA1(b *testing.B) {
	// `ofs-delta.pack`: commit at 12, tree at 131, OFS_DELTA at 207.
	// Sweeping all three per iteration mixes the size-varint loop
	// with the OFS_DELTA offset-varint path so the alloc cost is
	// representative of a real walk, not just the cheapest header.
	p, err := OpenPack(benchPackFixture("ofs-delta.pack"), SHA1)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = p.Close() })
	offsets := []int64{12, 131, 207}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, off := range offsets {
			if _, err := p.ReadHeader(off); err != nil {
				b.Fatal(err)
			}
		}
	}
}

func BenchmarkPack_ReadHeader_SHA256(b *testing.B) {
	// `sha256-three.pack` per `sha256-three.offsets.txt`: commit at
	// 12, tree at 144, blob at 206. No deltas in this fixture, but
	// `algo.Size() == 32` widens the peek buffer to 64 bytes — the
	// SHA-256 worst case the allocation has to cover.
	p, err := OpenPack(benchPackFixture("sha256-three.pack"), SHA256)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = p.Close() })
	offsets := []int64{12, 144, 206}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, off := range offsets {
			if _, err := p.ReadHeader(off); err != nil {
				b.Fatal(err)
			}
		}
	}
}

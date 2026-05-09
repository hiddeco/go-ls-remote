package reftable

import (
	"path/filepath"
	"testing"
)

// benchReaderRefSink and benchReaderOKSink defeat DCE on Reader-level
// benchmarks where the result would otherwise go unused.
var (
	benchReaderRefSink RefRecord
	benchReaderOKSink  bool
)

// benchFixturePath resolves a reftable fixture relative to
// `testdata/reftable/`. Mirrors the test-side [fixturePath] but
// without a *testing.T dependency so benchmarks can use it directly.
// internal/reftable -> repo root.
func benchFixturePath(rel string) string {
	return filepath.Join("..", "..", "testdata", "reftable", rel)
}

// BenchmarkReader_FindRef_Hit_WithIndex measures a single ref lookup
// that descends through the ref index — the steady-state cost of a
// `git ls-remote`-style query against a reftable that carries one.
//
// The fixture is `with-index-sha1` (~120 refs, block_size=512); a
// larger baseline is a follow-up that needs either a larger committed
// fixture or a synthetic in-memory writer.
func BenchmarkReader_FindRef_Hit_WithIndex(b *testing.B) {
	r, err := OpenReader(benchFixturePath("with-index-sha1/0001-0001-aaaaaaaa.ref"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = r.Close() })

	// Mid-namespace probes: each forces an index descent rather than
	// landing on the first leaf block. Cycling across a small set of
	// mid-keys keeps the access pattern realistic.
	probes := []string{
		"refs/heads/branch-1",
		"refs/heads/branch-50",
		"refs/heads/branch-99",
		"refs/heads/branch-120",
	}

	b.ReportAllocs()
	b.ResetTimer()
	i := 0
	for b.Loop() {
		rec, ok, err := r.FindRef(probes[i%len(probes)])
		if err != nil {
			b.Fatal(err)
		}
		benchReaderRefSink = rec
		benchReaderOKSink = ok
		i++
	}
}

func BenchmarkReader_FindRef_Hit_WithoutIndex(b *testing.B) {
	// without-index-sha1 is small enough that the writer omits the
	// ref index; FindRef falls back to a linear ref-block walk. This
	// is the cost of the no-index fast path the spec carves out for
	// tiny single-block files.
	r, err := OpenReader(benchFixturePath("without-index-sha1/0001-0001-aaaaaaaa.ref"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = r.Close() })

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		rec, ok, err := r.FindRef("refs/heads/main")
		if err != nil {
			b.Fatal(err)
		}
		benchReaderRefSink = rec
		benchReaderOKSink = ok
	}
}

func BenchmarkReader_FindRef_Miss_WithIndex(b *testing.B) {
	// Definitely-absent name: the descent still runs in full, then
	// the leaf scan exhausts. A negative-lookup floor.
	r, err := OpenReader(benchFixturePath("with-index-sha1/0001-0001-aaaaaaaa.ref"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = r.Close() })

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, ok, err := r.FindRef("refs/heads/branch-9999")
		if err != nil {
			b.Fatal(err)
		}
		benchReaderOKSink = ok
	}
}

func BenchmarkReader_FindRef_Symref(b *testing.B) {
	// HEAD is a symref; this lookup decodes the value_type=3 path on
	// every iteration (target_len varint + target string copy).
	r, err := OpenReader(benchFixturePath("single-sha1/0001-0001-aaaaaaaa.ref"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = r.Close() })

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		rec, ok, err := r.FindRef("HEAD")
		if err != nil {
			b.Fatal(err)
		}
		benchReaderRefSink = rec
		benchReaderOKSink = ok
	}
}

func BenchmarkReader_IterRefs_WithIndex(b *testing.B) {
	// Full forward walk: representative of the ls-remote bulk path.
	r, err := OpenReader(benchFixturePath("with-index-sha1/0001-0001-aaaaaaaa.ref"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = r.Close() })

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		var count int
		for rec, err := range r.IterRefs() {
			if err != nil {
				b.Fatal(err)
			}
			benchReaderRefSink = rec
			count++
		}
		if count == 0 {
			b.Fatal("no records iterated")
		}
	}
}

func BenchmarkOpenReader(b *testing.B) {
	// Open-time cost: mmap.Open + ReadAt(whole file) + parseHeader +
	// verifyTrailer (CRC over the footer). This dominates the very
	// first FindRef on a cold path, so worth tracking on its own.
	path := benchFixturePath("with-index-sha1/0001-0001-aaaaaaaa.ref")

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		r, err := OpenReader(path)
		if err != nil {
			b.Fatal(err)
		}
		_ = r.Close()
	}
}

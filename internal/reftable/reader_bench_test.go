package reftable

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
)

// benchReaderRefSink and benchReaderOKSink defeat DCE on Reader-level
// benchmarks where the result would otherwise go unused.
var (
	benchReaderRefSink RefRecord[objfmt.SHA1Hash]
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
	r, err := OpenReader[objfmt.SHA1Hash](benchFixturePath("with-index-sha1/0001-0001-aaaaaaaa.ref"))
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
	r, err := OpenReader[objfmt.SHA1Hash](benchFixturePath("without-index-sha1/0001-0001-aaaaaaaa.ref"))
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
	r, err := OpenReader[objfmt.SHA1Hash](benchFixturePath("with-index-sha1/0001-0001-aaaaaaaa.ref"))
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
	r, err := OpenReader[objfmt.SHA1Hash](benchFixturePath("single-sha1/0001-0001-aaaaaaaa.ref"))
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
	r, err := OpenReader[objfmt.SHA1Hash](benchFixturePath("with-index-sha1/0001-0001-aaaaaaaa.ref"))
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

// BenchmarkReader_FindRef_AtScale measures `FindRef` against
// realistic forge-scale ref sets. The 1k and 10k fixtures are
// single-table — the index-descent path is the only thing in scope;
// no stack-merge bookkeeping. Probes are cycled across mid-namespace
// names so each call forces a full descent rather than landing on
// the leading or trailing leaf block.
func BenchmarkReader_FindRef_AtScale(b *testing.B) {
	for _, sc := range []struct {
		name    string
		fixture string
		count   int
	}{
		{"n=1000", "many-refs-1k-sha1", 1000},
		{"n=10000", "many-refs-10k-sha1", 10000},
	} {
		b.Run(sc.name, func(b *testing.B) {
			r, err := OpenReader[objfmt.SHA1Hash](benchFixturePath(
				sc.fixture + "/0001-0001-aaaaaaaa.ref"))
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { _ = r.Close() })

			// Mid-namespace probes spanning the whole ref set so the
			// access pattern hits all block paths roughly evenly.
			probes := make([]string, 32)
			for i := range probes {
				probes[i] = fmt.Sprintf("refs/heads/branch-%d",
					(i*sc.count)/len(probes))
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
		})
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
		r, err := OpenReader[objfmt.SHA1Hash](path)
		if err != nil {
			b.Fatal(err)
		}
		_ = r.Close()
	}
}

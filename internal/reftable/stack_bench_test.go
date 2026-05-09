package reftable

import (
	"path/filepath"
	"testing"
)

// benchStackRefSink and benchStackOKSink defeat DCE on Stack-level
// benchmarks where the result would otherwise go unused.
var (
	benchStackRefSink RefRecord
	benchStackOKSink  bool
)

func benchStackDir(rel string) string {
	return filepath.Join("..", "..", "testdata", "reftable", rel)
}

func BenchmarkStack_FindRef_Hit(b *testing.B) {
	// `Stack.FindRef` is a pure map lookup against the merged view
	// built at OpenStack time — the merge cost is paid up front, so
	// this should be cheap and allocation-free.
	s, err := OpenStack(benchStackDir("with-index-sha1"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = s.Close() })

	probes := []string{
		"refs/heads/branch-1",
		"refs/heads/branch-50",
		"refs/heads/branch-99",
		"refs/heads/branch-120",
		"refs/heads/main",
		"HEAD",
	}

	b.ReportAllocs()
	b.ResetTimer()
	i := 0
	for b.Loop() {
		rec, ok, _ := s.FindRef(probes[i%len(probes)])
		benchStackRefSink = rec
		benchStackOKSink = ok
		i++
	}
}

func BenchmarkStack_FindRef_Miss(b *testing.B) {
	s, err := OpenStack(benchStackDir("with-index-sha1"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = s.Close() })

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, ok, _ := s.FindRef("refs/heads/does-not-exist")
		benchStackOKSink = ok
	}
}

func BenchmarkStack_IterRefs(b *testing.B) {
	// IterRefs walks the cached `sorted []string` slice; with the
	// post-OpenStack merge already done this should be O(N) per call
	// with a single per-name map lookup. A baseline against future
	// changes that revisit the iteration path.
	s, err := OpenStack(benchStackDir("with-index-sha1"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = s.Close() })

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		var count int
		for rec, err := range s.IterRefs() {
			if err != nil {
				b.Fatal(err)
			}
			benchStackRefSink = rec
			count++
		}
		if count == 0 {
			b.Fatal("no records iterated")
		}
	}
}

func BenchmarkOpenStack_SingleTable(b *testing.B) {
	// Single-reader merge cost: parseHeader/verifyTrailer + iterAllRefs
	// + map insert per record. The smallest realistic OpenStack call.
	dir := benchStackDir("single-sha1")

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		s, err := OpenStack(dir)
		if err != nil {
			b.Fatal(err)
		}
		_ = s.Close()
	}
}

// BenchmarkOpenStack_ShadowedStack measures multi-reader merge cost
// on the 3-table `stack-shadow-sha1` fixture; a 5-table variant is
// a follow-up that needs an extended fixture from the generator
// script (or a synthetic writer in tree).
func BenchmarkOpenStack_ShadowedStack(b *testing.B) {
	dir := benchStackDir("stack-shadow-sha1")

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		s, err := OpenStack(dir)
		if err != nil {
			b.Fatal(err)
		}
		_ = s.Close()
	}
}

package server

import (
	"io"
	"strconv"
	"testing"

	"github.com/hiddeco/go-ls-remote/internal/wire"
	"github.com/hiddeco/go-ls-remote/pktline"
)

// BenchmarkWriteLSRefsResponse measures the per-session cost of
// emitting the v2 `ls-refs` response body. The per-ref formatter
// runs inside `writeLSRefsResponse` once for HEAD plus once per ref
// the prefix filter admits, with optional `peeled:` and
// `symref-target:` decorations gated on [wire.RefsArgs.Peel] /
// [wire.RefsArgs.Symrefs]. A discovery against a populated remote
// scales linearly with admitted-ref count, and a forge serving
// concurrent ls-refs requests pays per-attribute cost on every line.
//
// Sub-benches parameterise on:
//
//   - `n` (10, 1000, 10000): the ref-count axis the response
//     scales on.
//   - attribute mix (`plain`, `peel`, `peel+symrefs`): the
//     decoration set the per-ref formatter touches. The peel arm
//     hits [refPeel]'s [objstore.RefEntry.PeelKnown] short-circuit
//     for ~5% of refs (annotated tags carrying recorded peels in
//     the synthesised packed-refs file). The symrefs arm adds the
//     `symref-target:` decoration on HEAD.
//   - prefix filter (`all` vs `prefix=refs/heads/`): the all-prefix
//     arm exercises the whole namespace; the bounded arm exercises
//     the per-iteration filter at [collectLSRefsRefs] and pins the
//     allocation savings of the post-refactor "filter at collection"
//     shape.
//
// The store and writer are constructed once outside the timed loop;
// output flows through [io.Discard] so the measurement isolates the
// ls-refs formatter from sink cost.
func BenchmarkWriteLSRefsResponse(b *testing.B) {
	type variant struct {
		name string
		args wire.RefsArgs
	}
	variants := []variant{
		{name: "plain", args: wire.RefsArgs{}},
		{name: "peel", args: wire.RefsArgs{Peel: true}},
		{name: "peel+symrefs", args: wire.RefsArgs{Peel: true, Symrefs: true}},
		{name: "prefix=refs/heads/", args: wire.RefsArgs{
			Prefixes: []string{"refs/heads/"},
		}},
	}

	for _, v := range variants {
		for _, n := range []int{10, 1000, 10000} {
			b.Run(v.name+"/n="+strconv.Itoa(n), func(b *testing.B) {
				store := buildBenchPackedRefsRepo(b, n, true)
				w := pktline.NewWriter(io.Discard)

				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					if err := writeLSRefsResponse(w, store, v.args); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

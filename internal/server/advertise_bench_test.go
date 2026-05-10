package server

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
	"github.com/hiddeco/go-ls-remote/internal/objstore"
	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/transport"
)

// BenchmarkWriteV0Advertisement measures the per-session cost of
// emitting the v0 reference-discovery advertisement against
// fixture-driven ref counts. The per-ref builder loop in
// `writeV0Advertisement` runs once per ref the backend exposes, with
// an additional pkt-line per peeled tag, so the wall-time of a v0
// discovery scales linearly with ref count: a forge mirror serving
// thousands of branches plus a tag history is the hot-path shape this
// benchmark protects.
//
// Sub-benches parameterise on `n` (10, 1000, 10000) and on whether the
// fixture carries a peel mix (`with-peel` keeps ~5% of refs as
// annotated tags with recorded `^<peel>` lines). The peel arm
// exercises the `refPeel` short-circuit through
// [objstore.RefEntry[objfmt.SHA1Hash].PeelKnown] without needing real tag bodies on
// disk.
//
// The store, options and writer are constructed once outside the
// timed loop. Output flows through a [pktline.Writer] backed by
// [io.Discard] so the measurement isolates the formatting and ref
// iteration cost from the cost of any sink — a real socket, an
// `io.Pipe`, or a `bytes.Buffer` would otherwise dominate at
// `n=10000`.
func BenchmarkWriteV0Advertisement(b *testing.B) {
	for _, peel := range []struct {
		name string
		on   bool
	}{
		{"no-peel", false},
		{"with-peel", true},
	} {
		for _, n := range []int{10, 1000, 10000} {
			b.Run(peel.name+"/n="+strconv.Itoa(n), func(b *testing.B) {
				store := buildBenchPackedRefsRepo(b, n, peel.on)
				opts := Options{
					Agent:             "test-agent/0.0",
					PreferredProtocol: transport.ProtocolV0,
				}
				w := pktline.NewWriter(io.Discard)

				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					if err := writeV0Advertisement(w, store, opts); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// BenchmarkWriteV2Advertisement measures the cost of emitting the v2
// capability advertisement. The cap set is fixed-size (`agent`,
// `ls-refs=unborn`, `object-format=<algo>`, `object-info`), so this
// bench is effectively a fixed-cost pkt-line emission probe — the
// ref namespace is not traversed. The bench still has value as a
// per-byte allocation counter: a tracer-heavy or proxying transport
// can otherwise inflate the per-session overhead silently.
func BenchmarkWriteV2Advertisement(b *testing.B) {
	store := buildBenchPackedRefsRepo(b, 0, false)
	opts := Options{
		Agent:             "test-agent/0.0",
		PreferredProtocol: transport.ProtocolV2,
	}
	w := pktline.NewWriter(io.Discard)

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if err := writeV2Advertisement(w, store, opts); err != nil {
			b.Fatal(err)
		}
	}
}

// buildBenchPackedRefsRepo materialises a gitdir under `tb.TempDir()`
// with HEAD pointing at refs/heads/main and a `packed-refs` file
// carrying n synthetic refs in C-locale byte order. When withPeel is
// true, ~5% of the refs are annotated tags with recorded `^<peel>`
// lines, exercising the loop's peel branch without needing real tag
// bodies on disk.
//
// The store is opened with the default sha1 algo (no config file is
// written, matching `objstore.Config`'s documented default at
// `internal/objstore/config.go:22`) and its `Close` is registered
// with `tb.Cleanup`. The shape is deliberately ref-only — no
// objects, no pack — so the v0 / v2 advertisement and ls-refs
// benches measure ref-iteration and pkt-line emission cost rather
// than blob/tree resolution. Object-info benches reuse the
// committed `pack-only` fixture instead.
//
// Accepts [testing.TB] so the per-ref allocation regression tests in
// `ls_refs_test.go` and `advertise_test.go` can reuse the synthetic
// fixture without growing a parallel constructor.
//
// Refnames carry a six-digit zero-padded index so the lexicographic
// ordering matches the natural numerical ordering — without padding,
// `branch-10` would sort before `branch-2` and the benchmarks across
// `n` would not be directly comparable.
func buildBenchPackedRefsRepo(tb testing.TB, n int, withPeel bool) *objstore.Store[objfmt.SHA1Hash] {
	tb.Helper()

	root := tb.TempDir()
	gitDir := filepath.Join(root, ".git")
	for _, sub := range []string{"refs", "objects/pack"} {
		if err := os.MkdirAll(filepath.Join(gitDir, sub), 0o755); err != nil {
			tb.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(gitDir, "HEAD"),
		[]byte("ref: refs/heads/main\n"), 0o644); err != nil {
		tb.Fatal(err)
	}

	var pr strings.Builder
	pr.WriteString("# pack-refs with: peeled fully-peeled\n")
	if n > 0 {
		// First ref pins HEAD so `Store.Head` returns a non-zero OID.
		oid := benchSyntheticOID(0x9E3779B97F4A7C15)
		pr.WriteString(oid + " refs/heads/main\n")
	}
	// Index 0 pinned HEAD above; the loop fills in the remaining n-1
	// refs.
	for i := 1; i < n; i++ {
		oid := benchSyntheticOID(uint64(i)*0x9E3779B97F4A7C15 + 1)
		switch {
		case withPeel && i%20 == 0:
			fmt.Fprintf(&pr, "%s refs/tags/v%06d\n", oid, i)
			peeled := benchSyntheticOID(uint64(i)*0x517CC1B727220A95 + 7)
			fmt.Fprintf(&pr, "^%s\n", peeled)
		default:
			fmt.Fprintf(&pr, "%s refs/heads/branch-%06d\n", oid, i)
		}
	}
	if err := os.WriteFile(filepath.Join(gitDir, "packed-refs"),
		[]byte(pr.String()), 0o644); err != nil {
		tb.Fatal(err)
	}

	s, err := objstore.Open[objfmt.SHA1Hash](gitDir)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = s.Close() })
	return s
}

// benchSyntheticOID returns a deterministic 40-char lowercase-hex
// string keyed on seed. The bytes are not a real SHA-1 over any
// object — every consumer of these strings either treats OIDs as
// opaque hex (the wire layer, the advertisement formatter) or
// resolves them against a backend that fails-fast on missing OIDs.
// The shape mirrors the synthetic-OID helper in
// `internal/wire/refs_v2_bench_test.go::benchSyntheticHex`.
func benchSyntheticOID(seed uint64) string {
	const hex = "0123456789abcdef"
	var out [40]byte
	for i := range out {
		out[i] = hex[(seed>>(uint(i%16)*4))&0xf]
	}
	for i := 16; i < 40; i++ {
		out[i] = hex[(seed*uint64(i+1))&0xf]
	}
	return string(out[:])
}

package server

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/hiddeco/go-ls-remote/internal/objstore"
	"github.com/hiddeco/go-ls-remote/internal/wire"
	"github.com/hiddeco/go-ls-remote/pktline"
)

// BenchmarkWriteObjectInfoResponse measures the per-session cost of
// emitting the v2 `object-info` response body.
// `writeObjectInfoResponse` -> `emitObjectInfoLine` runs once per
// requested OID, with the per-OID branch selecting between a hit
// (`<oid> <size>` formatting), a miss (`<oid> ` empty-size form), or
// a parse error (inline ERR pkt-line). The hit path also touches
// [objstore.Store.ObjectInfo] for size resolution; the miss path
// short-circuits at the loose-first / pack-second lookup.
//
// Sub-benches parameterise on:
//
//   - shape: `hits` cycles through known pack OIDs and exercises the
//     full ObjectInfo lookup-and-format path; `misses` exercises the
//     empty-size form's emission path with an OID that the store
//     resolves to `os.ErrNotExist`.
//   - size attribute (`with-size` vs `without-size`): the
//     `with-size` arm exercises the `fmt.Fprintf(&b, " %d", size)`
//     formatting that the no-size arm skips entirely.
//   - `n` (10, 100, 1000): the per-request OID-count axis. Real
//     `object-info` requests cluster in the low hundreds (a single
//     batch from a dependency resolver), so the parameter range
//     covers the realistic shape rather than the larger sweeps the
//     ls-refs bench covers.
//
// The pack-only fixture is materialised once per sub-bench. The
// store is opened with `WithoutCRCCheck` so the timing reflects the
// emitter's own cost rather than the per-OID CRC-32 verification
// captured by `BenchmarkStore_ObjectInfo_CRC` in the objstore
// package.
func BenchmarkWriteObjectInfoResponse(b *testing.B) {
	type variant struct {
		name string
		hit  bool
		size bool
	}
	variants := []variant{
		{name: "hits/with-size", hit: true, size: true},
		{name: "hits/without-size", hit: true, size: false},
		{name: "misses/with-size", hit: false, size: true},
	}

	// Two known OIDs from the `pack-only` fixture's `three-objects.pack`.
	// They mirror the constants in
	// `internal/objstore/idx_catalog_test.go` (which are unexported, so
	// the values are duplicated here rather than imported across the
	// package boundary).
	hits := []string{
		"26dae744f51e61913f50bd402cbe63953c7d637b", // commit
		"97d881a6f710fc8fc34524d80bfc782359137a5c", // blob
	}

	for _, v := range variants {
		for _, n := range []int{10, 100, 1000} {
			b.Run(v.name+"/n="+strconv.Itoa(n), func(b *testing.B) {
				store := openBenchPackOnlyStore(b)
				w := pktline.NewWriter(io.Discard)

				oids := make([]string, n)
				for i := range oids {
					if v.hit {
						oids[i] = hits[i%len(hits)]
					} else {
						// Synthesised hex: not a real OID, hits the
						// `os.ErrNotExist` branch in `Store.ObjectInfo`
						// without reaching any pack body.
						oids[i] = benchSyntheticOID(uint64(i)*0xDEADBEEFCAFEBABE + 1)
					}
				}
				args := wire.ObjectInfoArgs{Size: v.size}

				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					if err := writeObjectInfoResponse(w, store, args, oids); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// openBenchPackOnlyStore copies the committed `pack-only` fixture
// into a fresh `b.TempDir()`, renames the `dotgit` component to
// `.git`, and opens an `objstore.Store` rooted at it with the CRC
// check disabled. Mirrors `materializeRepoFixture` in shape but
// takes a `*testing.B` and returns the opened store directly so the
// bench loop stays terse.
//
// `WithoutCRCCheck` is set so the bench measures the emitter's own
// cost rather than the per-object CRC-32 verification budget. The
// CRC path is benched separately in
// `internal/objstore/store_bench_test.go::BenchmarkStore_ObjectInfo_CRC`.
func openBenchPackOnlyStore(b *testing.B) *objstore.Store {
	b.Helper()

	wd, err := os.Getwd()
	if err != nil {
		b.Fatal(err)
	}
	src := filepath.Join(wd, "..", "..", "testdata", "repos", "pack-only")
	dst := b.TempDir()
	if err := copyFixtureTreeForBench(src, dst); err != nil {
		b.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dst, "dotgit"),
		filepath.Join(dst, ".git")); err != nil {
		b.Fatal(err)
	}

	s, err := objstore.Open(filepath.Join(dst, ".git"),
		objstore.WithoutCRCCheck())
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = s.Close() })
	return s
}

// copyFixtureTreeForBench mirrors `copyFixtureTree` in
// `object_info_test.go` but stays out of `*testing.T` ergonomics so
// the bench helpers do not pull in the test-side `require` package.
// Same shape: walk src and replicate every entry under dst, keeping
// the `dotgit` component intact for the caller to rename.
func copyFixtureTreeForBench(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}

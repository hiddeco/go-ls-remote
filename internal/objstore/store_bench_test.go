package objstore

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
)

// benchInfoSink and benchHashSink defeat dead-code elimination on
// `Store[objfmt.SHA1Hash]`-level benchmarks where the result would otherwise go unused.
var (
	benchInfoSink Info
	benchHashSink objfmt.SHA1Hash
	benchOKSink   bool
)

// BenchmarkStore_PeelRef compares the two paths through `Store[objfmt.SHA1Hash].PeelRef`:
// the `fully-peeled` short-circuit that resolves entirely from the ref
// backend's recorded peel slot, and the fall-through that drops into
// `Store[objfmt.SHA1Hash].Peel` and reads the loose tag body.
//
// Both sub-benches go through the exact same call (`store.PeelRef`); the
// fixture choice is what selects the path:
//
//   - `ShortCircuit` opens `packed-refs-fully-peeled`, whose header
//     advertises the `fully-peeled` trait and whose annotated tag entry
//     carries the recorded `^<peel>` line. `Lookup` returns
//     `PeelKnown=true` and `PeelRef` returns immediately.
//   - `Fallthrough` opens `loose-tag-deep`, which ships annotated tags
//     as loose ref files plus loose tag objects but no `packed-refs`,
//     so `PeelKnown=false` and `PeelRef` falls through to `Peel`. The
//     `Peel` cache is warmed up before `b.ResetTimer` so the steady-state
//     measurement does not include the one-time loose-object read.
func BenchmarkStore_PeelRef(b *testing.B) {
	b.Run("ShortCircuit", func(b *testing.B) {
		root := materializeBenchFixture(b, "packed-refs-fully-peeled")
		s, err := Open[objfmt.SHA1Hash](root)
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() { _ = s.Close() })

		// Sanity-check the path before timing: the recorded peel must be
		// non-zero and `PeelKnown` must be set on the underlying entry,
		// which is what makes this the short-circuit case rather than a
		// silently-degraded fall-through.
		entry, found, err := s.refs.Lookup("refs/tags/v1")
		if err != nil || !found || !entry.PeelKnown || entry.Peeled.IsZero() {
			b.Fatalf("setup: want PeelKnown short-circuit on `refs/tags/v1`; got found=%v entry=%+v err=%v",
				found, entry, err)
		}

		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			peeled, ok, err := s.PeelRef("refs/tags/v1")
			if err != nil {
				b.Fatal(err)
			}
			benchHashSink = peeled
			benchOKSink = ok
		}
	})

	b.Run("Fallthrough", func(b *testing.B) {
		root := materializeBenchFixture(b, "loose-tag-deep")
		s, err := Open[objfmt.SHA1Hash](root)
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() { _ = s.Close() })

		// Warm the `Peel` cache so the measurement reflects the steady
		// state of the fall-through (Lookup + cache hit) rather than the
		// first call's loose-object read.
		if _, _, err := s.PeelRef("refs/tags/v1"); err != nil {
			b.Fatal(err)
		}

		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			peeled, ok, err := s.PeelRef("refs/tags/v1")
			if err != nil {
				b.Fatal(err)
			}
			benchHashSink = peeled
			benchOKSink = ok
		}
	})
}

// BenchmarkStore_ObjectInfo_DeltaChain measures the OFS_DELTA walk in
// `Store[objfmt.SHA1Hash].ObjectInfo` across chain depths the resolver realistically
// encounters. Depth 0 is a non-delta blob — the loose-first miss falls
// straight through to a non-delta pack lookup. Depth 1 is the smallest
// pack-delta (one OFS_DELTA hop landing on its base). Depths 8 and 63
// stress the iterative walk: 8 is a moderate repack-style chain, 63
// sits at `maxChainDepth - 1` and exercises the last successful step
// before the depth bound trips (`maxChainDepth = 64`, walked as a
// half-open `for d := 0; d < maxChainDepth; d++`).
//
// Every depth row uses the same synthetic fixture shape
// `object_info_test.go`'s `makeDeepOfsDeltaChain` lays down, so the
// parameter is the only variable across rows. The store is opened
// with `WithoutCRCCheck` so the timing reflects pure walk cost (CRC
// overhead is captured separately by `BenchmarkStore_ObjectInfo_CRC`).
func BenchmarkStore_ObjectInfo_DeltaChain(b *testing.B) {
	for _, depth := range []int{0, 1, 8, maxChainDepth - 1} {
		b.Run("depth="+strconv.Itoa(depth), func(b *testing.B) {
			dir := b.TempDir()
			// Depth 0 lays down a 1-entry pack with the terminal blob
			// alone, exercising the non-delta pack lookup. Depth >= 1
			// adds that many OFS_DELTA hops on top, so the benchmarked
			// `ObjectInfo` call walks `depth` headers before landing on
			// the blob.
			oid := makeBenchOfsDeltaChain(b, dir, depth)

			s, err := Open[objfmt.SHA1Hash](dir, WithoutCRCCheck())
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { _ = s.Close() })

			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				info, err := s.ObjectInfo(oid)
				if err != nil {
					b.Fatal(err)
				}
				benchInfoSink = info
			}
		})
	}
}

// BenchmarkStore_ObjectInfo_CRC isolates the per-object CRC32
// verification cost the pack walker incurs in its default mode. The
// fixture is a single non-delta packed commit so the only difference
// between the two sub-benches is the `WithoutCRCCheck` option: with
// CRC verification enabled the walker hashes the object's compressed
// bytes against `Idx.FindCRC32`; with it disabled the verification is
// skipped entirely.
//
// Depth-0 (no delta walking) is deliberate — a deeper chain would
// blend chain-walk cost into the measurement and dilute the signal
// `BenchmarkStore_ObjectInfo_DeltaChain` already covers.
func BenchmarkStore_ObjectInfo_CRC(b *testing.B) {
	commit := hashFromHexB(b, threeCommitOID, objfmt.SHA1)

	b.Run("CRCEnabled", func(b *testing.B) {
		root := materializeBenchFixture(b, "pack-only")
		s, err := Open[objfmt.SHA1Hash](root)
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() { _ = s.Close() })
		if !s.cfg.verifyCRC {
			b.Fatal("setup: default open must enable CRC verification")
		}

		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			info, err := s.ObjectInfo(commit)
			if err != nil {
				b.Fatal(err)
			}
			benchInfoSink = info
		}
	})

	b.Run("CRCDisabled", func(b *testing.B) {
		root := materializeBenchFixture(b, "pack-only")
		s, err := Open[objfmt.SHA1Hash](root, WithoutCRCCheck())
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() { _ = s.Close() })
		if s.cfg.verifyCRC {
			b.Fatal("setup: WithoutCRCCheck must flip verifyCRC off")
		}

		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			info, err := s.ObjectInfo(commit)
			if err != nil {
				b.Fatal(err)
			}
			benchInfoSink = info
		}
	})
}

// BenchmarkStore_IterRefs measures a full forward walk of every ref the
// configured backend exposes. The drained-iterator shape is the
// steady-state cost a `git ls-remote` consumer pays per call once any
// per-Store[objfmt.SHA1Hash] caches are warm.
//
// The size matrix covers loose-files refs at N=10 / 100 / 1000 via a
// synthetic fixture that lays N branches under `refs/heads/` plus a
// minimal HEAD/objects/refs scaffold. The reftable branch is
// represented by `with-reftable-content` only (~2 refs, the populated
// fixture committed alongside the package's reftable tests). A
// matching N=10/100/1000 reftable matrix needs an in-tree reftable
// writer or an extended fixture generator — neither exists yet, so
// the gap is documented and left as a follow-up rather than papered
// over with a smaller fixture mislabelled as a larger one.
func BenchmarkStore_IterRefs(b *testing.B) {
	for _, n := range []int{10, 100, 1000} {
		b.Run("Loose/N="+strconv.Itoa(n), func(b *testing.B) {
			root := makeLooseRefsFixture(b, n)
			s, err := Open[objfmt.SHA1Hash](root)
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { _ = s.Close() })

			// `makeLooseRefsFixture` adds a `refs/heads/main` for the
			// symbolic HEAD to resolve onto, so the iterated count is
			// `n + 1`. Pin the floor as a sanity check rather than
			// asserting a static count: a regression that drops a ref
			// would still trip, but a future fixture tweak that grows a
			// helper ref is not punished.
			want := n + 1

			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				var count int
				for entry, err := range s.IterRefs() {
					if err != nil {
						b.Fatal(err)
					}
					benchHashSink = entry.OID
					count++
				}
				if count != want {
					b.Fatalf("want %d refs, got %d", want, count)
				}
			}
		})
	}

	// Reftable: only the committed `with-reftable-content` fixture is
	// available today (HEAD + `refs/heads/main`). A
	// N=10/100/1000 matrix needs a reftable writer in-tree (or a
	// fixture-generator extension); when one lands, mirror the loose
	// shape above and drop this scaffolded single sub-bench.
	b.Run("Reftable/Fixture=with-reftable-content", func(b *testing.B) {
		root := materializeBenchFixture(b, "with-reftable-content")
		s, err := Open[objfmt.SHA1Hash](root)
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() { _ = s.Close() })

		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			var count int
			for entry, err := range s.IterRefs() {
				if err != nil {
					b.Fatal(err)
				}
				benchHashSink = entry.OID
				count++
			}
			if count == 0 {
				b.Fatal("no refs iterated")
			}
		}
	})
}

// makeLooseRefsFixture lays down a minimal gitdir at a fresh `b.TempDir`
// carrying n loose branch refs under `refs/heads/branch-{0..n-1}`. The
// HEAD is symbolic onto `refs/heads/main`, kept distinct from the
// numbered branches so the resolver has a stable target for `Head`.
//
// The OIDs are synthetic 40-hex strings derived from the branch index;
// the loose-refs backend treats the body as opaque hex, so the bytes
// only need to parse, not refer to any real object. No `packed-refs`
// is written — every ref surfaces through the `refs/heads/` directory
// walk, which is the realistic loose-refs shape for repos that have
// not run `git pack-refs`.
func makeLooseRefsFixture(b *testing.B, n int) string {
	b.Helper()

	root := b.TempDir()
	gitDir := filepath.Join(root, ".git")
	if err := os.MkdirAll(filepath.Join(gitDir, "objects"), 0o755); err != nil {
		b.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(gitDir, "refs", "heads"), 0o755); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "HEAD"),
		[]byte("ref: refs/heads/main\n"), 0o644); err != nil {
		b.Fatal(err)
	}

	// `main` carries a synthetic 0xaa OID so the symbolic HEAD resolves;
	// the numbered branches each get a distinct synthetic OID derived
	// from their index. Distinct OIDs are not strictly required by the
	// iterator but keep the fixture honest.
	mainOID := fmt.Sprintf("%040x", 0xaaaaaaaa)
	if err := os.WriteFile(filepath.Join(gitDir, "refs", "heads", "main"),
		[]byte(mainOID+"\n"), 0o644); err != nil {
		b.Fatal(err)
	}
	for i := range n {
		oid := fmt.Sprintf("%040x", uint64(i+1)*0x0101010101010101)
		name := fmt.Sprintf("branch-%d", i)
		if err := os.WriteFile(filepath.Join(gitDir, "refs", "heads", name),
			[]byte(oid+"\n"), 0o644); err != nil {
			b.Fatal(err)
		}
	}
	return gitDir
}

// makeBenchOfsDeltaChain is the bench-side sibling of the test-only
// `makeDeepOfsDeltaChain` helper in `object_info_test.go`. It produces
// a synthetic SHA-1 pack rooted at root with depth OFS_DELTA hops on
// top of one terminal blob, returning the OID of the chain head — the
// deepest delta when depth >= 1, the terminal blob when depth == 0.
//
// Depth 0 yields a 1-entry pack carrying the terminal blob alone, so
// the row exercises the loose-first miss + non-delta pack lookup path
// with no walk at all. Depth >= 1 follows the same shape as the test
// helper: a 1-byte type/size header, a 1-byte OFS varint pointing
// back at the predecessor, and a hand-rolled zlib delta body whose
// inflated leading varints are `source_size = target_size = 2`.
func makeBenchOfsDeltaChain(b *testing.B, root string, depth int) objfmt.SHA1Hash {
	b.Helper()
	if depth < 0 {
		b.Fatalf("makeBenchOfsDeltaChain: depth must be >= 0, got %d", depth)
	}
	if err := os.MkdirAll(filepath.Join(root, "objects", "pack"), 0o755); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "HEAD"),
		[]byte("ref: refs/heads/main\n"), 0o644); err != nil {
		b.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "refs"), 0o755); err != nil {
		b.Fatal(err)
	}

	pack, idx, headOID := encodeBenchOfsDeltaPack(b, depth)
	if err := os.WriteFile(filepath.Join(root, "objects", "pack", "deep-chain.pack"),
		pack, 0o644); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "objects", "pack", "deep-chain.idx"),
		idx, 0o644); err != nil {
		b.Fatal(err)
	}
	return headOID
}

// hashFromHexB is the bench-flavoured sibling of `hashFromHex`. The
// test variant takes `*testing.T` and uses `require.NoError`; the bench
// path needs `*testing.B` and a plain `b.Fatal`, so the two cannot
// share a body without dragging `interface { Fatal(...) }` plumbing
// through every caller.
func hashFromHexB(b *testing.B, s string, algo objfmt.Algo) objfmt.SHA1Hash {
	b.Helper()
	if algo != objfmt.SHA1 {
		b.Fatalf("hashFromHexB only supports SHA-1, got %v", algo)
	}
	h, err := objfmt.ParseSHA1Hex(s)
	if err != nil {
		b.Fatal(err)
	}
	return h
}

// encodeBenchOfsDeltaPack assembles a SHA-1 pack v2 carrying one
// terminal blob followed by depth OFS_DELTA entries pointing back at
// their immediate predecessors. Returns the pack bytes, the paired
// v2 idx bytes, and the chain head's synthetic OID — the deepest
// delta when depth >= 1, the terminal blob when depth == 0. The
// layout mirrors `makeDeepOfsDeltaChain` in `object_info_test.go`;
// see that function's doc comment for the canonical-Git framing
// citations.
func encodeBenchOfsDeltaPack(b *testing.B, depth int) (packBytes, idxBytes []byte, head objfmt.SHA1Hash) {
	b.Helper()

	// Terminal blob: type=3, size in low 4 bits, no continuation,
	// followed by a zlib-compressed 1-byte body. The body is opaque
	// to the walker for non-delta entries.
	baseBody := frameBenchBlob(b, []byte("x"))

	// Each OFS_DELTA's body uses the canonical synthetic delta header:
	// `source_size = target_size = 2`, padded so the inflate buffer
	// returns a stable run for the walker's varint decode.
	deltaBodyTail := zlibCompressB(b, []byte{0x02, 0x02, 0x00})

	pack := new(bytes.Buffer)
	pack.Write([]byte("PACK"))
	_ = binary.Write(pack, binary.BigEndian, uint32(2))
	_ = binary.Write(pack, binary.BigEndian, uint32(depth+1))

	type entry struct {
		oid    objfmt.SHA1Hash
		offset uint32
		crc    uint32
	}
	records := make([]entry, 0, depth+1)

	baseOffset := uint32(pack.Len())
	pack.Write(baseBody)
	records = append(records, entry{
		oid:    benchSyntheticOID(0),
		offset: baseOffset,
		crc:    crc32.ChecksumIEEE(baseBody),
	})

	prevOffset := int64(baseOffset)
	for d := 1; d <= depth; d++ {
		startOff := pack.Len()
		var body bytes.Buffer
		body.WriteByte(0x62) // OFS_DELTA, low4=2, no continuation
		rel := int64(startOff) - prevOffset
		if rel >= 0x80 {
			b.Fatalf("OFS varint stride grew past 1 byte at depth %d (rel=%d)", d, rel)
		}
		body.WriteByte(byte(rel))
		body.Write(deltaBodyTail)
		records = append(records, entry{
			oid:    benchSyntheticOID(uint8(d)),
			offset: uint32(startOff),
			crc:    crc32.ChecksumIEEE(body.Bytes()),
		})
		pack.Write(body.Bytes())
		prevOffset = int64(startOff)
	}

	trailer := sha1.Sum(pack.Bytes())
	pack.Write(trailer[:])

	slices.SortFunc(records, func(a, b entry) int {
		return bytes.Compare(a.oid[:20], b.oid[:20])
	})

	idx := new(bytes.Buffer)
	idx.Write([]byte{0xff, 't', 'O', 'c'})
	_ = binary.Write(idx, binary.BigEndian, uint32(2))
	for n := range 256 {
		var count uint32
		for _, r := range records {
			if r.oid[0] <= byte(n) {
				count++
			}
		}
		_ = binary.Write(idx, binary.BigEndian, count)
	}
	for _, r := range records {
		idx.Write(r.oid[:20])
	}
	for _, r := range records {
		_ = binary.Write(idx, binary.BigEndian, r.crc)
	}
	for _, r := range records {
		_ = binary.Write(idx, binary.BigEndian, r.offset)
	}
	idx.Write(trailer[:])
	idxSum := sha1.Sum(idx.Bytes())
	idx.Write(idxSum[:])

	return pack.Bytes(), idx.Bytes(), benchSyntheticOID(uint8(depth))
}

// frameBenchBlob frames body as a non-delta blob entry: 1 type/size
// byte followed by a zlib stream. body must fit in 4 bits.
func frameBenchBlob(b *testing.B, body []byte) []byte {
	b.Helper()
	if len(body) > 15 {
		b.Fatalf("frameBenchBlob: body length %d exceeds 4-bit size field", len(body))
	}
	var buf bytes.Buffer
	buf.WriteByte(0x30 | byte(len(body))) // type=3 (blob), no continuation
	buf.Write(zlibCompressB(b, body))
	return buf.Bytes()
}

// zlibCompressB encodes body with the default zlib compressor on a
// `*testing.B` receiver. Sibling of the test-side `zlibCompress`.
func zlibCompressB(b *testing.B, body []byte) []byte {
	b.Helper()
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := io.Copy(zw, bytes.NewReader(body)); err != nil {
		b.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		b.Fatal(err)
	}
	return buf.Bytes()
}

// benchSyntheticOID fabricates a deterministic SHA-1-shaped hash keyed
// on a 1-byte tag. Same shape as the test-side `syntheticOID`: the
// hash is not a real SHA-1 over any object, only a stable identity for
// idx population.
func benchSyntheticOID(tag uint8) objfmt.SHA1Hash {
	var h objfmt.SHA1Hash
	for i := range 20 {
		h[i] = tag
	}
	h[19] ^= byte(tag * 13)
	return h
}

// materializeBenchFixture is the bench-side sibling of
// `materializeFixture`. It mirrors that helper's `dotgit` -> `.git`
// rename and `t.TempDir`-equivalent lifetime, but on a `*testing.B`
// receiver. Kept as a sibling rather than promoted into a shared
// helper because the test variant has a deeper call graph
// (`require.NoError` for diagnostics) and lifting it would obscure
// the test-side error messages.
func materializeBenchFixture(b *testing.B, name string) string {
	b.Helper()

	wd, err := os.Getwd()
	if err != nil {
		b.Fatal(err)
	}
	src := filepath.Join(wd, "..", "..", "testdata", "repos", name)
	info, err := os.Stat(src)
	if err != nil {
		b.Fatalf("fixture %q missing: %v", name, err)
	}
	if !info.IsDir() {
		b.Fatalf("fixture %q is not a directory", name)
	}

	dst := b.TempDir()
	err = filepath.Walk(src, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		parts := splitAll(rel)
		for i, p := range parts {
			if p == "dotgit" {
				parts[i] = ".git"
			}
		}
		target := filepath.Join(append([]string{dst}, parts...)...)
		if fi.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, raw, 0o644)
	})
	if err != nil {
		b.Fatal(err)
	}
	return dst
}

package wire

import (
	"bytes"
	"fmt"
	"strconv"
	"testing"

	"github.com/hiddeco/go-ls-remote/pktline"
)

// benchRawRefSink defeats dead-code elimination on the decode loop —
// without an observable use of every yielded [RawRef] the compiler can
// erase parts of the iterator body that the benchmark is meant to
// measure.
var benchRawRefSink RawRef

// BenchmarkDecodeLSRefs measures the steady-state cost of draining a v2
// `ls-refs` response stream. `DecodeLSRefs` and `parseLSRefsLine` run
// once per ref in the response, so the wall-time of a discovery against
// a populated remote scales linearly with ref count: a forge mirror
// streaming tens of thousands of refs through this iterator is the
// hot-path shape this benchmark protects.
//
// The byte-tokenisation path inside `parseLSRefsLine` (split on spaces
// via `bytes.Fields`, then convert tokens to strings only at the
// `RawRef` field boundary) is the specific path under guard. A prior
// refactor that dropped a per-line `[]byte → string` copy landed
// without a benchmark; this file pins the post-refactor numbers so a
// future regression surfaces quantitatively rather than as an unnoticed
// drift.
//
// Each sub-bench parameterises the response on ref count `n`. The
// per-ref attribute mix is realistic for a tag-heavy mirror: roughly
// 5% of refs carry a `symref-target:` (a HEAD-shaped symbolic ref
// alongside a couple of mirror aliases), 30% carry a `peeled:`
// companion (annotated tags), and the remainder are plain branch
// refs. Refnames span `refs/heads/`, `refs/tags/`, and a deeper
// `refs/heads/<n>/topic` shape so the tokeniser sees a representative
// distribution of payload lengths. OIDs use SHA-1 hex to match the
// existing fixtures in `refs_v2_test.go` — the byte-tokenisation cost
// scales with token length, so SHA-256 would shift absolute numbers
// but not the comparison shape across `n`.
//
// The response bytes are built once per sub-bench, outside the
// `b.Loop()` body. Inside the loop only the decode work runs: a fresh
// [bytes.Reader] over the prebuilt buffer feeds a fresh
// [pktline.Reader], which feeds `DecodeLSRefs`. The yielded values are
// stored to a package-level sink so the iterator body is not eliminated.
func BenchmarkDecodeLSRefs(b *testing.B) {
	for _, n := range []int{10, 1000, 10000} {
		b.Run("n="+strconv.Itoa(n), func(b *testing.B) {
			stream := buildBenchLSRefsStream(b, n)
			payload := stream.Bytes()

			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				r := pktline.NewReader(bytes.NewReader(payload))
				for ref, err := range DecodeLSRefs(r) {
					if err != nil {
						b.Fatal(err)
					}
					benchRawRefSink = ref
				}
			}
		})
	}
}

// buildBenchLSRefsStream encodes n synthetic ref lines as data packets
// followed by a flush, matching the canonical v2 `ls-refs` response
// framing (`gitprotocol-v2.adoc` §"ls-refs"). The attribute mix per ref
// — ~5% `symref-target:`, ~30% `peeled:`, the remainder plain — is
// chosen to mirror a tag-heavy mirror's discovery output. OIDs are
// synthesised from the index so the bytes are stable across runs.
func buildBenchLSRefsStream(b *testing.B, n int) *bytes.Buffer {
	b.Helper()

	var buf bytes.Buffer
	w := pktline.NewWriter(&buf)
	for i := 0; i < n; i++ {
		line := buildBenchRefLine(i)
		if err := w.WritePacket([]byte(line)); err != nil {
			b.Fatal(err)
		}
	}
	if err := w.WriteFlush(); err != nil {
		b.Fatal(err)
	}
	return &buf
}

// buildBenchRefLine returns a single v2 `ls-refs` ref-line payload
// (including the trailing LF) keyed on i. The shape distribution
// matches the per-ref mix documented on `BenchmarkDecodeLSRefs`:
// `i % 20 == 0` produces a symref-bearing line, `i % 10 < 3` (and not
// already a symref) produces a peeled tag, the rest are plain branches.
func buildBenchRefLine(i int) string {
	oid := benchSyntheticHex(uint64(i)*0x0101010101010101 + 1)
	switch {
	case i%20 == 0:
		// Symbolic ref: a HEAD-shaped line plus mirror aliases pointing
		// back at a stable branch. Five per cent of refs hit this arm.
		target := "refs/heads/main"
		name := "HEAD"
		if i != 0 {
			name = fmt.Sprintf("refs/remotes/mirror-%d/HEAD", i/20)
		}
		return oid + " " + name + " symref-target:" + target + "\n"
	case i%10 < 3:
		// Annotated tag: peel companion attached. Roughly 30 per cent
		// of refs hit this arm (excluding the symref carve-out, which
		// fires first for `i % 20 == 0`).
		peeled := benchSyntheticHex(uint64(i)*0x1111111111111111 + 7)
		return oid + " refs/tags/v" + strconv.Itoa(i) + " peeled:" + peeled + "\n"
	default:
		// Plain branch ref. Every fourth branch lives one directory
		// deeper so the tokeniser sees a non-trivial refname-length mix.
		if i%4 == 0 {
			return oid + " refs/heads/" + strconv.Itoa(i/4) + "/topic-" +
				strconv.Itoa(i) + "\n"
		}
		return oid + " refs/heads/branch-" + strconv.Itoa(i) + "\n"
	}
}

// benchSyntheticHex returns a deterministic 40-hex-char string keyed on
// seed. The bytes are not a real SHA-1 over any object — the decoder
// treats OIDs as opaque hex, so any stable 40-char hex run drives the
// tokeniser through the same code path a real OID would.
func benchSyntheticHex(seed uint64) string {
	const hex = "0123456789abcdef"
	var out [40]byte
	for i := range out {
		out[i] = hex[(seed>>(uint(i%16)*4))&0xf]
	}
	// Mix in a second pass so the trailing 24 chars vary with seed too;
	// otherwise out[16:] is constant and the comparison cost across
	// refs collapses to the leading 16 characters.
	for i := 16; i < 40; i++ {
		out[i] = hex[(seed*uint64(i+1))&0xf]
	}
	return string(out[:])
}

package wire

import (
	"bytes"
	"strconv"
	"testing"

	"github.com/hiddeco/go-ls-remote/pktline"
)

// benchAdvSink defeats dead-code elimination on the v0/v1 parse loop —
// without an observable use of the returned [Advertisement] the
// compiler can erase parts of `parseV0Advertisement`'s body that the
// benchmark is meant to measure.
var benchAdvSink Advertisement

// BenchmarkParseAdvertisement_v0 measures the steady-state cost of
// reading a v0 (or v1, which shares the body grammar) ref
// advertisement to flush. `parseFirstRefLine` runs once on the
// cap-bearing head line; the per-packet body in `parseV0Advertisement`
// runs once per remaining ref line, and `applySymrefs` then walks every
// `symref=NAME:TARGET` capability against every parsed ref. A discovery
// against an older v0/v1-speaking server (older Gerrit, smart-HTTP
// intermediaries that have not adopted v2) does the same shape of work
// as v2: cost scales linearly with ref count, with an extra
// O(symrefs * refs) tail in `applySymrefs`.
//
// The byte-cut path inside the per-line body — `bytes.Cut` on space,
// `strings.CutSuffix` for the `^{}` peel marker, and the early
// `bytes.HasPrefix` shallow-line guard — is the specific path under
// guard. Pinning numbers here keeps a future regression in any of those
// hot bytes-package calls visible.
//
// Each sub-bench parameterises the advertisement on ref count `n`. The
// per-ref attribute mix is realistic for a tag-heavy mirror: roughly
// 5% of refs are advertised as symbolic via a `symref=` capability on
// the first line (a HEAD-shaped line alongside a couple of mirror
// aliases pointing back at a stable branch), 30% carry a peel
// companion line (`<peel-oid> refs/tags/v<i>^{}`), and the remainder
// are plain branch refs. Refnames span `refs/heads/`, `refs/tags/`,
// and a deeper `refs/heads/<n>/topic` shape so the tokeniser sees a
// representative distribution of payload lengths. OIDs use SHA-1 hex
// to match the existing fixtures in `refs_v0_test.go` — the byte-cut
// cost scales with token length, so SHA-256 would shift absolute
// numbers but not the comparison shape across `n`.
//
// The advertisement bytes are built once per sub-bench, outside the
// `b.Loop()` body. Inside the loop only the parse work runs: a fresh
// [bytes.Reader] over the prebuilt buffer feeds a fresh
// [pktline.Reader], which feeds `ParseAdvertisement`. The dispatcher
// routes the head data packet (no `version ` prefix) to
// `parseV0Advertisement`, so the v0 body grammar is the path measured.
// The returned advertisement is stored to a package-level sink so the
// parse is not eliminated.
func BenchmarkParseAdvertisement_v0(b *testing.B) {
	for _, n := range []int{10, 1000, 10000} {
		b.Run("n="+strconv.Itoa(n), func(b *testing.B) {
			payload := buildBenchV0Advertisement(b, n).Bytes()

			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				r := pktline.NewReader(bytes.NewReader(payload))
				ad, err := ParseAdvertisement(r, nil)
				if err != nil {
					b.Fatal(err)
				}
				benchAdvSink = ad
			}
		})
	}
}

// buildBenchV0Advertisement encodes n synthetic ref lines as data
// packets followed by a flush, matching the v0 ref-advertisement
// framing ([gitprotocol-pack.adoc §"Reference Discovery"]): the head
// data packet is the cap-bearing first ref line (`<oid> SP <refname>
// NUL <cap-list>`), every subsequent data packet is a plain
// `<oid> SP <refname>` body line, and an annotated tag is followed by
// its peel companion `<peel-oid> SP <refname>^{}`. The capability list
// on the first line carries every `symref=` entry the response
// references, so `applySymrefs` exercises its full O(symrefs * refs)
// fan-out.
//
// [gitprotocol-pack.adoc §"Reference Discovery"]: https://github.com/git/git/blob/v2.54.0/Documentation/gitprotocol-pack.adoc#reference-discovery
//
// The attribute mix per ref — ~5% symbolic, ~30% peeled, the remainder
// plain — mirrors a tag-heavy mirror's advertisement output. OIDs are
// synthesised from the index so the bytes are stable across runs.
func buildBenchV0Advertisement(b *testing.B, n int) *bytes.Buffer {
	b.Helper()

	caps := buildBenchV0Caps(n)
	var buf bytes.Buffer
	w := pktline.NewWriter(&buf)

	// First ref line is cap-bearing. Use a HEAD line so the leading
	// `symref=HEAD:refs/heads/main` capability has a target to bind to,
	// matching the canonical empty-or-populated repo shape.
	headOID := benchSyntheticHexV0(0x1111111111111111)
	first := headOID + " HEAD\x00" + caps + "\n"
	if err := w.WritePacket([]byte(first)); err != nil {
		b.Fatal(err)
	}

	// Stable target branch for HEAD's symref. Emitted second so the
	// first-line symref capability resolves to a present ref.
	mainOID := benchSyntheticHexV0(0x2222222222222222)
	mainLine := mainOID + " refs/heads/main\n"
	if err := w.WritePacket([]byte(mainLine)); err != nil {
		b.Fatal(err)
	}

	// Remaining n-1 ref lines follow the documented mix. Index 0 is
	// already covered by HEAD above; indexes 1..n-1 produce the body.
	for i := 1; i < n; i++ {
		for _, line := range buildBenchV0RefLines(i) {
			if err := w.WritePacket([]byte(line)); err != nil {
				b.Fatal(err)
			}
		}
	}
	if err := w.WriteFlush(); err != nil {
		b.Fatal(err)
	}
	return &buf
}

// buildBenchV0Caps returns the capability list that rides on the first
// ref line. Every ref slot whose `i % 20 == 0` (excluding 0, which is
// HEAD itself and gets its own dedicated `symref=HEAD:...` entry)
// contributes a `symref=refs/remotes/mirror-N/HEAD:refs/heads/main`
// token, so `applySymrefs` walks symrefs proportional to ~5% of n.
// Two unrelated capabilities (`agent`, `multi_ack`) keep the list
// shape representative of a real advertisement.
func buildBenchV0Caps(n int) string {
	var sb bytes.Buffer
	sb.WriteString("multi_ack agent=git/2.45.0 symref=HEAD:refs/heads/main")
	for i := 20; i < n; i += 20 {
		sb.WriteString(" symref=refs/remotes/mirror-")
		sb.WriteString(strconv.Itoa(i / 20))
		sb.WriteString("/HEAD:refs/heads/main")
	}
	return sb.String()
}

// buildBenchV0RefLines returns the ref-line payload(s) (each including
// trailing LF) for slot i in the body. The shape distribution matches
// the per-ref mix documented on `BenchmarkParseAdvertisement_v0`:
// `i % 20 == 0` produces a mirror-HEAD ref whose symref binding rides
// on the first line's cap list, `i % 10 < 3` produces an annotated
// tag (two lines: tag + peel), and the rest are plain branches with a
// directory-deep variant every fourth slot.
func buildBenchV0RefLines(i int) []string {
	oid := benchSyntheticHexV0(uint64(i)*0x0101010101010101 + 1)
	switch {
	case i%20 == 0:
		// Mirror HEAD that the first-line symref capability targets.
		// Five per cent of refs hit this arm (post-bootstrap; HEAD
		// itself was already emitted before the body loop starts).
		name := "refs/remotes/mirror-" + strconv.Itoa(i/20) + "/HEAD"
		return []string{oid + " " + name + "\n"}
	case i%10 < 3:
		// Annotated tag: ref line plus peel companion. Roughly 30
		// per cent of refs hit this arm (excluding the symref carve-
		// out, which fires first for `i % 20 == 0`).
		tagName := "refs/tags/v" + strconv.Itoa(i)
		peeled := benchSyntheticHexV0(uint64(i)*0x1111111111111111 + 7)
		return []string{
			oid + " " + tagName + "\n",
			peeled + " " + tagName + peeledSuffix + "\n",
		}
	default:
		// Plain branch ref. Every fourth branch lives one directory
		// deeper so the cut-on-space sees a non-trivial refname-length
		// mix.
		if i%4 == 0 {
			return []string{
				oid + " refs/heads/" + strconv.Itoa(i/4) + "/topic-" +
					strconv.Itoa(i) + "\n",
			}
		}
		return []string{oid + " refs/heads/branch-" + strconv.Itoa(i) + "\n"}
	}
}

// benchSyntheticHexV0 returns a deterministic 40-hex-char string keyed
// on seed. The bytes are not a real SHA-1 over any object — the parser
// treats OIDs as opaque hex, so any stable 40-char hex run drives the
// tokeniser through the same code path a real OID would. The two-pass
// fill mixes the trailing 24 chars with seed too; otherwise out[16:]
// would be constant and the byte-cut cost across refs would collapse
// to the leading 16 characters.
func benchSyntheticHexV0(seed uint64) string {
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

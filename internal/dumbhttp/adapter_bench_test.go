package dumbhttp_test

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/hiddeco/go-ls-remote/internal/dumbhttp"
	"github.com/hiddeco/go-ls-remote/pktline"
)

// BenchmarkNewAdapter measures the cost of fully synthesising a
// v0-shaped pkt-line stream from a dumb HTTP `info/refs` body. The
// bench drives the entire adapter path: the per-line scanner traversal
// over the body, the [strings.Cut] split into OID and refname, the
// size-cap check, and the [encodePktLine] frame for each ref. After
// the last ref pkt-line the trailing flush terminates the loop.
//
// Synthesis is the dumb-HTTP fallback's hot loop: a discovery against
// a server that does not advertise smart HTTP runs the entire body
// through this path before the wire layer parses a single ref. The
// per-call cost therefore scales linearly with ref count, which the
// `n` axis varies (10 / 100 / 1000) to bracket the realistic
// distribution — small repos, typical repos, and large refs-heavy
// repos.
//
// The synthesised body is built once outside the timed loop and
// reused across iterations. Each iteration constructs a fresh
// [pktline.Reader] over a [strings.Reader] backed by that body, so
// the adapter setup cost is charged to every call (mirroring the
// per-Open cost a real client pays).
func BenchmarkNewAdapter(b *testing.B) {
	for _, n := range []int{10, 100, 1000} {
		body := buildBenchDumbBody(n)
		b.Run("n="+strconv.Itoa(n), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				pr := dumbhttp.NewAdapter(strings.NewReader(body))
				for {
					pkt, err := pr.ReadPacket()
					if err != nil {
						if errors.Is(err, io.EOF) {
							break
						}
						b.Fatal(err)
					}
					if pkt.Kind == pktline.Flush {
						break
					}
				}
			}
		})
	}
}

// buildBenchDumbBody synthesises a dumb HTTP `info/refs` body with
// n ref records, alternating between branches and tags so the
// adapter's per-ref split path is exercised against both shapes.
// Records use HTAB as the canonical separator
// ([gitprotocol-http.adoc lines 158-200]) and a fixed 40-char OID.
// The body is plain text, returned as a string so the timed loop
// can wrap it in a [strings.Reader] without an extra allocation
// per iteration.
//
// [gitprotocol-http.adoc lines 158-200]: https://github.com/git/git/blob/v2.54.0/Documentation/gitprotocol-http.adoc?plain=1#L158-L200
func buildBenchDumbBody(n int) string {
	const oid = "0123456789abcdef0123456789abcdef01234567"
	var sb strings.Builder
	for i := range n {
		sb.WriteString(oid)
		sb.WriteByte('\t')
		if i%4 == 0 {
			sb.WriteString("refs/tags/v")
		} else {
			sb.WriteString("refs/heads/branch-")
		}
		fmt.Fprintf(&sb, "%d\n", i)
	}
	return sb.String()
}

package ssht

import (
	"strings"
	"testing"

	"github.com/hiddeco/go-ls-remote/trace"
	"github.com/hiddeco/go-ls-remote/transport"
)

// BenchmarkShellQuote measures the cost of escaping a path through the
// `sq_quote_buf`-equivalent helper that every [Transport.Open] runs
// once per dial. The function builds a single string per call; the
// load-bearing axes are (i) the per-byte loop's branch-prediction
// behaviour and (ii) the `strings.Builder` allocation pattern.
//
// Sub-cases bracket the realistic and adversarial input distribution:
//
//   - `plain`: the canonical `/repo.git` shape — no escape branches
//     taken. Establishes the per-call floor cost and pins the
//     allocation budget at one string + the builder's backing slice.
//   - `quote`: a payload that exercises every `'` escape — the
//     quote-injection adversarial input. Each escape adds four bytes
//     to the output.
//   - `bang`: a payload that exercises every `!` escape — the
//     csh-history-expansion adversarial input.
//   - `long-plain`: a 256-byte path with no escapes. Pins the loop's
//     hot-path cost as input length scales without escape work.
//
// The benchmark reports allocations so a future tweak that adds a
// stray byte-slice copy (e.g. converting via `[]byte(s)`) shows up as
// an alloc regression.
func BenchmarkShellQuote(b *testing.B) {
	cases := []struct {
		name string
		s    string
	}{
		{"plain", "/repo.git"},
		{"quote", "/a'b'c'd'e"},
		{"bang", "/a!b!c!d!e"},
		{"long-plain", "/" + strings.Repeat("repo-name/", 25) + ".git"},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				_ = shellQuote(tc.s)
			}
		})
	}
}

// BenchmarkTransport_Open measures the steady-state cost of a single
// dial through the SSH transport: TCP connect → SSH transport-and-auth
// handshake → `NewSession` → `Setenv` → pipe opens → `git-upload-pack`
// exec → initial pkt-line write → advertisement drain → `Close`.
//
// The dominant component is the cryptographic handshake itself
// (Curve25519 KEX plus ed25519 signing and verification on both
// sides), which the in-process fixture runs against pre-generated
// host and client keys. The remaining components — session channel
// setup, `shellQuote`, the `WriteStreamRequest` pkt-line emission —
// are dwarfed by it but are still part of the per-dial cost a caller
// pays.
//
// The tracer axis splits the production no-tracing shape from the
// active-but-discarding shape so the per-dial observability tax is
// quantifiable directly. SSH has no `WithEndpointTrace`-style
// doubling switch (the file transport's third arm); the SSH
// transport wires the tracer once at the client-side reader and
// writer, so two arms suffice.
//
// The fixture's accept loop and per-connection goroutines are joined
// at sub-bench teardown via `t.Cleanup` so a long bench run does not
// trip `goleak.VerifyTestMain`.
func BenchmarkTransport_Open(b *testing.B) {
	tracerCases := []struct {
		name   string
		tracer trace.Tracer
	}{
		{"tracer=nil", nil},
		{"tracer=discard", benchDiscardTracer{}},
	}

	for _, tr := range tracerCases {
		b.Run(tr.name, func(b *testing.B) {
			srv := newTestServer(b, testServerOpts{
				acceptEnv:     true,
				advertisement: flushAdvertisement(),
			})
			transp := New(
				WithAuth(Signer(srv.clientSigner)),
				WithKnownHosts(srv.hostKeyCallback()),
			)
			u := srv.URL()
			ctx := b.Context()
			opts := transport.OpenOptions{
				UserAgent: "bench/0.0",
				Tracer:    tr.tracer,
			}

			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				conn, err := transp.Open(ctx, u, opts)
				if err != nil {
					b.Fatal(err)
				}
				// Drain the single-flush advertisement so the dial
				// completes deterministically. The fixture writes
				// `flushAdvertisement()` (a bare `0000` packet); one
				// `ReadPacket` consumes it.
				if _, err := conn.Advertisement().ReadPacket(); err != nil {
					b.Fatal(err)
				}
				if err := conn.Close(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

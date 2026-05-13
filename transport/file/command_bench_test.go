package filet

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/trace"
	"github.com/hiddeco/go-ls-remote/transport"
)

// BenchmarkConn_Command measures the steady-state cost of a v2
// command round-trip through the file transport: validate → encode →
// pipe write → in-process server dispatch → response pkt-line stream
// → client-side drain. Unlike the HTTP transport's bench of the same
// name, there is no network to short-circuit; the in-process server
// goroutine IS the work, so the numbers reflect end-to-end command
// latency over the canonical fixture rather than just the per-call
// plumbing.
//
// Sub-benches cover four request shapes that bracket realistic
// distribution and three tracer wirings:
//
// Shape axis (mirrors `transport/http`'s `BenchmarkConn_Command` so
// cross-package comparisons line up):
//
//   - `ls-refs/plain`: bare ls-refs against `loose-only`'s ref set —
//     HEAD plus a handful of branches and tags.
//   - `ls-refs/peel`: typical read-only with `peel`.
//   - `ls-refs/peel+prefixes`: discovery filtered to `refs/heads/`
//     and `refs/tags/`.
//   - `object-info/100-oids`: 100 unresolvable OIDs — the server
//     emits the canonical `<oid> ` no-size shape per OID
//     ([protocol-caps.c::send_info]) so the per-OID server response
//     loop is exercised at scale without resolving real objects.
//
// Tracer axis isolates the [trace.PacketEvent] overhead the encode
// and decode paths pay per pkt-line:
//
//   - `tracer=nil`: production no-tracing shape; client-side reader
//     and writer short-circuit before any option callback.
//   - `tracer=discard`: non-nil tracer wired client-side only — the
//     current default. Each pkt-line crossing the pipe pair
//     produces exactly one [trace.PacketEvent] (the client side's
//     view of the byte).
//   - `tracer=discard+endpoint-trace`: opt-in
//     [WithEndpointTrace] doubles the events per pkt-line by also
//     wiring the server-side reader/writer. Numbers under this arm
//     reproduce the prior dual-sided default behaviour and let the
//     observability tax of doubling be quantified directly.
//
// The `loose-only` fixture is materialised once per sub-bench. The
// [Conn] is reused across iterations: the single-flight contract is
// satisfied because each iteration drains its response through the
// trailing flush before issuing the next request.
//
// [protocol-caps.c::send_info]: https://github.com/git/git/blob/v2.54.0/protocol-caps.c#L37
func BenchmarkConn_Command(b *testing.B) {
	const oid = "0123456789abcdef0123456789abcdef01234567"
	manyOIDs := make([]string, 100)
	for i := range manyOIDs {
		manyOIDs[i] = "oid " + oid
	}

	shapes := []struct {
		name       string
		cmd        string
		args, caps []string
	}{
		{"ls-refs/plain", "ls-refs", nil, []string{"object-format=sha1"}},
		{
			"ls-refs/peel",
			"ls-refs",
			nil,
			[]string{"peel", "object-format=sha1"},
		},
		{
			"ls-refs/peel+prefixes",
			"ls-refs",
			[]string{"ref-prefix refs/heads/", "ref-prefix refs/tags/"},
			[]string{"peel", "object-format=sha1"},
		},
		{
			"object-info/100-oids",
			"object-info",
			manyOIDs,
			[]string{"object-format=sha1"},
		},
	}
	tracerCases := []struct {
		name   string
		opts   []Option
		tracer trace.Tracer
	}{
		{"tracer=nil", nil, nil},
		{"tracer=discard", nil, benchDiscardTracer{}},
		{
			"tracer=discard+endpoint-trace",
			[]Option{WithEndpointTrace()},
			benchDiscardTracer{},
		},
	}

	for _, sh := range shapes {
		for _, tr := range tracerCases {
			b.Run(sh.name+"/"+tr.name, func(b *testing.B) {
				c := openBenchConn(b, "loose-only", tr.opts, tr.tracer)
				ctx := b.Context()

				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					rdr, err := c.Command(ctx, sh.cmd, cmdBody(sh.cmd, sh.args, sh.caps))
					if err != nil {
						b.Fatal(err)
					}
					drainResponseToFlush(b, rdr)
				}
			})
		}
	}
}

// openBenchConn materialises the named fixture, dials it through a
// fresh [Transport] configured with opts and tracer, and drains the
// advertisement so the [Conn] is positioned at the command-dispatch
// point. Cleanup closes the connection at the end of the sub-bench.
//
// The helper is a bench-only mirror of `openTestConn`: the test
// equivalent uses `*testing.T`, this one accepts `*testing.B` so the
// fixture-materialise step (which itself goes through `testing.TB`)
// composes cleanly with the bench loop.
func openBenchConn(b *testing.B, fixture string, opts []Option, tracer trace.Tracer) *Conn[objfmt.SHA1Hash] {
	b.Helper()
	gitdir := materializeServeableFixture(b, fixture)
	u, err := transport.ParseURL("file://" + gitdir)
	require.NoError(b, err)

	tr := New(opts...)
	conn, err := tr.Open(b.Context(), u, transport.OpenOptions{
		UserAgent: "bench/0.0",
		Tracer:    tracer,
	})
	require.NoError(b, err)
	b.Cleanup(func() { _ = conn.Close() })

	c, ok := conn.(*Conn[objfmt.SHA1Hash])
	require.True(b, ok)
	drainAdvertisement(b, c)
	return c
}

// drainResponseToFlush reads packets off rdr until it observes the
// trailing flush, discarding the data slices. The bench-only shape
// avoids the per-packet `bytes.Clone` that `readAllPackets` performs
// for tests — the bench does not retain packet contents across
// iterations, so cloning would only inflate alloc counts without
// measuring anything the production path does.
func drainResponseToFlush(b *testing.B, rdr *pktline.Reader) {
	b.Helper()
	for {
		p, err := rdr.ReadPacket()
		if err != nil {
			b.Fatal(err)
		}
		if p.Kind == pktline.Flush {
			return
		}
	}
}

// benchDiscardTracer is a non-nil [trace.Tracer] whose methods drop
// every event. It exists so command-path benches can isolate the cost
// of the active-tracer shape from the cost of the nil-tracer shape
// (the [pktline.Writer]'s no-tracer short-circuit).
type benchDiscardTracer struct{}

func (benchDiscardTracer) OnPacketEvent(*trace.PacketEvent) {}
func (benchDiscardTracer) OnEvent(trace.Event)              {}

package filet

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
	"github.com/hiddeco/go-ls-remote/trace"
	"github.com/hiddeco/go-ls-remote/transport"
)

// BenchmarkTransport_Open measures the steady-state cost of a single
// dial through the file transport: URL decode → `objstore.Open` →
// two `io.Pipe` pairs → goroutine spawn → advertisement-streaming
// wait → drain advertisement → `Close`. Each iteration runs the
// full lifecycle so the goroutine spawn + teardown is part of the
// measurement, matching what a long-lived caller pays per dial.
//
// The `empty` fixture is used so the per-call cost is dominated by
// the transport's own work — pipe construction, goroutine spawn,
// store open — rather than by ref-iteration in the advertisement.
// `internal/server`'s benches already cover the advertise path
// against larger fixtures; this bench's load-bearing axis is dial
// overhead, not ref-set scaling.
//
// The tracer axis is the same three-way split documented on
// [BenchmarkConn_Command]: `tracer=nil` (production short-circuit),
// `tracer=discard` (the current default, client-side only), and
// `tracer=discard+endpoint-trace` (opt-in doubling). Dial-time event
// volume is fixed (no per-pkt-line work besides the advertisement),
// so the inter-arm delta is the per-dial observability tax.
func BenchmarkTransport_Open(b *testing.B) {
	gitdir := materializeServeableFixture(b, "empty")
	u, err := transport.ParseURL("file://" + gitdir)
	require.NoError(b, err)

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

	for _, tr := range tracerCases {
		b.Run(tr.name, func(b *testing.B) {
			ctx := b.Context()

			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				transp := New(tr.opts...)
				conn, err := transp.Open(ctx, u, transport.OpenOptions{
					UserAgent: "bench/0.0",
					Tracer:    tr.tracer,
				})
				if err != nil {
					b.Fatal(err)
				}
				c := conn.(*Conn[objfmt.SHA1Hash])
				drainAdvertisement(b, c)
				if err := conn.Close(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

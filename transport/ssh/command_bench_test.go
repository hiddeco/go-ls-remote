package ssht

import (
	"context"
	"testing"

	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/trace"
	"github.com/hiddeco/go-ls-remote/transport"
)

// BenchmarkConn_Command measures the steady-state cost of a v2
// command round-trip through the SSH transport: validate ctx → invoke
// the [transport.CommandBody] callback against the persistent
// [pktline.Writer] → flush through the SSH channel → in-process
// `server.Serve` dispatch → response pkt-line stream → client-side
// drain. Unlike the HTTP transport's bench of the same name, there is
// no `client.Do` to short-circuit; the SSH channel and the bridged
// `server.Serve` goroutine ARE the work, so the numbers reflect
// end-to-end command latency against the canonical fixture.
//
// Sub-benches cover four request shapes that bracket realistic
// distribution and two tracer wirings. The shape axis mirrors
// `transport/file`'s `BenchmarkConn_Command` so cross-transport
// comparisons line up:
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
// The tracer axis isolates the [trace.PacketEvent] overhead the
// encode and decode paths pay per pkt-line:
//
//   - `tracer=nil`: production no-tracing shape; client-side reader
//     and writer short-circuit before any option callback.
//   - `tracer=discard`: non-nil tracer wired at the client-side
//     reader and writer. Each pkt-line crossing the SSH channel
//     produces one [trace.PacketEvent] per direction.
//
// SSH has no `WithEndpointTrace`-style doubling switch (the file
// transport's third arm); the tracer is wired exactly once at the
// client-side endpoints in this package's design.
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
		tracer trace.Tracer
	}{
		{"tracer=nil", nil},
		{"tracer=discard", benchDiscardTracer{}},
	}

	for _, sh := range shapes {
		for _, tr := range tracerCases {
			b.Run(sh.name+"/"+tr.name, func(b *testing.B) {
				c := openBenchBridgedConn(b, "loose-only", tr.tracer)
				ctx := context.Background()
				body := cmdBody(sh.cmd, sh.args, sh.caps)

				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					rdr, err := c.Command(ctx, sh.cmd, body)
					if err != nil {
						b.Fatal(err)
					}
					drainResponseToFlush(b, rdr)
				}
			})
		}
	}
}

// openBenchBridgedConn dials the in-process SSH fixture against a
// bridged SHA1 store, drains the advertisement, and returns the typed
// [Conn] ready for [Conn.Command]. It is the bench-only sibling of
// `openBridgedConn` in `command_test.go` — same wiring, `testing.TB`
// instead of `*testing.T` — kept here so the production helper does
// not pick up a bench-only tracer parameter.
func openBenchBridgedConn(b *testing.B, fixture string, tracer trace.Tracer) *Conn {
	b.Helper()
	bridge := bridgeSHA1Store(b, fixture)
	srv := newTestServer(b, testServerOpts{
		acceptEnv: true,
		serveStore: func() bridgeServer {
			return bridge
		},
	})

	tr := New(
		WithAuth(Signer(srv.clientSigner)),
		WithKnownHosts(srv.hostKeyCallback()),
	)
	conn, err := tr.Open(context.Background(), srv.URL(), transport.OpenOptions{
		UserAgent: "bench/0.0",
		Tracer:    tracer,
	})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = conn.Close() })

	c, ok := conn.(*Conn)
	if !ok {
		b.Fatalf("openBenchBridgedConn: Open returned %T, want *Conn", conn)
	}
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
// (the [pktline.Writer]'s no-tracer short-circuit). The same type is
// reused by [BenchmarkTransport_Open] in `open_bench_test.go`.
type benchDiscardTracer struct{}

func (benchDiscardTracer) OnPacketEvent(*trace.PacketEvent) {}
func (benchDiscardTracer) OnEvent(trace.Event)              {}

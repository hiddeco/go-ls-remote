package server

import (
	"bytes"
	"io"
	"strconv"
	"testing"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/trace"
)

// BenchmarkProcessV2Request measures the steady-state cost of the
// v2 dispatcher's parse loop in `processV2Request`. The probe drives
// an unknown-command request shape so dispatch resolves to the
// `command not supported` ERR + wrapped-`ErrServerRefused` short
// path: no handler runs, no store work happens, and the per-request
// cost reduces to (1) the per-pkt-line `ReadPacket` traversal,
// (2) the `bytes.CutPrefix(payload, commandPrefixBytes)` scan, and
// (3) the dispatcher's ERR pkt-line emission.
//
// The `caps` axis varies the number of capability-echo pkt-lines the
// loop must traverse before the args-section delim. Real clients
// echo only the caps the server advertised that they care about
// (`agent`, plus at most a handful of others), so the 0/4/16 sweep
// covers the realistic distribution. The 16-row variant pins the
// budget for a worst-case echo flood — the value an attacker could
// drive against a server that does not bound its accepted echoes.
//
// `nil` is passed for the [objstore.Store[objfmt.SHA1Hash]] argument: the
// unknown-command path never dereferences it, and the alternative —
// materialising an empty fixture — would dilute the measurement
// with `t.TempDir`/copy cost the parse loop has nothing to do with.
//
// The request payload is built once outside the timed loop. Each
// iteration constructs a fresh [pktline.Reader] over a
// [bytes.NewReader]; both allocations are charged to the parse-loop
// budget the bench protects, matching the convention from
// `internal/wire/refs_v2_bench_test.go::BenchmarkDecodeLSRefs`.
func BenchmarkProcessV2Request(b *testing.B) {
	for _, capEchoes := range []int{0, 4, 16} {
		b.Run("caps="+strconv.Itoa(capEchoes), func(b *testing.B) {
			var req bytes.Buffer
			req.Write(pktBytes("command=benchmark-unknown\n"))
			for i := range capEchoes {
				req.Write(pktBytes("agent=bench-client/" + strconv.Itoa(i) + "\n"))
			}
			req.Write(delimBytes)
			req.Write(flushBytes)
			payload := req.Bytes()

			opts := Options{Agent: "test-agent/0.0"}
			w := pktline.NewWriter(io.Discard)

			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				r := pktline.NewReader(bytes.NewReader(payload))
				_, _ = processV2Request[objfmt.SHA1Hash](r, w, nil, opts)
			}
		})
	}
}

// benchDiscardTracer is a non-nil [trace.Tracer] whose methods drop
// every event. It exists so `BenchmarkRunCommand` can isolate the cost
// of the active-tracer path from the cost of the nil-tracer path
// (helper's nil-receiver short-circuit).
type benchDiscardTracer struct{}

func (benchDiscardTracer) OnPacketEvent(*trace.PacketEvent) {}
func (benchDiscardTracer) OnEvent(trace.Event)              {}

// BenchmarkRunCommand measures the wrap-overhead `runCommand` adds
// around each v2 command-handler dispatch. The handler is a no-op
// closure (`func() error { return nil }`) so every nanosecond
// captured here is overhead the dispatcher pays unconditionally:
//
//   - The two `time.Now()` calls (start + end). The doc comment on
//     `runCommand` already justifies them as cold-path work;
//     this bench quantifies the claim.
//   - The two `trace.Emit` calls. With a nil tracer the helper
//     short-circuits at `trace/emit.go:24`. With a non-nil tracer
//     the [trace.CommandEvent] value escapes to the heap (the
//     interface call hides the concrete type from escape analysis)
//     and `OnEvent` runs.
//
// Two sub-benches cover the design-relevant ends of the tracer
// axis:
//
//   - `nil` exercises the production no-tracer shape: the
//     `time.Now()` cost plus the `trace.Emit` nil checks.
//   - `discard` exercises the active-tracer shape: same
//     `time.Now()` cost plus the heap-allocated event and the
//     `OnEvent` interface call. The delta is the per-dispatch
//     observability tax.
func BenchmarkRunCommand(b *testing.B) {
	fn := func() error { return nil }

	b.Run("nil", func(b *testing.B) {
		opts := Options{}
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			_ = runCommand(opts, "ls-refs", fn)
		}
	})

	b.Run("discard", func(b *testing.B) {
		opts := Options{Tracer: benchDiscardTracer{}}
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			_ = runCommand(opts, "ls-refs", fn)
		}
	})
}

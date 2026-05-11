package wire

import (
	"bytes"
	"testing"

	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/trace"
)

// BenchmarkEncodeV2CommandRequest measures the steady-state cost of
// serialising a v2 command-request frame to an in-memory
// [pktline.Writer]. Every [transport.Conn.Command] invocation across
// every transport runs this once before the request hits the wire, so
// the per-call CPU shape on the encode side is what scales with caps +
// args fan.
//
// Sub-benches cover four request shapes that bracket the realistic
// distribution, mirroring `transport/http`'s `BenchmarkEncodeCommandBody`
// so cross-package regression comparisons line up:
//
//   - `ls-refs/plain`: the bare minimum — `command=ls-refs` followed
//     by the delim and flush. Establishes the per-call floor cost.
//   - `ls-refs/peel`: one capability echo, no args — the typical
//     read-only `ls-refs` shape.
//   - `ls-refs/peel+prefixes`: one cap plus a small fan of
//     `ref-prefix` args. Models a discovery filtered to two
//     namespaces.
//   - `object-info/100-oids`: 100 `oid` arg lines plus an
//     `object-format` cap. Stresses the per-arg loop in the
//     realistic worst case.
//
// Each shape runs twice on the tracer axis. The `tracer=nil` arm is
// the production no-tracing shape, where [pktline.Writer]'s emit path
// short-circuits before the option callback is invoked. The
// `tracer=discard` arm wires a non-nil tracer whose `OnEvent` returns
// immediately, isolating the per-pkt-line [trace.PacketEvent]
// emission cost (interface call + event copy onto the heap) from any
// consumer-side work. The delta between the two arms is the
// observability tax the encode path pays per command.
//
// Each iteration constructs a fresh [pktline.Writer] over a reused
// [bytes.Buffer] and resets the buffer between iterations: the
// allocator pressure measured is the encoder's, not the buffer's.
func BenchmarkEncodeV2CommandRequest(b *testing.B) {
	const oid = "0123456789abcdef0123456789abcdef01234567"
	manyOIDs := make([]string, 100)
	for i := range manyOIDs {
		manyOIDs[i] = "oid " + oid
	}

	cases := []struct {
		name       string
		cmd        string
		args, caps []string
	}{
		{"ls-refs/plain", "ls-refs", nil, nil},
		{"ls-refs/peel", "ls-refs", nil, []string{"peel"}},
		{
			"ls-refs/peel+prefixes",
			"ls-refs",
			[]string{"ref-prefix refs/heads/", "ref-prefix refs/tags/"},
			[]string{"peel"},
		},
		{"object-info/100-oids", "object-info", manyOIDs, []string{"object-format=sha1"}},
	}
	const redacted = "https://example.com/repo.git/git-upload-pack"

	for _, tc := range cases {
		b.Run(tc.name+"/tracer=nil", func(b *testing.B) {
			var buf bytes.Buffer
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				buf.Reset()
				w := pktline.NewWriter(&buf)
				if err := EncodeV2CommandRequest(w, tc.cmd, tc.args, tc.caps); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(tc.name+"/tracer=discard", func(b *testing.B) {
			var buf bytes.Buffer
			tr := benchDiscardTracer{}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				buf.Reset()
				w := pktline.NewWriter(&buf,
					pktline.WithWriterTracerURL(tr, trace.DirectionOutbound, redacted))
				if err := EncodeV2CommandRequest(w, tc.cmd, tc.args, tc.caps); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// benchDiscardTracer is a non-nil [trace.Tracer] whose methods drop
// every event. It exists so encode-path benches can isolate the cost
// of the active-tracer shape from the nil-tracer short-circuit in
// [pktline.Writer].
type benchDiscardTracer struct{}

func (benchDiscardTracer) OnPacketEvent(*trace.PacketEvent) {}
func (benchDiscardTracer) OnEvent(trace.Event)              {}

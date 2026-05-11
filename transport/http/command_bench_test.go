package httpt

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/hiddeco/go-ls-remote/internal/wire"
	"github.com/hiddeco/go-ls-remote/trace"
)

// BenchmarkEncodeCommandBody measures the cost of serialising a v2
// command-request to its on-wire `bytes.Buffer`. Each [Conn.Command]
// invocation runs this once before the POST is dispatched, so the
// scaling axis (caps + args count) is what determines per-command
// CPU on the encode side.
//
// Sub-benches cover four request shapes that bracket the realistic
// distribution:
//
//   - `ls-refs/plain`: the bare minimum — `command=ls-refs` followed
//     by the delim and flush. Establishes the per-call floor cost.
//   - `ls-refs/peel`: one capability echo, no args — the typical
//     read-only `ls-refs` shape.
//   - `ls-refs/peel+prefixes`: one cap plus a small fan of
//     ref-prefix args. Models a discovery filtered to two
//     namespaces.
//   - `object-info/100-oids`: 100 `oid` arg lines plus an
//     object-format cap. Stresses the per-arg [pktline.Writer]
//     loop in the realistic worst case.
//
// Each shape runs twice on the tracer axis. The `tracer=nil` arm
// exercises the production no-tracing shape; the `tracer=discard`
// arm wires a non-nil tracer whose `OnEvent` returns immediately,
// isolating the per-pkt-line [trace.PacketEvent] emission cost from
// any consumer-side work. The delta between the two arms is the
// observability tax the encode path pays per command.
func BenchmarkEncodeCommandBody(b *testing.B) {
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
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				_ = encodeCommandBody(tc.cmd, tc.args, tc.caps, nil, redacted)
			}
		})
		b.Run(tc.name+"/tracer=discard", func(b *testing.B) {
			tr := benchDiscardTracer{}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				_ = encodeCommandBody(tc.cmd, tc.args, tc.caps, tr, redacted)
			}
		})
	}
}

// BenchmarkConn_Command measures the steady-state cost of issuing a
// v2 command through [Conn.Command] with the network short-circuited
// by a stub round-tripper. What remains is the work the command path
// performs on every call regardless of where the bytes go: payload
// validation, the [commandPostURL] derivation, body encoding, the
// `*http.Request` plumbing, the pre-dispatch credential resolver
// step, the response-handling switch, and the close-and-replace
// bookkeeping on `cmdBody`.
//
// The stub round-tripper returns a fresh `200 OK` with [http.NoBody]
// for every iteration; that body's `Read` returns `(0, io.EOF)`
// immediately, so [drainAndClose] on the prior iteration's body is
// effectively free and the benchmark isolates the [Conn.Command]
// body path rather than the body teardown shape.
//
// Tracer is nil throughout — the encode-path tracer overhead is
// covered separately by `BenchmarkEncodeCommandBody`.
func BenchmarkConn_Command(b *testing.B) {
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
		{
			"ls-refs/peel+prefixes",
			"ls-refs",
			[]string{"ref-prefix refs/heads/", "ref-prefix refs/tags/"},
			[]string{"peel"},
		},
		{"object-info/100-oids", "object-info", manyOIDs, []string{"object-format=sha1"}},
	}

	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			u, err := url.Parse("https://example.com/repo.git/info/refs")
			if err != nil {
				b.Fatal(err)
			}
			c := &Conn{
				client:            &http.Client{Transport: benchOKRoundTripper{}},
				url:               u,
				userAgent:         wire.DefaultUserAgent,
				gitProtocolHeader: "version=2",
			}
			ctx := context.Background()

			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				rdr, err := c.Command(ctx, tc.cmd, tc.args, tc.caps)
				if err != nil {
					b.Fatal(err)
				}
				_ = rdr
			}
		})
	}
}

// benchOKRoundTripper short-circuits every request with a 200 OK
// response whose body is [http.NoBody]. Used by `BenchmarkConn_Command`
// so the measurement isolates the in-process command path from the
// network and the response-body shape.
type benchOKRoundTripper struct{}

func (benchOKRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       http.NoBody,
		Request:    req,
	}, nil
}

// benchDiscardTracer is a non-nil [trace.Tracer] whose `OnEvent`
// drops every event. It exists so encode-path benches can isolate
// the cost of the active-tracer shape (interface call + event copy
// onto the heap) from the cost of the nil-tracer shape (helper's
// nil-receiver short-circuit).
type benchDiscardTracer struct{}

func (benchDiscardTracer) OnEvent(trace.Event) {}

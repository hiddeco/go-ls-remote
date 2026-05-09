package wire

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/trace"
)

// captureTracer collects every emitted [trace.Event] for assertion in
// tests. It mirrors the `fakeTracer` pattern used by `trace/trace_test.go`.
type captureTracer struct {
	events []trace.Event
}

func (c *captureTracer) OnEvent(e trace.Event) {
	c.events = append(c.events, e)
}

// pkt builds the on-wire bytes for a data packet with the given payload
// using a fresh [pktline.Writer] so the encoded length prefix matches
// what the production encoder emits.
func pkt(t *testing.T, payload string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := pktline.NewWriter(&buf)
	require.NoError(t, w.WritePacket([]byte(payload)))
	return buf.Bytes()
}

func TestEncodeLSRefs(t *testing.T) {
	cases := []struct {
		name       string
		args       RefsArgs
		caps       RawCapabilities
		userAgent  string
		wantBody   func(t *testing.T) []byte
		wantDrop   bool
		wantReason string
	}{
		{
			name: "empty args, empty caps",
			args: RefsArgs{},
			caps: nil,
			wantBody: func(t *testing.T) []byte {
				var b bytes.Buffer
				b.Write(pkt(t, "command=ls-refs\n"))
				b.WriteString("0001")
				b.WriteString("0000")
				return b.Bytes()
			},
		},
		{
			name: "agent echo uses default user agent when empty",
			args: RefsArgs{},
			caps: RawCapabilities{{Name: "agent", Value: "git/2.45.0"}},
			wantBody: func(t *testing.T) []byte {
				var b bytes.Buffer
				b.Write(pkt(t, "command=ls-refs\n"))
				b.Write(pkt(t, "agent=go-ls-remote\n"))
				b.WriteString("0001")
				b.WriteString("0000")
				return b.Bytes()
			},
		},
		{
			name:      "agent override",
			args:      RefsArgs{},
			caps:      RawCapabilities{{Name: "agent", Value: "git/2.45.0"}},
			userAgent: "my-tool/1.0",
			wantBody: func(t *testing.T) []byte {
				var b bytes.Buffer
				b.Write(pkt(t, "command=ls-refs\n"))
				b.Write(pkt(t, "agent=my-tool/1.0\n"))
				b.WriteString("0001")
				b.WriteString("0000")
				return b.Bytes()
			},
		},
		{
			name: "agent boolean still echoes per server_supports_v2",
			args: RefsArgs{},
			caps: RawCapabilities{{Name: "agent"}},
			wantBody: func(t *testing.T) []byte {
				var b bytes.Buffer
				b.Write(pkt(t, "command=ls-refs\n"))
				b.Write(pkt(t, "agent=go-ls-remote\n"))
				b.WriteString("0001")
				b.WriteString("0000")
				return b.Bytes()
			},
		},
		{
			name: "object-format echo uses advertised value",
			args: RefsArgs{},
			caps: RawCapabilities{{Name: "object-format", Value: "sha256"}},
			wantBody: func(t *testing.T) []byte {
				var b bytes.Buffer
				b.Write(pkt(t, "command=ls-refs\n"))
				b.Write(pkt(t, "object-format=sha256\n"))
				b.WriteString("0001")
				b.WriteString("0000")
				return b.Bytes()
			},
		},
		{
			name: "object-format boolean is not echoed",
			args: RefsArgs{},
			caps: RawCapabilities{{Name: "object-format"}},
			wantBody: func(t *testing.T) []byte {
				var b bytes.Buffer
				b.Write(pkt(t, "command=ls-refs\n"))
				b.WriteString("0001")
				b.WriteString("0000")
				return b.Bytes()
			},
		},
		{
			name: "agent then object-format in canonical order",
			args: RefsArgs{},
			caps: RawCapabilities{
				{Name: "agent", Value: "git/2.45.0"},
				{Name: "object-format", Value: "sha1"},
			},
			wantBody: func(t *testing.T) []byte {
				var b bytes.Buffer
				b.Write(pkt(t, "command=ls-refs\n"))
				b.Write(pkt(t, "agent=go-ls-remote\n"))
				b.Write(pkt(t, "object-format=sha1\n"))
				b.WriteString("0001")
				b.WriteString("0000")
				return b.Bytes()
			},
		},
		{
			name: "peel only",
			args: RefsArgs{Peel: true},
			caps: nil,
			wantBody: func(t *testing.T) []byte {
				var b bytes.Buffer
				b.Write(pkt(t, "command=ls-refs\n"))
				b.WriteString("0001")
				b.Write(pkt(t, "peel\n"))
				b.WriteString("0000")
				return b.Bytes()
			},
		},
		{
			name: "symrefs and ref-prefixes preserve slice order",
			args: RefsArgs{
				Symrefs:  true,
				Prefixes: []string{"refs/heads/", "refs/tags/"},
			},
			caps: nil,
			wantBody: func(t *testing.T) []byte {
				var b bytes.Buffer
				b.Write(pkt(t, "command=ls-refs\n"))
				b.WriteString("0001")
				b.Write(pkt(t, "symrefs\n"))
				b.Write(pkt(t, "ref-prefix refs/heads/\n"))
				b.Write(pkt(t, "ref-prefix refs/tags/\n"))
				b.WriteString("0000")
				return b.Bytes()
			},
		},
		{
			name: "unborn allowed when ls-refs advertises unborn",
			args: RefsArgs{Unborn: true},
			caps: RawCapabilities{{Name: "ls-refs", Value: "unborn"}},
			wantBody: func(t *testing.T) []byte {
				var b bytes.Buffer
				b.Write(pkt(t, "command=ls-refs\n"))
				b.WriteString("0001")
				b.Write(pkt(t, "unborn\n"))
				b.WriteString("0000")
				return b.Bytes()
			},
		},
		{
			name: "unborn dropped when ls-refs is boolean only",
			args: RefsArgs{Unborn: true},
			caps: RawCapabilities{{Name: "ls-refs"}},
			wantBody: func(t *testing.T) []byte {
				var b bytes.Buffer
				b.Write(pkt(t, "command=ls-refs\n"))
				b.WriteString("0001")
				b.WriteString("0000")
				return b.Bytes()
			},
			wantDrop:   true,
			wantReason: "server did not advertise ls-refs=unborn",
		},
		{
			name: "unborn dropped when ls-refs advertises a different value",
			args: RefsArgs{Unborn: true},
			caps: RawCapabilities{{Name: "ls-refs", Value: "other"}},
			wantBody: func(t *testing.T) []byte {
				var b bytes.Buffer
				b.Write(pkt(t, "command=ls-refs\n"))
				b.WriteString("0001")
				b.WriteString("0000")
				return b.Bytes()
			},
			wantDrop:   true,
			wantReason: "server did not advertise ls-refs=unborn",
		},
		{
			name: "unborn not requested even when advertised",
			args: RefsArgs{Unborn: false},
			caps: RawCapabilities{{Name: "ls-refs", Value: "unborn"}},
			wantBody: func(t *testing.T) []byte {
				var b bytes.Buffer
				b.Write(pkt(t, "command=ls-refs\n"))
				b.WriteString("0001")
				b.WriteString("0000")
				return b.Bytes()
			},
		},
		{
			name: "full happy path",
			args: RefsArgs{
				Peel:     true,
				Symrefs:  true,
				Unborn:   true,
				Prefixes: []string{"refs/heads/", "refs/tags/"},
			},
			caps: RawCapabilities{
				{Name: "agent", Value: "git/2.45.0"},
				{Name: "object-format", Value: "sha1"},
				{Name: "ls-refs", Value: "unborn"},
			},
			userAgent: "my-tool/1.0",
			wantBody: func(t *testing.T) []byte {
				var b bytes.Buffer
				b.Write(pkt(t, "command=ls-refs\n"))
				b.Write(pkt(t, "agent=my-tool/1.0\n"))
				b.Write(pkt(t, "object-format=sha1\n"))
				b.WriteString("0001")
				b.Write(pkt(t, "peel\n"))
				b.Write(pkt(t, "symrefs\n"))
				b.Write(pkt(t, "unborn\n"))
				b.Write(pkt(t, "ref-prefix refs/heads/\n"))
				b.Write(pkt(t, "ref-prefix refs/tags/\n"))
				b.WriteString("0000")
				return b.Bytes()
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			w := pktline.NewWriter(&buf)
			tracer := &captureTracer{}

			require.NoError(t, EncodeLSRefs(w, tc.args, tc.caps, tc.userAgent, tracer))
			assert.Equal(t, tc.wantBody(t), buf.Bytes())

			if tc.wantDrop {
				require.Len(t, tracer.events, 1)
				ev, ok := tracer.events[0].(CapabilityDropEvent)
				require.True(t, ok, "expected CapabilityDropEvent, got %T", tracer.events[0])
				assert.Equal(t, "ls-refs", ev.Command)
				assert.Equal(t, "unborn", ev.Argument)
				assert.Equal(t, tc.wantReason, ev.Reason)
				assert.False(t, ev.Time.IsZero(), "Time should be populated")
			} else {
				assert.Empty(t, tracer.events)
			}
		})
	}
}

func TestEncodeLSRefsNilTracer(t *testing.T) {
	// Verify a nil tracer is safe — the unborn-drop site must not panic
	// when no tracer is wired up.
	var buf bytes.Buffer
	w := pktline.NewWriter(&buf)
	args := RefsArgs{Unborn: true}
	caps := RawCapabilities{{Name: "ls-refs"}}

	require.NoError(t, EncodeLSRefs(w, args, caps, "", nil))

	var want bytes.Buffer
	want.Write(pkt(t, "command=ls-refs\n"))
	want.WriteString("0001")
	want.WriteString("0000")
	assert.Equal(t, want.Bytes(), buf.Bytes())
}

func TestCapabilityDropEventWhen(t *testing.T) {
	now := time.Unix(1700000000, 0)
	e := CapabilityDropEvent{Time: now}
	assert.Equal(t, now, e.When())
}

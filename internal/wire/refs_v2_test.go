package wire

import (
	"bytes"
	"errors"
	"io"
	"iter"
	"strings"
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

// collectLSRefs drains seq into a slice and returns the first error
// yielded, if any. Decoder semantics guarantee the iterator stops after
// surfacing an error, so a single `lastErr` captures the failure mode.
func collectLSRefs(seq iter.Seq2[RawRef, error]) (refs []RawRef, lastErr error) {
	for ref, err := range seq {
		if err != nil {
			lastErr = err
			return
		}
		refs = append(refs, ref)
	}
	return
}

// buildLSRefsStream encodes payloads as data packets followed by a
// flush, matching the canonical v2 `ls-refs` response framing
// (`gitprotocol-v2.adoc` §"ls-refs"). Each payload should already
// carry its trailing LF.
func buildLSRefsStream(t *testing.T, payloads ...string) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	w := pktline.NewWriter(&buf)
	for _, p := range payloads {
		require.NoError(t, w.WritePacket([]byte(p)))
	}
	require.NoError(t, w.WriteFlush())
	return &buf
}

func TestDecodeLSRefs(t *testing.T) {
	const (
		oidMain = "1111111111111111111111111111111111111111"
		oidTag  = "2222222222222222222222222222222222222222"
		oidPeel = "3333333333333333333333333333333333333333"
		oidHEAD = "4444444444444444444444444444444444444444"
	)

	t.Run("empty (just flush)", func(t *testing.T) {
		var buf bytes.Buffer
		w := pktline.NewWriter(&buf)
		require.NoError(t, w.WriteFlush())

		refs, err := collectLSRefs(DecodeLSRefs(pktline.NewReader(&buf)))
		require.NoError(t, err)
		assert.Empty(t, refs)
	})

	t.Run("simple ref", func(t *testing.T) {
		buf := buildLSRefsStream(t, oidMain+" refs/heads/main\n")

		refs, err := collectLSRefs(DecodeLSRefs(pktline.NewReader(buf)))
		require.NoError(t, err)
		assert.Equal(t, []RawRef{{OID: oidMain, Name: "refs/heads/main"}}, refs)
	})

	t.Run("peeled tag", func(t *testing.T) {
		buf := buildLSRefsStream(t, oidTag+" refs/tags/v1 peeled:"+oidPeel+"\n")

		refs, err := collectLSRefs(DecodeLSRefs(pktline.NewReader(buf)))
		require.NoError(t, err)
		assert.Equal(t, []RawRef{{
			OID:    oidTag,
			Name:   "refs/tags/v1",
			Peeled: oidPeel,
		}}, refs)
	})

	t.Run("symref-target", func(t *testing.T) {
		buf := buildLSRefsStream(t, oidHEAD+" HEAD symref-target:refs/heads/main\n")

		refs, err := collectLSRefs(DecodeLSRefs(pktline.NewReader(buf)))
		require.NoError(t, err)
		assert.Equal(t, []RawRef{{
			OID:    oidHEAD,
			Name:   "HEAD",
			Symref: "refs/heads/main",
		}}, refs)
	})

	t.Run("peeled and symref both", func(t *testing.T) {
		// Either order is legal: `process_ref_v2` matches by prefix on
		// each token. Verify both arrangements parse to the same value.
		cases := []string{
			oidTag + " refs/tags/v1 peeled:" + oidPeel + " symref-target:refs/heads/main\n",
			oidTag + " refs/tags/v1 symref-target:refs/heads/main peeled:" + oidPeel + "\n",
		}
		for _, line := range cases {
			t.Run(line, func(t *testing.T) {
				buf := buildLSRefsStream(t, line)

				refs, err := collectLSRefs(DecodeLSRefs(pktline.NewReader(buf)))
				require.NoError(t, err)
				assert.Equal(t, []RawRef{{
					OID:    oidTag,
					Name:   "refs/tags/v1",
					Peeled: oidPeel,
					Symref: "refs/heads/main",
				}}, refs)
			})
		}
	})

	t.Run("unborn HEAD with symref-target", func(t *testing.T) {
		buf := buildLSRefsStream(t, "unborn HEAD symref-target:refs/heads/main\n")

		refs, err := collectLSRefs(DecodeLSRefs(pktline.NewReader(buf)))
		require.NoError(t, err)
		assert.Equal(t, []RawRef{{
			Name:   "HEAD",
			Symref: "refs/heads/main",
			Unborn: true,
		}}, refs)
	})

	t.Run("unborn HEAD without symref", func(t *testing.T) {
		buf := buildLSRefsStream(t, "unborn HEAD\n")

		refs, err := collectLSRefs(DecodeLSRefs(pktline.NewReader(buf)))
		require.NoError(t, err)
		assert.Equal(t, []RawRef{{Name: "HEAD", Unborn: true}}, refs)
	})

	t.Run("unknown attribute ignored", func(t *testing.T) {
		buf := buildLSRefsStream(t,
			oidMain+" refs/heads/main symref-target:refs/heads/main weird-attr:value\n")

		refs, err := collectLSRefs(DecodeLSRefs(pktline.NewReader(buf)))
		require.NoError(t, err)
		assert.Equal(t, []RawRef{{
			OID:    oidMain,
			Name:   "refs/heads/main",
			Symref: "refs/heads/main",
		}}, refs)
	})

	t.Run("multiple refs in stream order", func(t *testing.T) {
		buf := buildLSRefsStream(t,
			oidHEAD+" HEAD symref-target:refs/heads/main\n",
			oidMain+" refs/heads/main\n",
			oidTag+" refs/tags/v1 peeled:"+oidPeel+"\n",
		)

		refs, err := collectLSRefs(DecodeLSRefs(pktline.NewReader(buf)))
		require.NoError(t, err)
		assert.Equal(t, []RawRef{
			{OID: oidHEAD, Name: "HEAD", Symref: "refs/heads/main"},
			{OID: oidMain, Name: "refs/heads/main"},
			{OID: oidTag, Name: "refs/tags/v1", Peeled: oidPeel},
		}, refs)
	})

	t.Run("malformed: one token only", func(t *testing.T) {
		buf := buildLSRefsStream(t, oidMain+"\n")

		refs, err := collectLSRefs(DecodeLSRefs(pktline.NewReader(buf)))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "malformed")
		assert.Empty(t, refs)
	})

	t.Run("unexpected control packet (delim)", func(t *testing.T) {
		var buf bytes.Buffer
		w := pktline.NewWriter(&buf)
		require.NoError(t, w.WriteDelim())
		require.NoError(t, w.WriteFlush())

		refs, err := collectLSRefs(DecodeLSRefs(pktline.NewReader(&buf)))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unexpected control packet")
		assert.Empty(t, refs)
	})

	t.Run("server ERR mid-stream", func(t *testing.T) {
		buf := buildLSRefsStream(t, "ERR access denied\n")

		refs, err := collectLSRefs(DecodeLSRefs(pktline.NewReader(buf)))
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrServerRefused)
		assert.Contains(t, err.Error(), "access denied")
		assert.Empty(t, refs)
	})

	t.Run("ERR after a successful ref still surfaces error", func(t *testing.T) {
		buf := buildLSRefsStream(t,
			oidMain+" refs/heads/main\n",
			"ERR boom\n",
		)

		refs, err := collectLSRefs(DecodeLSRefs(pktline.NewReader(buf)))
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrServerRefused)
		assert.Contains(t, err.Error(), "boom")
		assert.Equal(t, []RawRef{{OID: oidMain, Name: "refs/heads/main"}}, refs)
	})

	t.Run("truncated stream (EOF before flush)", func(t *testing.T) {
		// One data packet, no flush, and no further bytes — the reader
		// returns `io.EOF` on the next packet read.
		var buf bytes.Buffer
		w := pktline.NewWriter(&buf)
		require.NoError(t, w.WritePacket([]byte(oidMain+" refs/heads/main\n")))

		refs, err := collectLSRefs(DecodeLSRefs(pktline.NewReader(&buf)))
		require.Error(t, err)
		assert.True(t, errors.Is(err, io.ErrUnexpectedEOF),
			"want io.ErrUnexpectedEOF, got %v", err)
		assert.Equal(t, []RawRef{{OID: oidMain, Name: "refs/heads/main"}}, refs)
	})

	t.Run("early break: yield false stops iteration", func(t *testing.T) {
		buf := buildLSRefsStream(t,
			oidHEAD+" HEAD symref-target:refs/heads/main\n",
			oidMain+" refs/heads/main\n",
			oidTag+" refs/tags/v1\n",
		)

		var seen []RawRef
		var loopErr error
		for ref, err := range DecodeLSRefs(pktline.NewReader(buf)) {
			if err != nil {
				loopErr = err
				break
			}
			seen = append(seen, ref)
			break // bail on first ref
		}
		require.NoError(t, loopErr)
		require.Len(t, seen, 1)
		assert.Equal(t, "HEAD", seen[0].Name)
	})

	t.Run("error message starts with wire prefix", func(t *testing.T) {
		// Spot-check that errors carry a `wire:` prefix per package
		// convention — not a load-bearing assertion, just a guard.
		buf := buildLSRefsStream(t, "ERR nope\n")

		_, err := collectLSRefs(DecodeLSRefs(pktline.NewReader(buf)))
		require.Error(t, err)
		assert.True(t, strings.HasPrefix(err.Error(), "wire:"),
			"err = %q", err.Error())
	})
}

package wire

import (
	"bytes"
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

// captureTracer collects every emitted event for assertion in tests.
type captureTracer struct {
	events []trace.Event
}

func (c *captureTracer) OnPacketEvent(e *trace.PacketEvent) {
	cloned := *e
	if cloned.Bytes != nil {
		cloned.Bytes = bytes.Clone(cloned.Bytes)
	}
	c.events = append(c.events, cloned)
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
	t.Parallel()

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
				b.Write(pkt(t, "agent="+DefaultUserAgent+"\n"))
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
				b.Write(pkt(t, "agent="+DefaultUserAgent+"\n"))
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
				b.Write(pkt(t, "agent="+DefaultUserAgent+"\n"))
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
			t.Parallel()

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
	t.Parallel()

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
	t.Parallel()

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
			return refs, lastErr
		}
		refs = append(refs, ref)
	}
	return refs, lastErr
}

// buildLSRefsStream encodes payloads as data packets followed by a
// flush, matching the canonical v2 `ls-refs` response framing
// ([gitprotocol-v2.adoc §"ls-refs"]). Each payload should already
// carry its trailing LF.
//
// [gitprotocol-v2.adoc §"ls-refs"]: https://github.com/git/git/blob/v2.54.0/Documentation/gitprotocol-v2.adoc#ls-refs
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
	t.Parallel()

	const (
		oidMain = "1111111111111111111111111111111111111111"
		oidTag  = "2222222222222222222222222222222222222222"
		oidPeel = "3333333333333333333333333333333333333333"
		oidHEAD = "4444444444444444444444444444444444444444"
	)

	t.Run("empty (just flush)", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer
		w := pktline.NewWriter(&buf)
		require.NoError(t, w.WriteFlush())

		refs, err := collectLSRefs(DecodeLSRefs(pktline.NewReader(&buf)))
		require.NoError(t, err)
		assert.Empty(t, refs)
	})

	t.Run("simple ref", func(t *testing.T) {
		t.Parallel()

		buf := buildLSRefsStream(t, oidMain+" refs/heads/main\n")

		refs, err := collectLSRefs(DecodeLSRefs(pktline.NewReader(buf)))
		require.NoError(t, err)
		assert.Equal(t, []RawRef{{OID: oidMain, Name: "refs/heads/main"}}, refs)
	})

	t.Run("peeled tag", func(t *testing.T) {
		t.Parallel()

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
		t.Parallel()

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
		t.Parallel()

		// Either order is legal: `process_ref_v2` matches by prefix on
		// each token. Verify both arrangements parse to the same value.
		cases := []string{
			oidTag + " refs/tags/v1 peeled:" + oidPeel + " symref-target:refs/heads/main\n",
			oidTag + " refs/tags/v1 symref-target:refs/heads/main peeled:" + oidPeel + "\n",
		}
		for _, line := range cases {
			t.Run(line, func(t *testing.T) {
				t.Parallel()

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
		t.Parallel()

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
		t.Parallel()

		buf := buildLSRefsStream(t, "unborn HEAD\n")

		refs, err := collectLSRefs(DecodeLSRefs(pktline.NewReader(buf)))
		require.NoError(t, err)
		assert.Equal(t, []RawRef{{Name: "HEAD", Unborn: true}}, refs)
	})

	t.Run("unknown attribute ignored", func(t *testing.T) {
		t.Parallel()

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
		t.Parallel()
		buf := buildLSRefsStream(
			t,
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
		t.Parallel()

		buf := buildLSRefsStream(t, oidMain+"\n")

		refs, err := collectLSRefs(DecodeLSRefs(pktline.NewReader(buf)))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "malformed")
		assert.Empty(t, refs)
	})

	t.Run("unexpected control packet (delim)", func(t *testing.T) {
		t.Parallel()

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
		t.Parallel()

		buf := buildLSRefsStream(t, "ERR access denied\n")

		refs, err := collectLSRefs(DecodeLSRefs(pktline.NewReader(buf)))
		require.Error(t, err)
		require.ErrorIs(t, err, ErrServerRefused)
		assert.Contains(t, err.Error(), "access denied")
		assert.Empty(t, refs)
	})

	t.Run("ERR after a successful ref still surfaces error", func(t *testing.T) {
		t.Parallel()
		buf := buildLSRefsStream(
			t,
			oidMain+" refs/heads/main\n",
			"ERR boom\n",
		)

		refs, err := collectLSRefs(DecodeLSRefs(pktline.NewReader(buf)))
		require.Error(t, err)
		require.ErrorIs(t, err, ErrServerRefused)
		assert.Contains(t, err.Error(), "boom")
		assert.Equal(t, []RawRef{{OID: oidMain, Name: "refs/heads/main"}}, refs)
	})

	t.Run("truncated stream (EOF before flush)", func(t *testing.T) {
		t.Parallel()

		// One data packet, no flush, and no further bytes — the reader
		// returns `io.EOF` on the next packet read.
		var buf bytes.Buffer
		w := pktline.NewWriter(&buf)
		require.NoError(t, w.WritePacket([]byte(oidMain+" refs/heads/main\n")))

		refs, err := collectLSRefs(DecodeLSRefs(pktline.NewReader(&buf)))
		require.Error(t, err)
		require.ErrorIs(t, err, io.ErrUnexpectedEOF,
			"want io.ErrUnexpectedEOF, got %v", err)
		assert.Equal(t, []RawRef{{OID: oidMain, Name: "refs/heads/main"}}, refs)
	})

	t.Run("early break: yield false stops iteration", func(t *testing.T) {
		t.Parallel()
		buf := buildLSRefsStream(
			t,
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
		t.Parallel()

		// Spot-check that errors carry a `wire:` prefix per package
		// convention — not a load-bearing assertion, just a guard.
		buf := buildLSRefsStream(t, "ERR nope\n")

		_, err := collectLSRefs(DecodeLSRefs(pktline.NewReader(buf)))
		require.Error(t, err)
		assert.True(t, strings.HasPrefix(err.Error(), "wire:"),
			"err = %q", err.Error())
	})
}

// readLSRefsRequest walks an encoded `ls-refs` request stream and
// returns the recovered argument set. The wire shape it expects
// mirrors `EncodeLSRefs` (and [gitprotocol-v2.adoc §"ls-refs"]
// command-request): a `command=ls-refs` data packet, zero or more
// capability-echo data packets, a `0001` delim, then the body of
// `peel`, `symrefs`, `unborn`, and `ref-prefix <p>` lines closed by a
// `0000` flush. A failure to match any of those constraints fails the
// test.
//
// Argument order is canonical (`peel`, `symrefs`, `unborn`, then
// `ref-prefix` in slice order) per [connect.c::get_remote_refs lines 564-597],
// so the helper records each flag independently rather than
// asserting position.
//
// [gitprotocol-v2.adoc §"ls-refs"]: https://github.com/git/git/blob/v2.54.0/Documentation/gitprotocol-v2.adoc#ls-refs
// [connect.c::get_remote_refs lines 564-597]: https://github.com/git/git/blob/v2.54.0/connect.c#L564-L597
func readLSRefsRequest(t *testing.T, raw []byte) (args RefsArgs) {
	t.Helper()

	r := pktline.NewReader(bytes.NewReader(raw))

	first, err := r.ReadPacket()
	require.NoError(t, err)
	require.Equal(t, pktline.Data, first.Kind)
	require.Equal(t, "command=ls-refs\n", string(first.Data))

	for {
		p, err := r.ReadPacket()
		require.NoError(t, err)
		if p.Kind == pktline.Delim {
			break
		}
		require.Equal(t, pktline.Data, p.Kind,
			"unexpected control packet in header: %v", p.Kind)
	}

	for {
		p, err := r.ReadPacket()
		require.NoError(t, err)
		if p.Kind == pktline.Flush {
			return args
		}
		require.Equal(t, pktline.Data, p.Kind,
			"unexpected control packet in body: %v", p.Kind)
		line := string(bytes.TrimSuffix(p.Data, []byte{'\n'}))
		switch {
		case line == "peel":
			args.Peel = true
		case line == "symrefs":
			args.Symrefs = true
		case line == "unborn":
			args.Unborn = true
		case strings.HasPrefix(line, "ref-prefix "):
			args.Prefixes = append(args.Prefixes,
				strings.TrimPrefix(line, "ref-prefix "))
		default:
			t.Fatalf("unrecognised body line %q", line)
		}
	}
}

// TestLSRefs_attrSemantics pins the v2 `ls-refs` ref-line attribute
// rules that `parseLSRefsLine` derives from
// [connect.c::process_ref_v2 lines 395-470]. The parent
// `TestDecodeLSRefs` exercises the happy path; this test locks the
// rules that make the decoder forward-compatible with future
// attributes:
//
//  1. Attribute order does not matter — `process_ref_v2` matches each
//     token by prefix in a left-to-right loop with no positional
//     constraint.
//  2. Repeated attributes follow last-wins. `process_ref_v2` calls
//     `xstrdup` on each match without breaking the loop, so a second
//     `symref-target:` overwrites the first. The Go decoder mirrors
//     this for `symref-target:` and applies the same last-wins rule
//     to `peeled:`. Canonical Git treats each `peeled:` as a separate
//     ref entry rather than overwriting a single field — that
//     divergence is a deliberate idiomatic choice (`Peeled` is a
//     scalar on `RawRef`), and the test below pins which token wins
//     in our representation.
//  3. Unknown attributes are silently dropped — the trailing `else`
//     arm in `process_ref_v2` is implicit (no `else` branch runs
//     after the two `skip_prefix` checks fail). New attributes added
//     by future Git versions therefore must not break older clients.
//
// [connect.c::process_ref_v2 lines 395-470]: https://github.com/git/git/blob/v2.54.0/connect.c#L395-L470
func TestLSRefs_attrSemantics(t *testing.T) {
	t.Parallel()

	const (
		oidTag   = "1111111111111111111111111111111111111111"
		oidPeel1 = "2222222222222222222222222222222222222222"
		oidPeel2 = "3333333333333333333333333333333333333333"
	)

	t.Run("attribute order independence", func(t *testing.T) {
		t.Parallel()

		// `process_ref_v2` walks tokens left-to-right with no position
		// dependency, so swapping the two attrs must not affect the
		// parsed value. This is the structural form of the existing
		// "peeled and symref both" sub-case, hoisted out so future
		// attribute additions can be slotted alongside without
		// touching the happy-path table.
		want := []RawRef{{
			OID:    oidTag,
			Name:   "refs/tags/v1",
			Peeled: oidPeel1,
			Symref: "refs/heads/main",
		}}
		orderings := []string{
			oidTag + " refs/tags/v1 peeled:" + oidPeel1 + " symref-target:refs/heads/main\n",
			oidTag + " refs/tags/v1 symref-target:refs/heads/main peeled:" + oidPeel1 + "\n",
		}
		for _, line := range orderings {
			t.Run(line, func(t *testing.T) {
				t.Parallel()

				buf := buildLSRefsStream(t, line)
				refs, err := collectLSRefs(DecodeLSRefs(pktline.NewReader(buf)))
				require.NoError(t, err)
				assert.Equal(t, want, refs)
			})
		}
	})

	t.Run("repeated peeled attribute: last wins", func(t *testing.T) {
		t.Parallel()

		// Two `peeled:` tokens on a single line. Canonical Git appends
		// each as its own peeled ref entry; our decoder collapses to a
		// single scalar `Peeled` field whose value is the rightmost
		// token (each loop iteration overwrites the previous via
		// `ref.Peeled = t`). The test pins last-wins so a future
		// refactor toward first-wins (or first-error) does not change
		// the contract silently.
		buf := buildLSRefsStream(t,
			oidTag+" refs/tags/v1 peeled:"+oidPeel1+" peeled:"+oidPeel2+"\n")

		refs, err := collectLSRefs(DecodeLSRefs(pktline.NewReader(buf)))
		require.NoError(t, err)
		assert.Equal(t, []RawRef{{
			OID:    oidTag,
			Name:   "refs/tags/v1",
			Peeled: oidPeel2,
		}}, refs)
	})

	t.Run("repeated symref-target attribute: last wins", func(t *testing.T) {
		t.Parallel()

		// `process_ref_v2` calls `ref->symref = xstrdup(arg)` without
		// `break`, so a second `symref-target:` clobbers the first.
		// The Go decoder follows the same rule via overwrite assignment.
		buf := buildLSRefsStream(t,
			oidTag+" HEAD symref-target:refs/heads/old symref-target:refs/heads/new\n")

		refs, err := collectLSRefs(DecodeLSRefs(pktline.NewReader(buf)))
		require.NoError(t, err)
		assert.Equal(t, []RawRef{{
			OID:    oidTag,
			Name:   "HEAD",
			Symref: "refs/heads/new",
		}}, refs)
	})

	t.Run("unknown attribute forward compat", func(t *testing.T) {
		t.Parallel()

		// A future Git version may emit an attribute the decoder does
		// not recognise. `process_ref_v2`'s loop runs `skip_prefix` for
		// each known prefix and silently falls through when none match;
		// the Go decoder mirrors this with the trailing-comment fallthrough.
		// The known attrs on the same line must still survive verbatim.
		buf := buildLSRefsStream(t,
			oidTag+" refs/tags/v1 frobnitz:foo peeled:"+oidPeel1+
				" symref-target:refs/heads/main bazqux:bar\n")

		refs, err := collectLSRefs(DecodeLSRefs(pktline.NewReader(buf)))
		require.NoError(t, err)
		assert.Equal(t, []RawRef{{
			OID:    oidTag,
			Name:   "refs/tags/v1",
			Peeled: oidPeel1,
			Symref: "refs/heads/main",
		}}, refs)
	})
}

// TestLSRefs_roundTrip pins the encode side of the v2 `ls-refs` codec
// against silent drift. `EncodeLSRefs` writes the client request
// (`command=ls-refs` header, capability echoes, `0001` delim, body of
// `peel`/`symrefs`/`unborn`/`ref-prefix` lines, flush) while
// `DecodeLSRefs` reads the server response (per-ref data packets
// terminated by flush) — the two are *not* mirror images of each
// other, the same asymmetry that motivated `TestObjectInfo_roundTrip`
// ([protocol-caps.c::cap_object_info] vs [protocol-caps.c::send_info]).
//
// Each case encodes a `RefsArgs` covering a combination of the three
// boolean flags, re-parses the produced pkt-line stream via
// `readLSRefsRequest`, and asserts the request shape survived the
// trip. The `unborn` cases supply a capability set advertising
// `ls-refs=unborn` so `EncodeLSRefs` does not gate the flag away — the
// gating itself is exercised by `TestEncodeLSRefs`.
//
// [protocol-caps.c::cap_object_info]: https://github.com/git/git/blob/v2.54.0/protocol-caps.c#L79
// [protocol-caps.c::send_info]: https://github.com/git/git/blob/v2.54.0/protocol-caps.c#L37
func TestLSRefs_roundTrip(t *testing.T) {
	t.Parallel()

	unbornCaps := RawCapabilities{{Name: "ls-refs", Value: "unborn"}}

	cases := []struct {
		name string
		args RefsArgs
		caps RawCapabilities
	}{
		{
			name: "no flags",
			args: RefsArgs{},
			caps: nil,
		},
		{
			name: "peel only",
			args: RefsArgs{Peel: true},
			caps: nil,
		},
		{
			name: "symrefs only",
			args: RefsArgs{Symrefs: true},
			caps: nil,
		},
		{
			name: "unborn only (capability advertised)",
			args: RefsArgs{Unborn: true},
			caps: unbornCaps,
		},
		{
			name: "peel and symrefs",
			args: RefsArgs{Peel: true, Symrefs: true},
			caps: nil,
		},
		{
			name: "peel and unborn (capability advertised)",
			args: RefsArgs{Peel: true, Unborn: true},
			caps: unbornCaps,
		},
		{
			name: "symrefs and unborn (capability advertised)",
			args: RefsArgs{Symrefs: true, Unborn: true},
			caps: unbornCaps,
		},
		{
			name: "all flags with prefixes (capability advertised)",
			args: RefsArgs{
				Peel:     true,
				Symrefs:  true,
				Unborn:   true,
				Prefixes: []string{"refs/heads/", "refs/tags/"},
			},
			caps: unbornCaps,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			w := pktline.NewWriter(&buf)
			require.NoError(t, EncodeLSRefs(w, tc.args, tc.caps, "", nil))

			got := readLSRefsRequest(t, buf.Bytes())
			assert.Equal(t, tc.args.Peel, got.Peel, "peel flag")
			assert.Equal(t, tc.args.Symrefs, got.Symrefs, "symrefs flag")
			assert.Equal(t, tc.args.Unborn, got.Unborn, "unborn flag")
			assert.Equal(t, tc.args.Prefixes, got.Prefixes, "ref-prefix order")
		})
	}
}

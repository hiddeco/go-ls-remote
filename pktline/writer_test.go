package pktline

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hiddeco/go-ls-remote/trace"
)

func TestWriter_WritePacket(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		payload []byte
		want    string
	}{
		{
			name:    "empty payload",
			payload: nil,
			want:    "0004",
		},
		{
			name:    "small payload",
			payload: []byte("hi\n"),
			want:    "0007hi\n",
		},
		{
			name:    "ASCII payload",
			payload: []byte("command=ls-refs\n"),
			want:    "0014command=ls-refs\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			w := NewWriter(&buf)
			require.NoError(t, w.WritePacket(tt.payload))
			assert.Equal(t, tt.want, buf.String())
		})
	}
}

func TestWriter_controlPackets(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		write func(*Writer) error
		want  string
	}{
		{"flush", (*Writer).WriteFlush, "0000"},
		{"delim", (*Writer).WriteDelim, "0001"},
		{"response-end", (*Writer).WriteResponseEnd, "0002"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			w := NewWriter(&buf)
			require.NoError(t, tt.write(w))
			assert.Equal(t, tt.want, buf.String())
		})
	}
}

// TestWriter_WritePacket_maxPayload exercises the largest payload size
// the format permits, exactly [MaxPayload] bytes. The on-wire length
// prefix is `fff0` (= MaxPayload + 4).
func TestWriter_WritePacket_maxPayload(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	w := NewWriter(&buf)

	require.NoError(t, w.WritePacket(bytes.Repeat([]byte{'a'}, MaxPayload)))
	assert.Equal(t, 4+MaxPayload, buf.Len())
	assert.True(t, bytes.HasPrefix(buf.Bytes(), []byte("fff0")),
		"length prefix = %q, want fff0", buf.Bytes()[:4])
}

// TestWriter_WritePacket_overflow verifies that payloads exceeding
// [MaxPayload] are rejected before any byte is written.
func TestWriter_WritePacket_overflow(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	w := NewWriter(&buf)

	err := w.WritePacket(bytes.Repeat([]byte{'a'}, MaxPayload+1))
	require.ErrorIs(t, err, ErrPayloadTooLarge)
	assert.Zero(t, buf.Len(), "no bytes should be written when payload is over the limit")
}

// errWriter is an [io.Writer] that always returns errWriterFail. It is
// used to confirm that write failures from the underlying writer
// propagate unchanged out of the pktline write methods.
type errWriter struct{}

var errWriterFail = errors.New("errWriter: forced failure")

func (errWriter) Write(_ []byte) (int, error) { return 0, errWriterFail }

// captureTracer records every [trace.PacketEvent] it receives, cloning
// the event so the caller can inspect it after subsequent emits reuse
// the same struct (see the lifetime contract on [trace.PacketEvent]).
type captureTracer struct {
	events []trace.PacketEvent
}

func (c *captureTracer) OnPacketEvent(e *trace.PacketEvent) {
	cloned := *e
	if e.Bytes != nil {
		cloned.Bytes = bytes.Clone(e.Bytes)
	}
	c.events = append(c.events, cloned)
}

func (c *captureTracer) OnEvent(trace.Event) {}

func TestWriter_WriteLine(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		s    string
		want string
	}{
		{
			name: "empty string",
			s:    "",
			want: "0005\n",
		},
		{
			name: "typical line",
			s:    "command=ls-refs",
			want: "0014command=ls-refs\n",
		},
		{
			name: "ASCII line with spaces",
			s:    "agent=test/1.0",
			want: "0013agent=test/1.0\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			w := NewWriter(&buf)
			require.NoError(t, w.WriteLine(tt.s))
			assert.Equal(t, tt.want, buf.String())
		})
	}
}

// TestWriter_WriteLine_overflow verifies the cap check rejects payloads
// (string + trailing `'\n'`) that exceed [MaxPayload] before any byte is
// written.
func TestWriter_WriteLine_overflow(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	w := NewWriter(&buf)

	// `s` plus the trailing newline is MaxPayload + 1.
	s := strings.Repeat("a", MaxPayload)
	err := w.WriteLine(s)
	require.ErrorIs(t, err, ErrPayloadTooLarge)
	assert.Zero(t, buf.Len(), "no bytes should be written when payload is over the limit")
}

// TestWriter_WriteLine_maxPayload exercises the largest payload size
// the format permits: a string of `MaxPayload - 1` bytes plus the
// trailing newline reaches exactly MaxPayload bytes of payload.
func TestWriter_WriteLine_maxPayload(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	w := NewWriter(&buf)

	require.NoError(t, w.WriteLine(strings.Repeat("a", MaxPayload-1)))
	assert.Equal(t, 4+MaxPayload, buf.Len())
	assert.True(t, bytes.HasPrefix(buf.Bytes(), []byte("fff0")),
		"length prefix = %q, want fff0", buf.Bytes()[:4])
	assert.Equal(t, byte('\n'), buf.Bytes()[buf.Len()-1])
}

// TestWriter_WriteLine_writeError confirms an error from the underlying
// writer propagates unchanged.
func TestWriter_WriteLine_writeError(t *testing.T) {
	t.Parallel()
	w := NewWriter(errWriter{})
	err := w.WriteLine("hello")
	require.ErrorIs(t, err, errWriterFail)
}

// TestWriter_WriteLine_tracerEmission verifies a tracer wired in via
// [WithWriterTracer] receives a `trace.PacketEvent` carrying the
// payload bytes (including the trailing `'\n'`) of each WriteLine call.
func TestWriter_WriteLine_tracerEmission(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	tr := &captureTracer{}
	w := NewWriter(&buf, WithWriterTracer(tr, trace.DirectionOutbound))

	require.NoError(t, w.WriteLine("command=ls-refs"))
	require.NoError(t, w.WriteLine(""))

	require.Len(t, tr.events, 2)
	assert.Equal(t, trace.PacketData, tr.events[0].Kind)
	assert.Equal(t, "command=ls-refs\n", string(tr.events[0].Bytes))
	assert.Equal(t, trace.PacketData, tr.events[1].Kind)
	assert.Equal(t, "\n", string(tr.events[1].Bytes))
}

func TestWriter_WriteLineParts(t *testing.T) {
	t.Parallel()
	t.Run("empty parts list", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		w := NewWriter(&buf)
		require.NoError(t, w.WriteLineParts())
		assert.Equal(t, "0005\n", buf.String())
	})

	t.Run("matches WritePacket for concatenated payload", func(t *testing.T) {
		t.Parallel()
		const oid = "9c52d0f2bbc8e3a141b1c0c83f7d5e6a2b3c4d5e"

		var lineBuf bytes.Buffer
		lineWriter := NewWriter(&lineBuf)
		require.NoError(t, lineWriter.WriteLineParts("oid ", oid))

		var packetBuf bytes.Buffer
		packetWriter := NewWriter(&packetBuf)
		require.NoError(t, packetWriter.WritePacket([]byte("oid "+oid+"\n")))

		assert.Equal(t, packetBuf.Bytes(), lineBuf.Bytes())
	})

	t.Run("three-part payload", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		w := NewWriter(&buf)
		require.NoError(t, w.WriteLineParts("ref-prefix ", "refs/", "tags/"))
		assert.Equal(t, "001aref-prefix refs/tags/\n", buf.String())
	})
}

// TestWriter_WriteLineParts_overflow verifies the cap check sums the
// part lengths plus the trailing newline and rejects payloads that
// exceed [MaxPayload] before any byte is written.
func TestWriter_WriteLineParts_overflow(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	w := NewWriter(&buf)

	half := strings.Repeat("a", MaxPayload/2)
	rest := strings.Repeat("b", MaxPayload-len(half)+1)

	err := w.WriteLineParts(half, rest)
	require.ErrorIs(t, err, ErrPayloadTooLarge)
	assert.Zero(t, buf.Len(), "no bytes should be written when payload is over the limit")
}

// TestWriter_WriteLineParts_writeError confirms an error from the
// underlying writer propagates unchanged.
func TestWriter_WriteLineParts_writeError(t *testing.T) {
	t.Parallel()
	w := NewWriter(errWriter{})
	err := w.WriteLineParts("oid ", "deadbeef")
	require.ErrorIs(t, err, errWriterFail)
}

// TestWriter_WriteLineParts_tracerEmission verifies a tracer wired in
// via [WithWriterTracer] receives a `trace.PacketEvent` carrying the
// concatenated payload bytes (including the trailing `'\n'`).
func TestWriter_WriteLineParts_tracerEmission(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	tr := &captureTracer{}
	w := NewWriter(&buf, WithWriterTracer(tr, trace.DirectionOutbound))

	require.NoError(t, w.WriteLineParts("oid ", "deadbeef"))

	require.Len(t, tr.events, 1)
	assert.Equal(t, trace.PacketData, tr.events[0].Kind)
	assert.Equal(t, "oid deadbeef\n", string(tr.events[0].Bytes))
}

// TestWriter_roundTrip sends a small request through [NewWriter] and
// reads it back through [NewReader], exercising both codecs together.
func TestWriter_roundTrip(t *testing.T) {
	t.Parallel()
	inputs := []string{
		"command=ls-refs\n",
		"agent=test/1.0\n",
		"ref-prefix refs/tags/\n",
	}

	var buf bytes.Buffer
	w := NewWriter(&buf)
	for _, p := range inputs {
		require.NoError(t, w.WritePacket([]byte(p)))
	}
	require.NoError(t, w.WriteFlush())

	r := NewReader(strings.NewReader(buf.String()))
	for _, want := range inputs {
		pkt, err := r.ReadPacket()
		require.NoError(t, err)
		assert.Equal(t, Data, pkt.Kind)
		assert.Equal(t, want, string(pkt.Data))
	}
	pkt, err := r.ReadPacket()
	require.NoError(t, err)
	assert.Equal(t, Flush, pkt.Kind)
}

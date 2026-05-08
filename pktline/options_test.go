package pktline

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hiddeco/go-ls-remote/trace"
)

// capturingTracer is a minimal [trace.Tracer] that records every event
// it receives. It exists only as a test fixture in this file.
type capturingTracer struct {
	events []trace.Event
}

func (c *capturingTracer) OnEvent(e trace.Event) {
	c.events = append(c.events, e)
}

func TestReader_emitsPacketEvents(t *testing.T) {
	tr := &capturingTracer{}
	r := NewReader(
		strings.NewReader("0007hi\n0000"),
		WithReaderTracer(tr, trace.DirectionInbound),
	)

	_, err := r.ReadPacket()
	require.NoError(t, err)
	_, err = r.ReadPacket()
	require.NoError(t, err)

	require.Len(t, tr.events, 2)

	t.Run("data packet", func(t *testing.T) {
		ev, ok := tr.events[0].(trace.PacketEvent)
		require.True(t, ok, "got %T, want trace.PacketEvent", tr.events[0])
		assert.Equal(t, trace.DirectionInbound, ev.Direction)
		assert.Equal(t, trace.PacketData, ev.Kind)
		assert.Equal(t, "hi\n", string(ev.Bytes))
	})

	t.Run("flush packet", func(t *testing.T) {
		ev, ok := tr.events[1].(trace.PacketEvent)
		require.True(t, ok, "got %T, want trace.PacketEvent", tr.events[1])
		assert.Equal(t, trace.PacketFlush, ev.Kind)
		assert.Nil(t, ev.Bytes)
	})
}

func TestReader_nilTracerNoEmit(t *testing.T) {
	// Without WithReaderTracer the reader proceeds without invoking any
	// tracer; the test simply verifies no panic.
	r := NewReader(strings.NewReader("0007hi\n"))
	_, err := r.ReadPacket()
	require.NoError(t, err)
}

func TestReader_WithReaderTracerURL(t *testing.T) {
	tr := &capturingTracer{}
	r := NewReader(
		strings.NewReader("0007hi\n"),
		WithReaderTracerURL(tr, trace.DirectionInbound, "https://example.com/repo"),
	)

	_, err := r.ReadPacket()
	require.NoError(t, err)
	require.Len(t, tr.events, 1)

	ev := tr.events[0].(trace.PacketEvent)
	assert.Equal(t, "https://example.com/repo", ev.URL)
}

func TestWriter_emitsPacketEvents(t *testing.T) {
	tr := &capturingTracer{}
	var buf bytes.Buffer
	w := NewWriter(&buf, WithWriterTracer(tr, trace.DirectionOutbound))

	require.NoError(t, w.WritePacket([]byte("hi\n")))
	require.NoError(t, w.WriteFlush())

	require.Len(t, tr.events, 2)

	t.Run("data write", func(t *testing.T) {
		ev := tr.events[0].(trace.PacketEvent)
		assert.Equal(t, trace.DirectionOutbound, ev.Direction)
		assert.Equal(t, trace.PacketData, ev.Kind)
		assert.Equal(t, "hi\n", string(ev.Bytes))
	})

	t.Run("flush write", func(t *testing.T) {
		ev := tr.events[1].(trace.PacketEvent)
		assert.Equal(t, trace.PacketFlush, ev.Kind)
		assert.Nil(t, ev.Bytes)
	})
}

func TestWriter_WritePacketOverflowDoesNotEmit(t *testing.T) {
	tr := &capturingTracer{}
	var buf bytes.Buffer
	w := NewWriter(&buf, WithWriterTracer(tr, trace.DirectionOutbound))

	err := w.WritePacket(bytes.Repeat([]byte{'a'}, MaxPayload+1))
	require.Error(t, err)
	assert.Empty(t, tr.events, "no event should be emitted when WritePacket fails validation")
}

func Test_kindToTracerKind(t *testing.T) {
	tests := []struct {
		name string
		in   Kind
		want trace.PacketKind
	}{
		{"data", Data, trace.PacketData},
		{"flush", Flush, trace.PacketFlush},
		{"delim", Delim, trace.PacketDelim},
		{"response-end", ResponseEnd, trace.PacketResponseEnd},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, kindToTracerKind(tt.in))
		})
	}
}

package trace

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestNewWriterTracer asserts that the writer tracer's output contains
// expected substrings for each built-in event kind. The output format
// is documented as not stable, so tests verify presence of identifying
// fragments rather than an exact format string.
//
// Packet events are dispatched through OnPacketEvent; all other events
// flow through OnEvent. The two dispatch paths are exercised separately.
func TestNewWriterTracer(t *testing.T) {
	t.Parallel()

	at := time.Unix(0, 0)

	t.Run("packet-data-outbound", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer
		tr := NewWriterTracer(&buf)
		e := PacketEvent{
			Time: at, Direction: DirectionOutbound,
			URL:   "https://example.com/repo",
			Bytes: []byte("hello"), Kind: PacketData,
		}
		tr.OnPacketEvent(&e)
		out := buf.String()
		for _, s := range []string{"packet", ">"} {
			assert.Contains(t, out, s, "output: %q", out)
		}
	})

	t.Run("packet-flush-inbound", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer
		tr := NewWriterTracer(&buf)
		e := PacketEvent{Time: at, Direction: DirectionInbound, Kind: PacketFlush}
		tr.OnPacketEvent(&e)
		out := buf.String()
		assert.Contains(t, out, "flush", "output: %q", out)
	})

	onEventCases := []struct {
		name     string
		event    Event
		contains []string
	}{
		{
			name: "http",
			event: HTTPEvent{
				Time: at, Method: "GET", URL: "https://example.com/info/refs",
				Status: 200, Duration: 5 * time.Millisecond,
			},
			contains: []string{"http", "GET", "200"},
		},
		{
			name: "negotiate",
			event: NegotiateEvent{
				Time: at, URL: "https://example.com/repo", Version: 2,
				ServerAgent: "git/2.45", Capabilities: []string{"ls-refs"},
			},
			contains: []string{"negotiate", "v=2", "git/2.45"},
		},
		{
			name: "command-end",
			event: CommandEvent{
				Time: at, URL: "https://example.com/repo", Name: "ls-refs",
				Phase: CommandEnd, Duration: 1 * time.Millisecond,
			},
			contains: []string{"command", "ls-refs", "end"},
		},
	}
	for _, tt := range onEventCases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			tr := NewWriterTracer(&buf)
			tr.OnEvent(tt.event)
			out := buf.String()
			for _, s := range tt.contains {
				assert.Contains(t, out, s, "output: %q", out)
			}
		})
	}
}

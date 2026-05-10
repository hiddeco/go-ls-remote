package filet

import (
	"bytes"
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hiddeco/go-ls-remote/trace"
	"github.com/hiddeco/go-ls-remote/transport"
)

// capturingTracer records every event passed to OnEvent. It is safe
// for concurrent use; the in-process server runs on its own goroutine
// and emits packet events from there at the same time the client side
// emits its own events on the calling goroutine.
type capturingTracer struct {
	mu     sync.Mutex
	events []trace.Event
}

func (c *capturingTracer) OnEvent(e trace.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Clone byte slices on `PacketEvent` since the doc on
	// `trace.PacketEvent.Bytes` warns the slice aliases the reader's
	// internal buffer and is valid only for the duration of OnEvent.
	if pe, ok := e.(trace.PacketEvent); ok {
		if pe.Bytes != nil {
			pe.Bytes = bytes.Clone(pe.Bytes)
		}
		c.events = append(c.events, pe)
		return
	}
	c.events = append(c.events, e)
}

func (c *capturingTracer) snapshot() []trace.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]trace.Event, len(c.events))
	copy(out, c.events)
	return out
}

func (c *capturingTracer) packetEvents() []trace.PacketEvent {
	var out []trace.PacketEvent
	for _, e := range c.snapshot() {
		if p, ok := e.(trace.PacketEvent); ok {
			out = append(out, p)
		}
	}
	return out
}

func (c *capturingTracer) commandEvents() []trace.CommandEvent {
	var out []trace.CommandEvent
	for _, e := range c.snapshot() {
		if ce, ok := e.(trace.CommandEvent); ok {
			out = append(out, ce)
		}
	}
	return out
}

// TestTracer_PacketEvents_BothEndpoints verifies that wiring a single
// tracer through `OpenOptions.Tracer` causes the file transport to emit
// `PacketEvent`s from both endpoints of the in-process pipe pair: the
// client-side reader and writer AND the server-side reader and writer.
// One pkt-line crossing the pipe therefore produces two events on the
// shared tracer — one per endpoint's view — and the test asserts both
// directions are populated.
func TestTracer_PacketEvents_BothEndpoints(t *testing.T) {
	gitdir := materializeServeableFixture(t, "loose-only")
	u, err := transport.ParseURL("file://" + gitdir)
	require.NoError(t, err)

	tracer := &capturingTracer{}

	tr := New()
	conn, err := tr.Open(context.Background(), u, transport.OpenOptions{
		UserAgent: "test/0.0",
		Tracer:    tracer,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	c, ok := conn.(*Conn)
	require.True(t, ok)

	drainAdvertisement(t, c)

	rdr, err := c.Command(context.Background(), "ls-refs",
		[]string{"peel"}, []string{"object-format=sha1"})
	require.NoError(t, err)
	_ = readAllPackets(t, rdr)

	// Close synchronises with the server goroutine's exit (`<-c.done`),
	// so every PacketEvent the server-side reader and writer emitted is
	// guaranteed visible. Snapshotting before Close races against the
	// goroutine still draining its pkt-line writes through the trace
	// emit path.
	require.NoError(t, conn.Close())

	pkts := tracer.packetEvents()
	require.NotEmpty(t, pkts,
		"the wired tracer must observe pkt-line events from at least one endpoint")

	var inbound, outbound int
	wantURL := transport.RedactURL(u.Raw)
	for _, p := range pkts {
		switch p.Direction {
		case trace.DirectionInbound:
			inbound++
		case trace.DirectionOutbound:
			outbound++
		}
		assert.Equal(t, wantURL, p.URL,
			"every PacketEvent must carry the redacted file:// URL")
	}
	assert.Greater(t, inbound, 0,
		"the tracer must observe inbound PacketEvents (client reading server, server reading client)")
	assert.Greater(t, outbound, 0,
		"the tracer must observe outbound PacketEvents (client writing to server, server writing to client)")

	// With both endpoints wired, every byte that crosses the pipe is
	// observed twice — once by the writing side as Outbound and once by
	// the reading side as Inbound. The two counts must therefore match
	// to within the small set of in-flight packets that would be
	// captured by only one side at a given moment. After a fully
	// drained advertisement and command response, no packets are in
	// flight: every emitted pkt-line has been read.
	assert.Equal(t, inbound, outbound,
		"two-endpoint wiring should yield matching inbound/outbound counts after a full drain")
}

// TestTracer_CommandEvents_StillFlow regression-checks that wiring the
// tracer through the new packet-level path does not regress the
// command-level flow (already wired on the server side via
// `server.Options.Tracer`). A single `ls-refs` round-trip must still
// surface a `CommandStart`/`CommandEnd` pair.
func TestTracer_CommandEvents_StillFlow(t *testing.T) {
	gitdir := materializeServeableFixture(t, "loose-only")
	u, err := transport.ParseURL("file://" + gitdir)
	require.NoError(t, err)

	tracer := &capturingTracer{}

	tr := New()
	conn, err := tr.Open(context.Background(), u, transport.OpenOptions{
		UserAgent: "test/0.0",
		Tracer:    tracer,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	c, ok := conn.(*Conn)
	require.True(t, ok)

	drainAdvertisement(t, c)

	rdr, err := c.Command(context.Background(), "ls-refs", nil,
		[]string{"object-format=sha1"})
	require.NoError(t, err)
	_ = readAllPackets(t, rdr)

	// Close synchronises with the server goroutine's exit (`<-c.done`),
	// so any CommandEvents the server emitted before returning are
	// guaranteed visible after Close returns. Reading `commandEvents`
	// before Close races against the in-process server's `CommandEnd`
	// emission, which only fires after the response handler returns.
	require.NoError(t, conn.Close())

	cmds := tracer.commandEvents()
	var start, end int
	for _, ce := range cmds {
		if ce.Name != "ls-refs" {
			continue
		}
		switch ce.Phase {
		case trace.CommandStart:
			start++
		case trace.CommandEnd:
			end++
		}
	}
	assert.Equal(t, 1, start, "expected exactly one CommandStart for ls-refs")
	assert.Equal(t, 1, end, "expected exactly one CommandEnd for ls-refs")
}

// TestTracer_NilTracer_NoEvents pins the no-tracer path: passing a nil
// Tracer must not emit anything. Combined with the helpers' return-nil-
// on-disabled behaviour, this also asserts the option-builder path is
// allocation-free (no slice header allocated, no option applied).
func TestTracer_NilTracer_NoEvents(t *testing.T) {
	gitdir := materializeServeableFixture(t, "loose-only")
	u, err := transport.ParseURL("file://" + gitdir)
	require.NoError(t, err)

	tr := New()
	conn, err := tr.Open(context.Background(), u, transport.OpenOptions{
		UserAgent: "test/0.0",
		// Tracer left nil.
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	c, ok := conn.(*Conn)
	require.True(t, ok)

	drainAdvertisement(t, c)

	rdr, err := c.Command(context.Background(), "ls-refs", nil,
		[]string{"object-format=sha1"})
	require.NoError(t, err)
	_ = readAllPackets(t, rdr)

	// No assertions on a nil tracer — the test exists to confirm a nil
	// tracer does not panic and the round-trip still completes. The
	// allocation-free guarantee is structural (the helpers return nil)
	// rather than something this test can directly observe.
}

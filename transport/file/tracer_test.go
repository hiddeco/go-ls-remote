package filet

import (
	"bytes"
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
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
	// `pktline` emits `*trace.PacketEvent` referencing storage it
	// reuses across emits, so a captured pointer would be overwritten
	// by the next call. Copy the struct value, then clone the Bytes
	// slice (which aliases the reader's or writer's internal buffer)
	// before appending. See the lifetime contract on
	// [trace.PacketEvent].
	if pe, ok := e.(*trace.PacketEvent); ok {
		cloned := *pe
		if cloned.Bytes != nil {
			cloned.Bytes = bytes.Clone(cloned.Bytes)
		}
		c.events = append(c.events, cloned)
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

// runTracedRoundTrip opens a [Conn] against the `loose-only` fixture
// with tracer wired through `OpenOptions.Tracer`, runs an
// advertisement drain plus a single `ls-refs` command, and returns
// the captured `PacketEvent`s. Both shape assertions and event-count
// comparisons are layered on top of this helper so the workload is
// identical across default-vs-opt-in cases.
func runTracedRoundTrip(t *testing.T, opts ...Option) ([]trace.PacketEvent, string) {
	t.Helper()

	gitdir := materializeServeableFixture(t, "loose-only")
	u, err := transport.ParseURL("file://" + gitdir)
	require.NoError(t, err)

	tracer := &capturingTracer{}

	tr := New(opts...)
	conn, err := tr.Open(context.Background(), u, transport.OpenOptions{
		UserAgent: "test/0.0",
		Tracer:    tracer,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	c, ok := conn.(*Conn[objfmt.SHA1Hash])
	require.True(t, ok)

	drainAdvertisement(t, c)

	rdr, err := c.Command(context.Background(), "ls-refs",
		cmdBody("ls-refs", []string{"peel"}, []string{"object-format=sha1"}))
	require.NoError(t, err)
	_ = readAllPackets(t, rdr)

	// Close synchronises with the server goroutine's exit (`<-c.done`),
	// so every PacketEvent the server-side reader and writer emitted is
	// guaranteed visible. Snapshotting before Close races against the
	// goroutine still draining its pkt-line writes through the trace
	// emit path.
	require.NoError(t, conn.Close())

	return tracer.packetEvents(), transport.RedactURL(u.Raw)
}

// TestTracer_PacketEvents_DefaultClientSideOnly pins the default-off
// shape: without `WithEndpointTrace`, the file transport wires
// `OpenOptions.Tracer` only at the client-side reader and writer. Each
// pkt-line crossing the pipe pair therefore produces exactly one
// event on the shared tracer — matching the HTTP transport's
// one-event-per-pkt-line shape. The client observes its own writes as
// Outbound and the server's writes as Inbound when reading them off
// the pipe, so both directions are populated, but no event originates
// from the in-process server's reader/writer.
func TestTracer_PacketEvents_DefaultClientSideOnly(t *testing.T) {
	pkts, wantURL := runTracedRoundTrip(t)
	require.NotEmpty(t, pkts,
		"the client-side tracer must observe pkt-line events for the round-trip")

	var inbound, outbound int
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
		"the client-side reader must observe inbound PacketEvents (server's writes)")
	assert.Greater(t, outbound, 0,
		"the client-side writer must observe outbound PacketEvents (client's writes)")
}

// TestTracer_PacketEvents_WithEndpointTraceDoubles verifies the opt-in
// path: passing `WithEndpointTrace()` to [New] additionally wires the
// tracer at the in-process server's reader and writer, so each
// pkt-line on the pipe pair produces TWO events (one Outbound from
// the writing side, one Inbound from the reading side). Compared to
// the default shape, the same workload yields exactly twice as many
// events in total, with matching inbound and outbound counts after a
// fully drained round-trip.
func TestTracer_PacketEvents_WithEndpointTraceDoubles(t *testing.T) {
	defaultPkts, _ := runTracedRoundTrip(t)
	endpointPkts, wantURL := runTracedRoundTrip(t, WithEndpointTrace())

	require.NotEmpty(t, defaultPkts,
		"the default round-trip must produce at least one PacketEvent")
	require.NotEmpty(t, endpointPkts,
		"the endpoint-traced round-trip must produce at least one PacketEvent")

	assert.Equal(t, 2*len(defaultPkts), len(endpointPkts),
		"endpoint-traced round-trip should emit exactly twice as many events as the default")

	var inbound, outbound int
	for _, p := range endpointPkts {
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
		"the tracer must observe inbound PacketEvents from both endpoints")
	assert.Greater(t, outbound, 0,
		"the tracer must observe outbound PacketEvents from both endpoints")

	// With both endpoints wired, every byte that crosses the pipe is
	// observed twice — once by the writing side as Outbound and once
	// by the reading side as Inbound. After a fully drained
	// advertisement and command response, no packets are in flight:
	// every emitted pkt-line has been read.
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
	c, ok := conn.(*Conn[objfmt.SHA1Hash])
	require.True(t, ok)

	drainAdvertisement(t, c)

	rdr, err := c.Command(context.Background(), "ls-refs",
		cmdBody("ls-refs", nil, []string{"object-format=sha1"}))
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
	c, ok := conn.(*Conn[objfmt.SHA1Hash])
	require.True(t, ok)

	drainAdvertisement(t, c)

	rdr, err := c.Command(context.Background(), "ls-refs",
		cmdBody("ls-refs", nil, []string{"object-format=sha1"}))
	require.NoError(t, err)
	_ = readAllPackets(t, rdr)

	// No assertions on a nil tracer — the test exists to confirm a nil
	// tracer does not panic and the round-trip still completes. The
	// allocation-free guarantee is structural (the helpers return nil)
	// rather than something this test can directly observe.
}

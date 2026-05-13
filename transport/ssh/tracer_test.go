package ssht

import (
	"bytes"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hiddeco/go-ls-remote/trace"
	"github.com/hiddeco/go-ls-remote/transport"
)

// capturingTracer records every event passed to `OnEvent` or
// `OnPacketEvent`. It is safe for concurrent use; the test server runs
// on its own goroutines and emits packet events from there at the same
// time the client side emits its own events on the calling goroutine.
//
// The shape mirrors the one used by the file and HTTP transport test
// suites — kept package-private here rather than centralised because
// tracer-test helpers are conventionally private to the transport's
// own test package.
type capturingTracer struct {
	mu     sync.Mutex
	events []trace.Event
}

func (c *capturingTracer) OnPacketEvent(e *trace.PacketEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cloned := *e
	if cloned.Bytes != nil {
		cloned.Bytes = bytes.Clone(cloned.Bytes)
	}
	c.events = append(c.events, cloned)
}

func (c *capturingTracer) OnEvent(e trace.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
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

// openBridgedConnTraced is the tracer-aware sibling of
// `openBridgedConn` in `command_test.go`. It opens a [Conn] against
// the `loose-only` fixture with tracer wired through
// `transport.OpenOptions.Tracer`, drains the advertisement, and
// returns both the typed [Conn] and the redacted URL the events
// should carry. Keeping the helper local — rather than exporting it
// from `command_test.go` — avoids tangling the tracer assertions
// with the command-only tests that already pass `defaultOpenOptions`.
func openBridgedConnTraced(t *testing.T, tracer trace.Tracer) (*Conn, string) {
	t.Helper()

	bridge := bridgeSHA1Store(t, "loose-only")
	srv := newTestServer(t, testServerOpts{
		acceptEnv: true,
		serveStore: func() bridgeServer {
			return bridge
		},
	})

	tr := New(
		WithAuth(Signer(srv.clientSigner)),
		WithKnownHosts(srv.hostKeyCallback()),
	)
	u := srv.URL()
	conn, err := tr.Open(t.Context(), u, transport.OpenOptions{
		UserAgent: "test/0.0",
		Tracer:    tracer,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	c, ok := conn.(*Conn)
	require.True(t, ok)
	drainAdvertisement(t, c)
	return c, transport.RedactURL(u.Raw)
}

// TestTracer_PacketEvents_BothDirections pins the contract: a tracer
// wired through `transport.OpenOptions.Tracer` observes
// [trace.PacketEvent] values on both directions across a full
// round-trip. Outbound covers the initial extra-parameter pkt-line
// plus the v2 command-request frames; inbound covers the
// advertisement plus the command response.
//
// The assertion is structural ("at least one of each direction") rather
// than counting exact events: pkt-line framing can shift across
// refactors, but the wiring contract is invariant.
func TestTracer_PacketEvents_BothDirections(t *testing.T) {
	t.Parallel()

	tracer := &capturingTracer{}
	c, wantURL := openBridgedConnTraced(t, tracer)

	rdr, err := c.Command(t.Context(), "ls-refs",
		cmdBody("ls-refs", []string{"peel"}, []string{"object-format=sha1"}))
	require.NoError(t, err)
	_ = readAllPackets(t, rdr)

	// Close synchronises the session-wait goroutine's exit so every
	// PacketEvent the writer flushed before close is visible.
	require.NoError(t, c.Close())

	pkts := tracer.packetEvents()
	require.NotEmpty(t, pkts,
		"the tracer must observe pkt-line events for the round-trip")

	var inbound, outbound int
	for _, p := range pkts {
		switch p.Direction {
		case trace.DirectionInbound:
			inbound++
		case trace.DirectionOutbound:
			outbound++
		}
		assert.Equal(t, wantURL, p.URL,
			"every PacketEvent must carry the redacted ssh:// URL")
	}
	assert.Positive(t, inbound,
		"the reader must observe inbound PacketEvents (server writes)")
	assert.Positive(t, outbound,
		"the writer must observe outbound PacketEvents (client writes)")
}

// TestTracer_NilTracer_NoEvents pins the no-tracer path: leaving
// `transport.OpenOptions.Tracer` unset must complete a round-trip
// without panicking and must not emit anything. The
// allocation-free guarantee on the disabled path is structural — the
// helpers return nil so the option slice is never allocated — and is
// covered by `pktline/tracer_alloc_test.go` at that layer; this test
// confirms the SSH transport doesn't accidentally bypass the
// short-circuit.
func TestTracer_NilTracer_NoEvents(t *testing.T) {
	t.Parallel()

	bridge := bridgeSHA1Store(t, "loose-only")
	srv := newTestServer(t, testServerOpts{
		acceptEnv: true,
		serveStore: func() bridgeServer {
			return bridge
		},
	})

	tr := New(
		WithAuth(Signer(srv.clientSigner)),
		WithKnownHosts(srv.hostKeyCallback()),
	)
	conn, err := tr.Open(t.Context(), srv.URL(), transport.OpenOptions{
		UserAgent: "test/0.0",
		// Tracer left nil.
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	c, ok := conn.(*Conn)
	require.True(t, ok)
	drainAdvertisement(t, c)

	rdr, err := c.Command(t.Context(), "ls-refs",
		cmdBody("ls-refs", nil, []string{"object-format=sha1"}))
	require.NoError(t, err)
	_ = readAllPackets(t, rdr)

	require.NoError(t, c.Close())
}

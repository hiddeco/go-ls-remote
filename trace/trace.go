// Package trace defines the [Tracer] interface and event types emitted by
// the go-ls-remote library at significant points in the wire-protocol and
// HTTP lifecycle.
//
// The package is dependency-free: it imports nothing from sibling
// library packages because it is itself imported by the lowest-level
// codec ([github.com/hiddeco/go-ls-remote/pktline]) and by the transport
// layer. Letting trace depend on those would create import cycles. As a
// result, certain event field types are defined as local mirrors of
// types that live elsewhere — see [PacketKind], which mirrors
// `pktline.Kind`, and [NegotiateEvent.Version], which holds the raw
// protocol version as an int rather than `transport.ProtocolVersion`.
//
// Tracing is opt-in. A nil [Tracer] disables emission entirely and the
// library performs no work at instrumentation sites in that case.
package trace

import "time"

// Direction identifies the flow of a traced packet relative to the local
// process: a packet read from the network is [DirectionInbound], a
// packet written is [DirectionOutbound].
type Direction uint8

const (
	// DirectionInbound is set on events for data received from the remote.
	DirectionInbound Direction = iota + 1

	// DirectionOutbound is set on events for data sent to the remote.
	DirectionOutbound
)

// Tracer receives events emitted by the library at significant points
// in the wire-protocol and HTTP lifecycle.
//
// # Concurrency
//
// Implementations must not block; both methods are called synchronously
// on the I/O hot path, and slow tracers measurably degrade throughput.
//
// A Tracer may receive events from multiple sources in parallel —
// either commands multiplexed on a single HTTP-backed Session, or
// multiple Sessions sharing the Tracer across goroutines.
// Implementations must therefore be safe for concurrent OnEvent and
// OnPacketEvent calls. Wrapping non-concurrent sinks in a mutex is
// the usual pattern.
//
// # Nil tracer
//
// A nil Tracer disables tracing. Library code at every emission site is
// gated by an explicit `if tracer != nil` check, so passing nil incurs
// no allocation or per-call overhead beyond a single nil-pointer
// comparison.
type Tracer interface {
	// OnPacketEvent reports a single pkt-line read or write to the
	// tracer. This is the per-pkt-line hot path: every successful
	// `pktline.Reader.ReadPacket` and `pktline.Writer.WritePacket`
	// call invokes OnPacketEvent under a wired tracer. Implementations
	// MUST NOT block — pkt-line throughput on the wire is gated by how
	// fast OnPacketEvent returns.
	//
	// The pointed-to [PacketEvent] is owned by the emitter and reused
	// across calls. The pointer and every field — including Bytes —
	// are valid only for the duration of the OnPacketEvent call;
	// callers retaining state across the call MUST copy.
	OnPacketEvent(*PacketEvent)

	// OnEvent reports a single non-pkt-line event (HTTP, negotiation,
	// command lifecycle). The event value's contents are valid for the
	// duration of the call only when the event documentation says so
	// explicitly. Callers retaining such state across the call must copy.
	OnEvent(Event)
}

// Event is the common interface implemented by every value the library
// emits through [Tracer.OnEvent].
//
// `PacketEvent` is dispatched through [Tracer.OnPacketEvent] instead;
// it does not flow through [Tracer.OnEvent] from library-internal
// emitters. Third-party tracers that funnel events through OnEvent
// for logging may still call OnEvent themselves with `*PacketEvent`,
// but the high-frequency pktline path no longer pays the polymorphic
// dispatch cost.
//
// Event remains an unsealed interface: third parties may define their
// own event types and pass them through any [Tracer.OnEvent] caller.
// Consumers that type-switch on Event must include a `default` arm to
// remain forward-compatible.
type Event interface {
	// When returns the wall-clock time at which the event was generated.
	When() time.Time
}

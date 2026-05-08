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

// Tracer receives [Event] values emitted by the library at significant
// points in the wire-protocol and HTTP lifecycle.
//
// # Concurrency
//
// Implementations must not block; OnEvent is called synchronously on the
// I/O hot path, and slow tracers measurably degrade throughput.
//
// Implementations must also be safe for concurrent use if the library is
// used concurrently against the same Tracer (HTTP-backed sessions
// multiplex commands; see the public Session contract). Wrapping
// non-concurrent sinks in a mutex is the usual pattern.
//
// # Nil tracer
//
// A nil Tracer disables tracing. Library code at every emission site is
// gated by an explicit `if tracer != nil` check, so passing nil incurs
// no allocation or per-call overhead beyond a single nil-pointer
// comparison.
type Tracer interface {
	// OnEvent reports a single event to the tracer.
	//
	// The event value's contents are valid for the duration of the call
	// only when the event documentation says so explicitly (notably
	// [PacketEvent.Bytes], which aliases an internal buffer). Callers
	// retaining such state across the call must copy.
	OnEvent(Event)
}

// Event is the common interface implemented by every value the library
// emits through [Tracer.OnEvent].
//
// Event is intentionally not a sealed interface: third parties may
// define their own event types and pass them through any [Tracer].
// Consumers that type-switch on Event therefore must include a
// `default` arm to remain forward-compatible if either the library or a
// third party introduces additional event types.
//
// All built-in events are defined in this package: [PacketEvent],
// [HTTPEvent], [NegotiateEvent], and [CommandEvent].
type Event interface {
	// When returns the wall-clock time at which the event was generated.
	When() time.Time
}

package wire

import "time"

// CapabilityDropEvent is emitted when the encoder drops a per-command
// argument because the server did not advertise the capability that
// gates it. The v2 per-command grammar
// (`gitprotocol-v2.adoc` §"command-request") forbids sending args the
// server has not opted in to; rather than fail the request, the
// encoder downgrades silently and emits this event so a tracer can
// surface the divergence to a caller diagnosing missing data.
//
// CapabilityDropEvent is owned by the `wire` package rather than the
// `trace` package because it is specific to encoder downgrades and
// would not benefit other layers. Adding it to `trace` would extend
// that package's surface for a single emit site; defining it locally
// keeps the cross-package contract minimal — `trace` only needs to
// expose the [trace.Event] interface for third-party events to flow
// through any [trace.Tracer].
type CapabilityDropEvent struct {
	// Time is the wall-clock time the event was generated.
	Time time.Time

	// Command is the v2 command name the dropped argument belonged
	// to, e.g. `"ls-refs"`.
	Command string

	// Argument is the dropped argument's name, e.g. `"unborn"`.
	Argument string

	// Reason is a short human-readable explanation of the drop, e.g.
	// `"server did not advertise ls-refs=unborn"`.
	Reason string
}

// When implements [trace.Event].
func (e CapabilityDropEvent) When() time.Time { return e.Time }

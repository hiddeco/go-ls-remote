package filet

// Option configures a [Transport] at construction time. Construct an
// Option via the package's `With*` helpers; the type is intentionally
// sealed so the option set cannot grow outside this package.
type Option interface {
	apply(*Transport)
}

type optionFunc func(*Transport)

func (f optionFunc) apply(t *Transport) { f(t) }

// WithEndpointTrace additionally wires [transport.OpenOptions.Tracer]
// at the in-process server's pkt-line reader and writer. By default
// the tracer is wired only at the client-side reader and writer, so
// each pkt-line crossing the pipe pair produces a single
// [trace.PacketEvent] — matching the HTTP transport's
// one-event-per-pkt-line shape. With this option set, each pkt-line
// produces TWO events: one [trace.DirectionOutbound] from the writing
// side, one [trace.DirectionInbound] from the reading side.
//
// Use this when the in-process server's view of the traffic is
// load-bearing — round-trip causal-chain debugging or test fixtures
// that pin both endpoints' framing. Most callers should leave it
// off so a tracer wired through `transport.OpenOptions` produces the
// same volume of events regardless of which transport handled the
// dial.
func WithEndpointTrace() Option {
	return optionFunc(func(t *Transport) {
		t.endpointTrace = true
	})
}

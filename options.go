package lsremote

import (
	"github.com/hiddeco/go-ls-remote/trace"
	"github.com/hiddeco/go-ls-remote/transport"
)

// Option configures a [Dial]. Compose Options from the `With*`
// constructors exported by this package; the interface is intentionally
// sealed via the unexported `applyDial` method so the option set cannot
// grow outside this package.
type Option interface {
	applyDial(*dialConfig)
}

// dialConfig is the resolved option set a [Dial] consumes. Each `With*`
// constructor mutates one field. The zero value is meaningful: a nil
// registry asks [Dial] to fill in its default, a nil tracer disables
// tracing, an empty `userAgent` defers to the transport default, and a
// nil `protocol` leaves version negotiation automatic.
type dialConfig struct {
	// registry holds the [transport.Registry] the [Dial] will route URL
	// schemes through. A nil registry means "use the package default,"
	// which [Dial] resolves at call time.
	registry *transport.Registry

	// tracer receives protocol observability events. A nil tracer
	// disables tracing — emission sites short-circuit on the nil check.
	tracer trace.Tracer

	// userAgent overrides the User-Agent string on HTTP-based
	// transports. The empty string defers to the transport's own
	// default.
	userAgent string

	// protocol pins protocol-version negotiation. A nil pointer means
	// "auto-negotiate" — the integer zero of [ProtocolVersion] is
	// [ProtocolV0], so the pointer is the only way to encode the
	// absence of a preference distinctly from "pin to v0".
	protocol *ProtocolVersion
}

// optionFunc adapts a plain func into the sealed [Option] interface.
type optionFunc func(*dialConfig)

func (f optionFunc) applyDial(c *dialConfig) { f(c) }

// WithTransports replaces the default HTTP-only transport registry with
// r. Use it to opt into SSH, git, or file transports, or to substitute
// a custom HTTP client.
//
// Passing nil is permitted and means "fall back to the package default"
// — [Dial] performs the substitution at call time, so a nil here is
// indistinguishable from omitting the option.
func WithTransports(r *transport.Registry) Option {
	return optionFunc(func(c *dialConfig) {
		c.registry = r
	})
}

// WithTracer installs t as the [trace.Tracer] for protocol
// observability. A nil tracer disables tracing — the default — and
// every emission site in the library short-circuits on an explicit
// nil check.
func WithTracer(t trace.Tracer) Option {
	return optionFunc(func(c *dialConfig) {
		c.tracer = t
	})
}

// WithUserAgent overrides the User-Agent string sent on HTTP-based
// transports. The empty string means "use the transport's own
// default," which matches omitting the option entirely.
func WithUserAgent(s string) Option {
	return optionFunc(func(c *dialConfig) {
		c.userAgent = s
	})
}

// WithProtocol pins protocol negotiation to a specific wire version.
// Omit the option to auto-negotiate (prefer v2, fall back to v0).
//
// There is no `Auto` sentinel: absence of WithProtocol is the auto
// signal. The constructor captures v by value into a fresh pointer so
// later mutation of the caller's variable cannot affect the stored
// preference.
func WithProtocol(v ProtocolVersion) Option {
	return optionFunc(func(c *dialConfig) {
		pinned := v
		c.protocol = &pinned
	})
}

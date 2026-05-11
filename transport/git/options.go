package gitt

import "net"

// Option configures a [Transport] at construction time. Construct an
// Option via the package's `With*` helpers; the type is intentionally
// sealed so the option set cannot grow outside this package.
type Option interface {
	apply(*Transport)
}

type optionFunc func(*Transport)

func (f optionFunc) apply(t *Transport) { f(t) }

// WithDialer supplies the [net.Dialer] the [Transport] uses for the
// TCP dial underlying every git-daemon connection. Useful for setting
// per-connection timeouts, source addresses, KeepAlive intervals, and
// other dial-level knobs.
//
// Passing nil is permitted and means "use the 30-second default dialer";
// the resolution happens inside the unexported `dialer` helper so callers
// may pass nil through without an extra branch at construction.
func WithDialer(d *net.Dialer) Option {
	return optionFunc(func(t *Transport) {
		t.dialer = d
	})
}

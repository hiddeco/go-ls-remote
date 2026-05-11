// Package gitt implements the git-daemon transport for the `git://`
// URL scheme. The transport speaks the native git wire protocol over a
// plain TCP connection to a git-daemon process on port 9418
// (git-daemon(1), `connect.c:git_connect`). No authentication or
// encryption is performed; the protocol is unauthenticated by design.
// Construct a [Transport] with [New] and register it with a
// `transport.Registry` to use the `git` scheme.
package gitt

import (
	"context"
	"net"
	"time"
)

// defaultDialer is used whenever no [WithDialer] option is provided or
// a nil dialer is explicitly passed via [WithDialer].
var defaultDialer = &net.Dialer{Timeout: 30 * time.Second}

// Transport is the git-daemon transport. It is constructed via [New]
// and configured with `With*` [Option] helpers.
//
// A zero-value [Transport] is usable: it dials with the 30-second
// default dialer when no [WithDialer] option is supplied.
type Transport struct {
	// dialer is the [net.Dialer] used for the underlying TCP dial. A nil
	// value resolves to defaultDialer at dial time.
	dialer *net.Dialer

	// dialFn overrides the TCP dial when set. Nil means use the resolved
	// *net.Dialer. The field is package-private and intended only for
	// tests that need to inject a fake net.Conn (e.g. one whose Write
	// always fails). It is never exported and must not be set in
	// production code.
	dialFn func(ctx context.Context, network, addr string) (net.Conn, error)
}

// New returns a [Transport] configured with opts. The zero
// configuration is usable; options refine it. Nil entries in opts are
// skipped, so callers may pass conditionally constructed options
// without guarding each one.
func New(opts ...Option) *Transport {
	t := &Transport{}
	for _, opt := range opts {
		if opt == nil {
			continue
		}
		opt.apply(t)
	}
	return t
}

// Schemes implements [transport.Transport]. The git-daemon transport
// claims the single `git` scheme.
func (t *Transport) Schemes() []string {
	return []string{"git"}
}

// resolvedDialer returns the [net.Dialer] that will be used for the
// TCP dial. If no dialer was supplied (or a nil dialer was passed via
// [WithDialer]), defaultDialer is returned so the caller always
// receives a non-nil value.
func (t *Transport) resolvedDialer() *net.Dialer {
	if t.dialer != nil {
		return t.dialer
	}
	return defaultDialer
}

// resolvedDialFn returns the dial function that [Open] uses to
// establish a TCP connection. When `dialFn` is set (test-only
// injection), it is returned directly. Otherwise the function wraps
// the resolved `*net.Dialer` so all callers go through a single
// consistent path.
func (t *Transport) resolvedDialFn() func(ctx context.Context, network, addr string) (net.Conn, error) {
	if t.dialFn != nil {
		return t.dialFn
	}
	d := t.resolvedDialer()
	return d.DialContext
}

package ssht

import (
	"net"

	"golang.org/x/crypto/ssh"
)

// Option configures a [Transport] at construction time. Construct an
// Option via the package's `With*` helpers; the type is intentionally
// sealed so the option set cannot grow outside this package.
type Option interface {
	apply(*Transport)
}

type optionFunc func(*Transport)

func (f optionFunc) apply(t *Transport) { f(t) }

// WithAuth wires r into the [Transport]. The resolver is consulted
// once per dial; see [AuthResolver] for the full contract.
//
// Passing nil is permitted and means "anonymous SSH authentication" —
// the [Transport] will offer no auth methods and will only succeed
// against servers permitting anonymous access, which is rare in
// practice. Most callers wire one of the built-in resolvers ([Agent],
// [KeyFile], or [Signer]) or a custom implementation.
func WithAuth(r AuthResolver) Option {
	return optionFunc(func(t *Transport) {
		t.auth = r
	})
}

// WithClientConfig supplies a complete [ssh.ClientConfig] template the
// [Transport] uses as the base for every dial. The other configuration
// options ([WithAuth], [WithKnownHosts]) are merged onto a copy of this
// template at dial time using a caller-precedence rule: a non-nil
// `Auth` or `HostKeyCallback` field on the supplied config wins over
// the value carried by the corresponding `With*` option, and a nil
// field is filled in from the option. The merge produces a fresh
// [ssh.ClientConfig] per dial, so mutating the template after passing
// it to [WithClientConfig] is unsupported — the [Transport] reads the
// fields it needs at construction time without copying eagerly, and
// concurrent mutation observed during a dial has undefined behaviour.
//
// Passing nil is permitted and means "build the config from scratch
// using the other options."
func WithClientConfig(cfg *ssh.ClientConfig) Option {
	return optionFunc(func(t *Transport) {
		t.clientCfg = cfg
	})
}

// WithKnownHosts supplies the [ssh.HostKeyCallback] the [Transport]
// uses to verify the server's host key. Callers typically wire
// `golang.org/x/crypto/ssh/knownhosts.New(path)` so verification reads
// from an OpenSSH-format `known_hosts` file.
//
// Passing nil is permitted but the resulting behaviour depends on
// [WithClientConfig]:
//
//   - If [WithClientConfig] is not used or supplies a config whose
//     `HostKeyCallback` is nil, no host-key verification is configured
//     and every dial will fail — the underlying x/crypto/ssh client
//     refuses to dial without a callback. Calling
//     `WithKnownHosts(ssh.InsecureIgnoreHostKey())` is the explicit
//     opt-out for testing.
//   - If [WithClientConfig] supplies a config whose `HostKeyCallback`
//     is set, that callback wins (see the merge rule on
//     [WithClientConfig]).
//
// The callback is consulted by x/crypto/ssh during the SSH handshake
// for every dial.
func WithKnownHosts(cb ssh.HostKeyCallback) Option {
	return optionFunc(func(t *Transport) {
		t.hostKey = cb
	})
}

// WithDialer supplies the [*net.Dialer] the [Transport] uses for the
// TCP dial underlying every SSH connection. Useful for setting
// per-connection timeouts, source addresses, KeepAlive intervals, and
// other dial-level knobs.
//
// Passing nil is permitted and means "use a zero-value [net.Dialer]"
// at dial time; the resolution happens inside [Transport.Open] so
// callers may pass nil through without an extra branch at
// construction.
func WithDialer(d *net.Dialer) Option {
	return optionFunc(func(t *Transport) {
		t.dialer = d
	})
}

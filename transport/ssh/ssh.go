package ssht

import (
	"context"
	"errors"
	"net"

	"golang.org/x/crypto/ssh"

	"github.com/hiddeco/go-ls-remote/transport"
)

// Transport is the SSH Git [transport.Transport]. It is constructed via
// [New] and configured with `With*` [Option] helpers.
//
// A zero-value [Transport] is usable but rarely useful: without an
// [AuthResolver] every dial will fail at SSH authentication, and
// without a host-key callback (either from [WithKnownHosts] or supplied
// via [WithClientConfig]) every dial will be rejected at host-key
// verification. Typical use therefore wires at least one of [WithAuth]
// and [WithKnownHosts].
//
// Field ordering clusters interface- and pointer-shaped fields ahead of
// any value fields so the struct packs without padding on 64-bit
// platforms; today all four fields are pointer-shaped.
type Transport struct {
	// auth resolves SSH authentication methods per dial. A nil value
	// means "no authentication" — see [WithAuth].
	auth AuthResolver

	// clientCfg is a caller-supplied [ssh.ClientConfig] template. The
	// other options (auth, host-key callback) are merged onto a copy
	// of this template at dial time; see [WithClientConfig] for the
	// merge rule. A nil value means the [Transport] builds the config
	// from scratch.
	clientCfg *ssh.ClientConfig

	// hostKey verifies the server's host key. A nil value means no
	// host-key callback is configured by this option; see
	// [WithKnownHosts] for the interaction with [WithClientConfig].
	hostKey ssh.HostKeyCallback

	// dialer is the [net.Dialer] used for the underlying TCP dial. A
	// nil value resolves to a zero-value dialer at dial time.
	dialer *net.Dialer
}

// errNotImplemented is returned by [Transport.Open] until the dial,
// exec, and version-negotiation work lands.
var errNotImplemented = errors.New("ssht: not implemented yet")

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

// Schemes implements [transport.Transport]. The SSH transport claims
// the single `ssh` scheme; URLs in `git@host:path` scp-like shorthand
// are normalised to `ssh://` by the URL parser before the [Registry]
// lookup runs.
func (t *Transport) Schemes() []string {
	return []string{"ssh"}
}

// Open implements [transport.Transport]. It is intentionally stubbed
// in this commit: the dial, command exec, and version-negotiation
// machinery lands in the next commit. Until then every call returns a
// fixed sentinel error so that misconfigured wiring fails loudly rather
// than panicking on a half-built [Conn].
func (t *Transport) Open(ctx context.Context, u *transport.URL, opts transport.OpenOptions) (transport.Conn, error) {
	return nil, errNotImplemented
}

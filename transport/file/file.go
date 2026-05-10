// Package filet implements the local-filesystem Git [transport.Transport].
//
// The transport opens a repository on the local filesystem and serves
// it through an in-process upload-pack loop, exposing the same
// pkt-line-shaped advertisement and command stream the network
// transports return. Callers reach it through the `file://` URL
// scheme and the standard [transport.Registry] composition.
//
// This file holds the [Transport] type, its [New] constructor, and the
// [Transport.Schemes] entry point. The dial lifecycle and [transport.Conn]
// implementation live in sibling files.
package filet

import (
	"context"
	"errors"

	"github.com/hiddeco/go-ls-remote/transport"
)

// Transport is the local-filesystem Git [transport.Transport]. It is
// constructed via [New] and configured with `With*` [Option] helpers.
//
// A zero-value [Transport] is usable: the dial opens the repository
// referenced by the `file://` URL and serves it through an in-process
// upload-pack loop with no extra configuration.
//
// The struct deliberately has no exported knobs — the local-file
// transport accepts a `file://` URL and nothing else, so there is no
// path-injection surface to expose. Future per-Transport tuning (for
// example a hook that swaps in a test object-store backend) will be
// added here.
type Transport struct{}

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

// Schemes implements [transport.Transport]. The local-filesystem
// transport claims the single `file` scheme; lookups in
// [transport.Registry] are case-insensitive.
func (t *Transport) Schemes() []string {
	return []string{"file"}
}

// Open implements [transport.Transport]. The dial lifecycle — opening
// the on-disk repository through `internal/objstore` and wiring it to
// the in-process server over an `io.Pipe` pair — lands in a
// follow-up commit. Until then Open returns a not-implemented
// sentinel so a misconfigured caller fails loudly rather than dialling
// a half-built transport.
func (t *Transport) Open(ctx context.Context, u *transport.URL, opts transport.OpenOptions) (transport.Conn, error) {
	return nil, errors.New("transport/file: not implemented")
}

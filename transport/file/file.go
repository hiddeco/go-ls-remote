// Package filet implements the local-filesystem Git [transport.Transport].
//
// The transport opens a repository on the local filesystem and serves
// it through an in-process upload-pack loop, exposing the same
// pkt-line-shaped advertisement and command stream the network
// transports return. Callers reach it through the `file://` URL
// scheme and the standard [transport.Registry] composition.
//
// # Lifecycle
//
// [Transport.Open] resolves the URL's path to an on-disk repository,
// opens it through `internal/objstore`, and spawns a goroutine running
// `internal/server.Serve` against an in-memory `io.Pipe` pair. The
// returned [Conn] owns both the goroutine and the underlying store;
// [Conn.Close] cancels the goroutine's context, closes the pipes, and
// releases the store in a single idempotent step.
//
// # Concurrency
//
// A [Conn] returned by [Transport.Open] is a single-session handle and
// is not safe for concurrent use. The local-filesystem transport
// attaches a single in-process server goroutine to one pkt-line pipe
// pair for the [Conn]'s lifetime, so every advertisement read and
// every [Conn.Command] call shares one stream and one server end.
// Concurrent [Conn.Command] invocations would interleave request
// frames into the same pipe and race against the server's response
// emission; the contract per [transport.Conn] is that callers
// serialise these calls themselves. Multiple independent [Conn]s
// against the same or different repositories can run in parallel —
// each owns a disjoint goroutine and pipe pair.
package filet

import (
	"context"

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

// Open implements [transport.Transport]. It resolves u to an on-disk
// repository, opens it through `internal/objstore`, and spawns a
// goroutine running `internal/server.Serve` against an in-memory
// `io.Pipe` pair so the returned [Conn]'s advertisement reader is
// already streaming bytes when Open returns.
//
// Failure modes split between synchronous and goroutine-mediated:
// URL decode errors, missing repositories, unsupported repository
// formats, and pinned protocol versions outside `{v0, v2}` surface as
// `*ProtocolError` before any goroutine spawns; per-command failures
// from the server goroutine surface on the next read or write of the
// returned [Conn]'s reader or writer.
func (t *Transport) Open(ctx context.Context, u *transport.URL, opts transport.OpenOptions) (transport.Conn, error) {
	return t.open(ctx, u, opts)
}

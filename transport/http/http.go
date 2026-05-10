package httpt

import (
	"context"
	"crypto/tls"
	"net/http"

	"github.com/hiddeco/go-ls-remote/transport"
)

// defaultUserAgent is the User-Agent string the HTTP transport sends
// when no per-Transport or per-call override applies. It carries the
// project name and a major-version digit so server-side analytics can
// distinguish this client from canonical Git without misrepresenting
// it as `git/...`.
const defaultUserAgent = "lsremote/0"

// defaultMaxRedirects bounds the number of HTTP redirects the probe
// follows when [WithMaxRedirects] is not configured. Ten matches both
// `net/http`'s built-in cap and canonical Git's default behaviour;
// keeping the same number means a server that drives clients to the
// limit gets the same outcome on either implementation.
const defaultMaxRedirects = 10

// Transport is the HTTP/HTTPS Git [transport.Transport]. It is
// constructed via [New] and configured with `With*` [Option] helpers.
//
// A zero-value [Transport] is usable: an [http.DefaultClient] is used
// for the underlying I/O, no credentials are sent, and redirects
// follow the canonical-Git default (`initial`).
//
// Field ordering clusters interface- and pointer-shaped fields ahead
// of value fields so the struct packs without padding on 64-bit
// platforms.
type Transport struct {
	// client is the underlying [http.Client]. A nil value is
	// resolved to [http.DefaultClient] at [Transport.Open] time so
	// callers can pass nil through [WithClient] without surprise.
	client *http.Client

	// creds resolves a [Credentials] per dial. nil means anonymous
	// requests.
	creds CredentialResolver

	// tlsConfig overrides TLS for HTTPS dials. nil means use the
	// stdlib defaults via the configured client.
	tlsConfig *tls.Config

	// userAgent is the per-Transport agent string. The empty string
	// defers to [transport.OpenOptions.UserAgent] and ultimately to
	// the package default.
	userAgent string

	// maxRedirects bounds the redirect chain. Zero defers to the
	// package default of 10; negative values normalise to zero. Both
	// of those resolutions happen at [Transport.Open] time.
	maxRedirects int

	// followRedirects selects when redirects are followed. The zero
	// value is [FollowRedirectsInitial], matching canonical Git.
	followRedirects FollowRedirects
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

// Schemes implements [transport.Transport]. The HTTP transport claims
// `https` first then `http`; lookups in [transport.Registry] are
// case-insensitive so this list defines presentation order only.
func (t *Transport) Schemes() []string {
	return []string{"https", "http"}
}

// Open implements [transport.Transport]. It performs the HTTP
// discovery probe against u — `GET <u>/info/refs?service=git-upload-pack`
// — and, on success, returns a [Conn]. On the smart branch the
// advertisement reader is positioned past the
// `# service=git-upload-pack` preamble. On the dumb branch the
// reader is the synthetic v0-shaped pkt-line stream produced by
// `internal/dumbhttp`, and [Conn.Command] short-circuits to
// [ErrUnsupportedProtocol] since dumb HTTP has no v2 endpoint.
// Failure modes surface either a sentinel from this package
// ([ErrAuthRequired], [ErrAuthFailed], [ErrNotFound],
// [ErrUnsupportedProtocol]) or a [*ProtocolError]; see the helpers
// in `open.go` for the dispatch table.
func (t *Transport) Open(ctx context.Context, u *transport.URL, opts transport.OpenOptions) (transport.Conn, error) {
	return t.open(ctx, u, opts)
}

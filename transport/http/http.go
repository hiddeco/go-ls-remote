package httpt

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"

	"github.com/hiddeco/go-ls-remote/transport"
)

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

// Open implements [transport.Transport]. The full smart-probe with
// dumb-HTTP fallback lands in a follow-up change; the current body
// returns a placeholder error so a misuse surfaces a clear message
// rather than a nil dereference.
func (t *Transport) Open(ctx context.Context, u *transport.URL, opts transport.OpenOptions) (transport.Conn, error) {
	return nil, errors.New("transport/http: Open not implemented")
}

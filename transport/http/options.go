package httpt

import (
	"fmt"
	"net/http"
)

// Option configures a [Transport] at construction time. Construct an
// Option via the package's `With*` helpers; the type is intentionally
// sealed so the option set cannot grow outside this package.
type Option interface {
	apply(*Transport)
}

type optionFunc func(*Transport)

func (f optionFunc) apply(t *Transport) { f(t) }

// WithClient supplies the underlying [http.Client] the [Transport]
// uses for every request.
//
// Passing nil is permitted and means "use [http.DefaultClient]"; the
// resolution happens at [Transport.Open] time so callers may override
// freely without an extra branch at construction.
func WithClient(c *http.Client) Option {
	return optionFunc(func(t *Transport) {
		t.client = c
	})
}

// WithCredentials wires r into the [Transport]. The resolver is
// consulted once per dial; see [CredentialResolver] for the full
// contract.
//
// Passing nil is permitted and means "no credentials" — the
// [Transport] sends anonymous requests and surfaces the server's
// authentication challenge to the caller.
func WithCredentials(r CredentialResolver) Option {
	return optionFunc(func(t *Transport) {
		t.creds = r
	})
}

// WithUserAgent records the per-Transport User-Agent string. It is
// consulted only when [transport.OpenOptions.UserAgent] passed to
// [Transport.Open] is the empty string; a non-empty
// `OpenOptions.UserAgent` always wins. When neither is set the
// package default (`wire.DefaultUserAgent`) applies. The resolved value
// is reused on every command POST issued through the resulting
// [Conn].
func WithUserAgent(ua string) Option {
	return optionFunc(func(t *Transport) {
		t.userAgent = ua
	})
}

// WithFollowRedirects selects the redirect policy. The zero value of
// [FollowRedirects] is [FollowRedirectsInitial], which matches
// canonical Git's `http.followRedirects=initial` default
// ([Documentation/config/http.adoc:359-365]).
//
// [Documentation/config/http.adoc:359-365]: https://github.com/git/git/blob/v2.54.0/Documentation/config/http.adoc?plain=1#L359-L365
func WithFollowRedirects(p FollowRedirects) Option {
	return optionFunc(func(t *Transport) {
		t.followRedirects = p
	})
}

// WithMaxRedirects bounds how many consecutive redirects the
// [Transport] will follow before reporting an error.
//
// The value is recorded verbatim. Resolution rules — `0` falls back to
// the package default of 10, negative values normalise to `0` (no
// redirects) — are applied at dial time.
func WithMaxRedirects(n int) Option {
	return optionFunc(func(t *Transport) {
		t.maxRedirects = n
	})
}

// FollowRedirects selects when the [Transport] follows HTTP redirects.
// The variants match canonical Git's `http.followRedirects` config
// values ([Documentation/config/http.adoc:359-365]).
//
// The zero value is [FollowRedirectsInitial], so a freshly constructed
// [Transport] follows redirects on the initial discovery GET but
// rejects redirects on subsequent POSTs — the canonical default.
//
// [Documentation/config/http.adoc:359-365]: https://github.com/git/git/blob/v2.54.0/Documentation/config/http.adoc?plain=1#L359-L365
type FollowRedirects uint8

const (
	// FollowRedirectsInitial follows redirects on the initial GET to
	// `info/refs` but rejects them on later POSTs. This is the zero
	// value of [FollowRedirects] and the canonical Git default.
	FollowRedirectsInitial FollowRedirects = iota

	// FollowRedirectsAlways follows redirects on every request,
	// including POSTs. Equivalent to canonical Git's `always`.
	FollowRedirectsAlways

	// FollowRedirectsNever rejects every redirect. Equivalent to
	// canonical Git's `false`.
	FollowRedirectsNever
)

// String returns the canonical Git config token for the variant —
// `initial`, `always`, `never` — or `unknown(N)` for any out-of-range
// value.
func (p FollowRedirects) String() string {
	switch p {
	case FollowRedirectsInitial:
		return "initial"
	case FollowRedirectsAlways:
		return "always"
	case FollowRedirectsNever:
		return "never"
	default:
		return fmt.Sprintf("unknown(%d)", uint8(p))
	}
}

package httpt

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// errRedirectRejected is the sentinel returned from
// [probeRedirector.check] when the redirect policy refuses to follow a
// 3xx. It travels through `client.Do` wrapped in `*url.Error`; the
// probe call site unwraps to a [ProtocolError] whose `Err` wraps this
// sentinel via `fmt.Errorf("...: %w", ...)` so an `errors.Is` test
// matches regardless of the human-readable phrasing
// ([classifyRedirectError]). Lowercase on purpose: external callers
// should not pin behaviour on it.
var errRedirectRejected = errors.New("transport/http: redirect rejected by policy")

// errRedirectTooMany is the sentinel wrapped by the [ProtocolError]
// when a redirect chain exceeds the configured cap. Same wrapping
// rules as [errRedirectRejected].
var errRedirectTooMany = errors.New("transport/http: too many redirects")

// probeRedirector carries the per-call state the [http.Client]'s
// `CheckRedirect` hook needs to enforce the redirect policy
// (`Documentation/config/http.adoc:359-365`) and the cross-origin
// auth-strip rule modelled on `http.c::update_url_from_redirect`
// (around `http.c:2268`).
//
// One instance is constructed per [Transport.open] invocation. It
// retains a reference to the resolver for the lifetime of the
// redirect chain, then is discarded; nothing outlives the call so
// there are no GC concerns.
//
// The redirector deliberately does NOT carry a [context.Context]
// field: storing a context on a struct violates the Go convention
// (`pkg.go.dev/context` documents context as a function argument,
// not a struct field) and would forward-trap a reused
// `*http.Client` to the probe's context. `check` reads
// `req.Context()` instead, which `net/http` propagates unchanged
// through redirect follow-up requests, so the resolver is consulted
// with the per-request context regardless of which call drove the
// chain.
type probeRedirector struct {
	policy FollowRedirects
	max    int
	creds  CredentialResolver

	// resolveErr captures the first credential-resolver error so the
	// probe call site can surface it. `CheckRedirect` cannot itself
	// return a wrapped resolver error and have it round-trip through
	// `client.Do` unmolested, so we stash it and treat the redirect as
	// rejected; the caller threads it back into a [*ProtocolError].
	resolveErr error
}

// check is the [http.Client.CheckRedirect] callback. It is invoked by
// `net/http` immediately before each follow-up request — after stdlib
// has populated the new request's headers from the initial request,
// but before the request is dispatched. That timing is what lets us
// strip and re-supply `Authorization` based on Git's own cross-origin
// rule (host change OR scheme downgrade) rather than stdlib's
// domain-prefix heuristic, which leaks credentials across ports and
// across an `https`→`http` downgrade.
//
// The resolver is consulted only when there was an `Authorization`
// header to strip — i.e. when the chain is propagating credentials
// across origins. A redirect on an anonymous request goes through
// untouched; if the destination demands auth it returns 401 and the
// caller's standard 401-retry path handles credential lookup at the
// post-redirect URL. That layering keeps the anonymous-first probe
// contract intact (`remote-curl.c::http_request_reauth` matches it on
// the curl side).
func (r *probeRedirector) check(req *http.Request, via []*http.Request) error {
	if r.policy == FollowRedirectsNever {
		return errRedirectRejected
	}
	// A `0` cap is treated as "no redirects allowed"; the rejection
	// uses [errRedirectRejected] so the surfaced message reads as
	// "redirects disabled" rather than "exceeded 0 hops". This covers
	// both `WithMaxRedirects(0)` and `WithMaxRedirects(-1)`, which
	// [resolveMaxRedirects] clamps to zero.
	if r.max == 0 {
		return errRedirectRejected
	}
	if len(via) > r.max {
		return errRedirectTooMany
	}

	prev := via[len(via)-1].URL
	if !isCrossOrigin(prev, req.URL) {
		return nil
	}
	if req.Header.Get("Authorization") == "" {
		return nil
	}
	req.Header.Del("Authorization")
	if r.creds == nil {
		return nil
	}
	// `req.Context()` is the per-request context `net/http`
	// propagates through redirect follow-ups; reading it here keeps
	// the resolver consult tied to the call that actually drove the
	// chain.
	creds, err := r.creds.Resolve(req.Context(), req.URL)
	if err != nil {
		r.resolveErr = err
		return errRedirectRejected
	}
	if creds == nil {
		return nil
	}
	if err := creds.Apply(req); err != nil {
		r.resolveErr = err
		return errRedirectRejected
	}
	return nil
}

// isCrossOrigin reports whether navigating from prev to next crosses an
// origin boundary as Git defines it: a different host (case-insensitive)
// or an `https`→`http` scheme downgrade. A scheme upgrade
// (`http`→`https`) to the same host is NOT cross-origin, matching
// canonical Git's behaviour around `http.c::update_url_from_redirect`
// (a redirect-to-https on the same host preserves credentials).
//
// The host comparison includes any explicit port: `example.com:8001`
// and `example.com:8002` are different origins. `net/http` itself only
// compares hostnames here, so a port change between same-named hosts
// would otherwise leak `Authorization`.
func isCrossOrigin(prev, next *url.URL) bool {
	if !strings.EqualFold(prev.Host, next.Host) {
		return true
	}
	if prev.Scheme == "https" && next.Scheme == "http" {
		return true
	}
	return false
}

// httpClientForProbe returns the [http.Client] the probe uses for one
// call. It wraps the caller-supplied client (if any) without mutating
// it: a fresh client value is constructed that inherits the underlying
// `Transport`, `Jar`, and `Timeout`, but installs our own
// `CheckRedirect` so the policy applies even when a caller has
// configured their own.
//
// The returned client carries no per-request context: the redirector
// reads `req.Context()` inside `CheckRedirect`, so the same client
// can be reused safely across requests driven by different contexts
// (the command path reuses the probe's client to inherit cookies,
// transport-level config, and any test hooks).
//
// The caller's `Jar` is preserved through the shallow copy. That is
// the desired cookie-continuity semantic across a redirect chain:
// any `Set-Cookie` returned by an intermediate hop is stored on the
// shared jar and replayed on the next hop, matching what a user
// running `git` against the same URL would see.
//
// Mutating the caller's client in place was rejected because callers
// commonly share an [http.Client] across packages; setting fields like
// `CheckRedirect` would surprise anyone holding the same pointer.
func httpClientForProbe(base *http.Client, redir *probeRedirector) *http.Client {
	if base == nil {
		base = http.DefaultClient
	}
	cp := *base
	cp.CheckRedirect = redir.check
	return &cp
}

// resolveMaxRedirects normalises the per-Transport [Transport.maxRedirects]
// field. A zero value (the unset case) maps to the package default;
// negative values clamp to zero, equivalent to "do not follow". The
// stored value is preserved as-is on the [Transport] so an inspecting
// caller can tell which case applied.
func resolveMaxRedirects(n int) int {
	switch {
	case n == 0:
		return defaultMaxRedirects
	case n < 0:
		return 0
	default:
		return n
	}
}

// classifyRedirectError unwraps an error returned from `client.Do` into
// a [ProtocolError] when the cause is one of the redirect sentinels.
// Stdlib wraps the policy error in [url.Error]; we look through that
// to attach the response status (if any) to the surfaced error so a
// caller can distinguish "redirect rejected on a 302" from "too many
// hops". When the cause is something else (a network error, say) the
// returned [ProtocolError] still carries the wrapped err so the call
// site can round-trip it via [errors.Is].
func classifyRedirectError(err error, resp *http.Response, redacted string, redir *probeRedirector) (*ProtocolError, bool) {
	switch {
	case errors.Is(err, errRedirectRejected):
		// A resolver error wins over the synthetic policy sentinel: it
		// is the real cause and callers may want to match it.
		cause := redir.resolveErr
		if cause == nil {
			// Wrap the sentinel so callers can match it with
			// `errors.Is`. A zero hop cap is "redirects disabled"
			// rather than a policy rejection: phrase it that way so
			// `WithMaxRedirects(0)` produces a message matching the
			// configuration that triggered the rejection.
			if redir.max == 0 && redir.policy != FollowRedirectsNever {
				cause = fmt.Errorf("redirects disabled (max-redirects=0): %w", errRedirectRejected)
			} else {
				cause = fmt.Errorf("redirect rejected by %s policy: %w", redir.policy, errRedirectRejected)
			}
		}
		pe := &ProtocolError{URL: redacted, Op: "probe", Err: cause}
		if resp != nil {
			pe.Status = resp.StatusCode
		}
		return pe, true
	case errors.Is(err, errRedirectTooMany):
		return &ProtocolError{
			URL: redacted,
			Op:  "probe",
			Err: fmt.Errorf("redirect chain exceeded %d hops: %w", redir.max, errRedirectTooMany),
		}, true
	}
	return nil, false
}

package httpt

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hiddeco/go-ls-remote/internal/dumbhttp"
	"github.com/hiddeco/go-ls-remote/internal/wire"
	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/trace"
	"github.com/hiddeco/go-ls-remote/transport"
)

// open performs the HTTP discovery probe against u and, on success,
// returns a [Conn]. On the smart branch the Conn's [pktline.Reader]
// is positioned past the `# service=git-upload-pack` preamble and its
// trailing flush packet; on the dumb branch the Conn wraps a
// [dumbhttp.NewAdapter] over the body. Failure modes — bad status,
// malformed preamble, auth challenges — surface either a sentinel
// from this package or a [ProtocolError] depending on the layer that
// detected them.
//
// The probe shape mirrors canonical Git's
// `remote-curl.c::discover_refs` (`remote-curl.c:465-577`): GET
// `<base>/info/refs?service=git-upload-pack` with the
// `Git-Protocol` header carrying the negotiated wire version, then
// dispatch on the response's status and content type. Smart vs
// dumb selection follows `gitprotocol-http.adoc:230-281`: a
// `200 OK` with `Content-Type: application/x-git-upload-pack-advertisement`
// is the smart path; any other content type on a `200 OK` is the
// dumb path.
//
// Redirects honour [Transport.followRedirects]
// (`Documentation/config/http.adoc:359-365`); the per-call
// [probeRedirector] enforces both the policy and the cross-origin
// auth-strip rule modelled on `http.c::update_url_from_redirect`.
// `FollowRedirectsInitial` and `FollowRedirectsAlways` both follow
// redirects on this GET probe; they diverge on the command POST,
// where `Initial` rejects 3xx and `Always` follows.
func (t *Transport) open(ctx context.Context, u *transport.URL, opts transport.OpenOptions) (transport.Conn, error) {
	redir := &probeRedirector{
		policy: t.followRedirects,
		max:    resolveMaxRedirects(t.maxRedirects),
		creds:  t.creds,
	}
	client := httpClientForProbe(t.client, redir)

	probeURL := buildInfoRefsURL(u)
	redacted := transport.RedactURL(probeURL)
	ua := resolveUserAgent(t.userAgent, opts.UserAgent)
	gitProto := wire.HTTPProtocolHeader(opts.PreferredProtocol)
	connCfg := connConfig{
		client:            client,
		creds:             t.creds,
		redir:             redir,
		userAgent:         ua,
		gitProtocolHeader: gitProto,
		tracer:            opts.Tracer,
	}

	resp, err := doProbe(ctx, client, probeURL, ua, gitProto, nil, opts.Tracer)
	if err != nil {
		// `client.Do` may return both a non-nil response and a non-nil
		// error (e.g. a `CheckRedirect` rejection). Modern `net/http`
		// closes the body itself, but the documented contract leaves
		// the body for the caller; close defensively for symmetry with
		// the command path and to insulate against a future stdlib
		// change.
		if resp != nil && resp.Body != nil {
			drainAndClose(resp.Body)
		}
		if pe, ok := classifyRedirectError(err, resp, redacted, "probe", redir); ok {
			return nil, pe
		}
		return nil, &ProtocolError{URL: redacted, Op: "probe", Err: err}
	}

	if resp.StatusCode == http.StatusUnauthorized {
		// One auth retry. A challenge-aware resolver would benefit
		// from the response's `WWW-Authenticate` value, but the
		// current [CredentialResolver] interface does not accept a
		// challenge argument; widening it lands in a follow-up
		// change. Until then the retry matches
		// `remote-curl.c::http_request_reauth` at a coarser grain.
		//
		// The retry uses the URL the 401 came from — i.e. the
		// post-redirect URL when the chain redirected before the
		// challenge — rather than the original probe URL. That
		// matches what the resolver expects (the resolver was
		// consulted with the URL the 401 was received from) and is
		// what canonical Git's `http_request_reauth` does as a
		// matter of course.
		retryURL := probeURL
		if resp.Request != nil && resp.Request.URL != nil {
			retryURL = resp.Request.URL.String()
		}
		drainAndClose(resp.Body)

		if t.creds == nil {
			return nil, ErrAuthRequired
		}
		retryParsed, perr := url.Parse(retryURL)
		if perr != nil {
			return nil, fmt.Errorf("transport/http: parse retry url %s: %w", redacted, perr)
		}
		creds, rerr := t.creds.Resolve(ctx, retryParsed)
		if rerr != nil {
			return nil, fmt.Errorf("transport/http: resolve credentials for %s: %w", redacted, rerr)
		}
		if creds == nil {
			return nil, ErrAuthRequired
		}

		// The redirector is reused across the two probe attempts so the
		// caller's [http.Client] keeps its single `CheckRedirect` hook.
		// In the current control flow `resolveErr` is unreachable at this
		// point — the only path that sets it returns a redirect error
		// from the first `doProbe` and short-circuits the function — but
		// resetting it here keeps the invariant explicit so a future
		// refactor cannot leak first-attempt state into the retry's
		// classification.
		redir.resolveErr = nil

		resp, err = doProbe(ctx, client, retryURL, ua, gitProto, creds, opts.Tracer)
		if err != nil {
			if resp != nil && resp.Body != nil {
				drainAndClose(resp.Body)
			}
			if pe, ok := classifyRedirectError(err, resp, redacted, "probe", redir); ok {
				return nil, pe
			}
			return nil, &ProtocolError{URL: redacted, Op: "probe", Err: err}
		}
		if resp.StatusCode == http.StatusUnauthorized {
			drainAndClose(resp.Body)
			return nil, ErrAuthFailed
		}
	}

	switch resp.StatusCode {
	case http.StatusOK:
		return handleOK(resp, redacted, connCfg)
	case http.StatusForbidden:
		drainAndClose(resp.Body)
		return nil, ErrAuthFailed
	case http.StatusNotFound:
		drainAndClose(resp.Body)
		return nil, ErrNotFound
	}

	if resp.StatusCode >= 500 && resp.StatusCode <= 599 {
		server := readServerExcerpt(resp.Body)
		_ = resp.Body.Close()
		return nil, &ProtocolError{
			URL:    redacted,
			Op:     "probe",
			Status: resp.StatusCode,
			Server: server,
		}
	}

	server := readServerExcerpt(resp.Body)
	_ = resp.Body.Close()
	return nil, &ProtocolError{
		URL:    redacted,
		Op:     "probe",
		Status: resp.StatusCode,
		Server: server,
		Err:    fmt.Errorf("unexpected status %d", resp.StatusCode),
	}
}

// connConfig bundles the per-call data [Transport.open] needs to hand
// to a freshly minted [Conn] so the command path can reuse the same
// client, credential resolver, redirect-policy redirector, User-Agent,
// and `Git-Protocol` header without walking back to a `*Transport`
// reference.
type connConfig struct {
	client            *http.Client
	creds             CredentialResolver
	redir             *probeRedirector
	tracer            trace.Tracer
	userAgent         string
	gitProtocolHeader string
}

// handleOK splits the smart and dumb branches off a `200 OK` response.
// The smart branch validates the `# service=git-upload-pack` preamble
// per `gitprotocol-http.adoc:274-286` and hands the post-preamble
// reader to a [Conn]. The dumb branch wraps the body in
// [dumbhttp.NewAdapter], producing a synthetic v0-shaped pkt-line
// stream the wire layer can consume uniformly with the smart shape.
//
// The [Conn] records the URL the response actually came from, which
// is the post-redirect URL when the probe followed a chain. The
// command path reuses that URL as its base, so preserving it here is
// what makes `http.c::update_url_from_redirect`-style chasing work
// end-to-end.
func handleOK(resp *http.Response, redacted string, cfg connConfig) (transport.Conn, error) {
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		mediaType = strings.TrimSpace(resp.Header.Get("Content-Type"))
	}
	if !strings.EqualFold(mediaType, smartAdvContentType) {
		return handleDumb(resp, cfg), nil
	}
	return handleSmart(resp, redacted, cfg)
}

// handleSmart finalises a smart-HTTP probe response: it validates the
// `# service=git-upload-pack` preamble per
// `gitprotocol-http.adoc:274-286`, then constructs a [Conn] whose
// reader is positioned past the preamble. The dumb-HTTP path does NOT
// invoke the preamble strip — `internal/dumbhttp` synthesises a v0
// advertisement directly without a service preamble — so the strip
// runs only on this branch.
//
// The reader is constructed with the per-request URL pinned for
// [trace.PacketEvent] emission. HTTP redirects mean the URL the
// response actually came from may differ from the original probe
// URL, so the URL is taken from `resp.Request` (the final hop)
// rather than the caller-known original.
func handleSmart(resp *http.Response, redacted string, cfg connConfig) (transport.Conn, error) {
	rdr := pktline.NewReader(resp.Body, inboundReaderOpts(cfg.tracer, finalRespURL(resp))...)
	if err := stripSmartPreamble(rdr); err != nil {
		drainAndClose(resp.Body)
		return nil, &ProtocolError{
			URL:    redacted,
			Op:     "probe",
			Status: http.StatusOK,
			Err:    err,
		}
	}
	return newConn(resp, rdr, cfg, false), nil
}

// handleDumb finalises a dumb-HTTP probe response by handing the body
// to [dumbhttp.NewAdapter]. The Conn is flagged dumb so [Conn.Command]
// short-circuits to [ErrUnsupportedProtocol] without dialing.
//
// The adapter's synthesised pkt-line stream carries the tracer so
// [trace.PacketEvent] values reflect the v0-shaped output the wire
// layer would have observed against a real smart-v0 server.
func handleDumb(resp *http.Response, cfg connConfig) transport.Conn {
	rdr := dumbhttp.NewAdapter(resp.Body, inboundReaderOpts(cfg.tracer, finalRespURL(resp))...)
	return newConn(resp, rdr, cfg, true)
}

// newConn assembles a [Conn] from a probe response. It captures the
// post-redirect URL in `*url.URL` form so [Conn.Command] can rewrite
// the path to derive the per-command POST URL.
func newConn(resp *http.Response, rdr *pktline.Reader, cfg connConfig, dumb bool) *Conn {
	var finalURL *url.URL
	if resp.Request != nil && resp.Request.URL != nil {
		// Defensive copy: the [http.Request]'s URL pointer may be
		// referenced by stdlib internals, and command POSTs mutate a
		// copy of this value when rewriting the path.
		u := *resp.Request.URL
		finalURL = &u
	}
	return &Conn{
		body:              resp.Body,
		reader:            rdr,
		client:            cfg.client,
		url:               finalURL,
		creds:             cfg.creds,
		userAgent:         cfg.userAgent,
		gitProtocolHeader: cfg.gitProtocolHeader,
		redir:             cfg.redir,
		tracer:            cfg.tracer,
		dumb:              dumb,
	}
}

// finalRespURL returns the URL of the final hop of the redirect
// chain that produced resp, redacted via [transport.RedactURL]. It
// is used to pin a [trace.PacketEvent] URL on a freshly constructed
// [pktline.Reader] for an HTTP response body. When `resp.Request`
// is unset (a defensive case modern `net/http` does not produce on
// the success path), the empty string is returned: an empty URL on
// a [trace.PacketEvent] is preferable to a panic.
func finalRespURL(resp *http.Response) string {
	if resp == nil || resp.Request == nil || resp.Request.URL == nil {
		return ""
	}
	return transport.RedactURL(resp.Request.URL.String())
}

// stripSmartPreamble consumes the `# service=git-upload-pack\n`
// pkt-line and the flush that follows. After both reads the
// [pktline.Reader] is positioned at the first byte of the actual
// advertisement; from there the wire layer parses v0/v1/v2 framing
// downstream. Behaviour matches `gitprotocol-http.adoc:281-287`.
func stripSmartPreamble(r *pktline.Reader) error {
	first, err := r.ReadPacket()
	if err != nil {
		return fmt.Errorf("read smart preamble: %w", err)
	}
	if first.Kind != pktline.Data {
		return fmt.Errorf("smart preamble: expected data packet, got %v", first.Kind)
	}
	payload := strings.TrimRight(string(first.Data), "\n")
	if payload != smartPreamblePayload {
		return fmt.Errorf("smart preamble: want %q, got %q", smartPreamblePayload, payload)
	}

	flush, err := r.ReadPacket()
	if err != nil {
		return fmt.Errorf("read smart preamble flush: %w", err)
	}
	if flush.Kind != pktline.Flush {
		return fmt.Errorf("smart preamble: want flush after service line, got %v", flush.Kind)
	}
	return nil
}

// doProbe issues a single GET against probeURL with the supplied
// headers and (optional) credentials. When tracer is non-nil it
// records one [trace.HTTPEvent] for the call regardless of how many
// redirect hops the chain follows: per-hop events would require
// either a custom round-tripper or a tracer-aware `CheckRedirect`,
// neither of which the current design takes on.
func doProbe(ctx context.Context, client *http.Client, probeURL, ua, gitProto string, creds Credentials, tracer trace.Tracer) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Git-Protocol", gitProto)
	if creds != nil {
		if err := creds.Apply(req); err != nil {
			return nil, fmt.Errorf("apply credentials: %w", err)
		}
	}
	start := time.Now()
	resp, err := client.Do(req)
	emitHTTPEvent(tracer, req, resp, err, start)
	return resp, err
}

// buildInfoRefsURL assembles the discovery URL from u. Userinfo is
// deliberately not propagated: credentials flow through the
// [CredentialResolver] seam, not the URL itself, matching how
// canonical Git separates `transport_anonymize_url` (used for
// display) from the auth path (used for headers).
func buildInfoRefsURL(u *transport.URL) string {
	out := &url.URL{
		Scheme:   u.Scheme,
		Host:     joinHostPort(u),
		Path:     strings.TrimSuffix(u.Path, "/") + "/info/refs",
		RawQuery: "service=git-upload-pack",
	}
	return out.String()
}

// joinHostPort renders u.Host (bracketing IPv6 literals) and appends
// u.Port when set, producing the `host` field expected by [url.URL].
func joinHostPort(u *transport.URL) string {
	host := u.Host
	if strings.Contains(host, ":") {
		// IPv6 literal: bracket so the optional port disambiguates.
		host = "[" + host + "]"
	}
	if u.Port != "" {
		host = host + ":" + u.Port
	}
	return host
}

// resolveUserAgent picks the User-Agent header value per the
// precedence documented on [WithUserAgent]:
// `OpenOptions.UserAgent` (non-empty) wins; otherwise the
// per-Transport value; otherwise the package default.
func resolveUserAgent(transportUA, openUA string) string {
	return cmp.Or(openUA, transportUA, defaultUserAgent)
}

// drainCap bounds how much body data the package will consume before
// closing the response in error or cleanup paths. Idle-pool reuse in
// `net/http` requires the body to reach EOF (or be closed) so leaving
// a small drain in place lets the underlying connection be reused for
// subsequent requests; capping the drain at 16 KiB stops a
// misbehaving server from pinning memory on a body that streams
// indefinitely.
const drainCap = 1 << 14

// drainAndClose reads up to [drainCap] bytes from body so the
// underlying connection can be returned to the [http.Client] pool,
// then closes the body. Errors are intentionally swallowed: the
// caller has already decided what error to surface.
func drainAndClose(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, drainCap))
	_ = body.Close()
}

// readServerExcerpt reads up to 1 KiB from body and returns it as a
// string. If more than 1 KiB was available, the returned string ends
// in `"..."` so callers can spot truncation. A partial read followed
// by an I/O error returns whatever bytes were captured before the
// error rather than discarding them.
//
// The 1 KiB cap matches what canonical Git's `show_http_message`
// surfaces from a 5xx body: enough text to triage from a log line
// without pinning unbounded memory.
func readServerExcerpt(body io.ReadCloser) string {
	if body == nil {
		return ""
	}
	const max = 1024
	buf, err := io.ReadAll(io.LimitReader(body, max+1))
	if err != nil && len(buf) == 0 {
		return ""
	}
	if len(buf) > max {
		return string(buf[:max]) + "..."
	}
	return string(buf)
}

const (
	// smartAdvContentType is the response Content-Type a smart-HTTP
	// server uses for the discovery advertisement
	// (`gitprotocol-http.adoc:227`). Comparison is case-insensitive
	// and tolerant of trailing parameters such as `; charset=utf-8`;
	// see [handleOK] for the parsing path.
	smartAdvContentType = "application/x-git-upload-pack-advertisement"

	// smartPreamblePayload is the data-packet payload that opens a
	// smart-HTTP advertisement, less the trailing newline. The wire
	// includes the LF and clients are expected to ignore it
	// (`gitprotocol-http.adoc:281-284`).
	smartPreamblePayload = "# service=git-upload-pack"
)

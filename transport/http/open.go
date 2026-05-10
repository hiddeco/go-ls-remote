package httpt

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"github.com/hiddeco/go-ls-remote/internal/wire"
	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/transport"
)

// open performs the smart-HTTP discovery probe against u and, on
// success, returns a [Conn] whose [pktline.Reader] is positioned
// past the `# service=git-upload-pack` preamble and its trailing
// flush packet. Failure modes — bad status, malformed preamble,
// auth challenges — surface either a sentinel from this package or
// a [ProtocolError] depending on the layer that detected them.
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
// The redirect policy is deliberately the [http.Client]'s default
// (10 hops, follow on GET). Canonical Git's
// `http.followRedirects=initial` policy lands in a follow-up
// change; until then the default is a workable approximation for
// the discovery GET, which is the only request this entry point
// issues.
func (t *Transport) open(ctx context.Context, u *transport.URL, opts transport.OpenOptions) (transport.Conn, error) {
	client := t.client
	if client == nil {
		client = http.DefaultClient
	}

	probeURL := buildInfoRefsURL(u)
	redacted := transport.RedactURL(probeURL)
	ua := resolveUserAgent(t.userAgent, opts.UserAgent)
	gitProto := wire.HTTPProtocolHeader(opts.PreferredProtocol)

	resp, err := doProbe(ctx, client, probeURL, ua, gitProto, nil)
	if err != nil {
		return nil, &ProtocolError{URL: redacted, Op: "probe", Err: err}
	}

	if resp.StatusCode == http.StatusUnauthorized {
		// One auth retry. A challenge-aware resolver would benefit
		// from the response's `WWW-Authenticate` value, but the
		// current [CredentialResolver] interface does not accept a
		// challenge argument; widening it lands in a follow-up
		// change. Until then the retry matches
		// `remote-curl.c::http_request_reauth` at a coarser grain.
		drainAndClose(resp.Body)

		if t.creds == nil {
			return nil, ErrAuthRequired
		}
		creds, rerr := t.creds.Resolve(ctx, infoRefsForResolver(u))
		if rerr != nil {
			return nil, fmt.Errorf("transport/http: resolve credentials: %w", rerr)
		}
		if creds == nil {
			return nil, ErrAuthRequired
		}

		resp, err = doProbe(ctx, client, probeURL, ua, gitProto, creds)
		if err != nil {
			return nil, &ProtocolError{URL: redacted, Op: "probe", Err: err}
		}
		if resp.StatusCode == http.StatusUnauthorized {
			drainAndClose(resp.Body)
			return nil, ErrAuthFailed
		}
	}

	switch resp.StatusCode {
	case http.StatusOK:
		return handleOK(resp, redacted)
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

// handleOK splits the smart and dumb branches off a `200 OK` response.
// The smart branch validates the `# service=git-upload-pack` preamble
// per `gitprotocol-http.adoc:274-286` and hands the post-preamble
// reader to a [Conn]. The dumb branch is left as a placeholder until
// the dumb-HTTP adapter lands.
func handleOK(resp *http.Response, redacted string) (transport.Conn, error) {
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		mediaType = strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Type")))
	}
	if !strings.EqualFold(mediaType, smartAdvContentType) {
		// Dumb-HTTP branch. The adapter that maps `info/refs` plus
		// the loose/packed object endpoints into a Conn lands in a
		// follow-up change.
		drainAndClose(resp.Body)
		return nil, errors.New("transport/http: dumb HTTP detected; adapter wires up in a follow-up change")
	}

	rdr := pktline.NewReader(resp.Body)
	if err := stripSmartPreamble(rdr); err != nil {
		drainAndClose(resp.Body)
		return nil, &ProtocolError{
			URL:    redacted,
			Op:     "probe",
			Status: http.StatusOK,
			Err:    err,
		}
	}
	return &Conn{
		body:   resp.Body,
		reader: rdr,
		url:    redacted,
	}, nil
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
// headers and (optional) credentials.
func doProbe(ctx context.Context, client *http.Client, probeURL, ua, gitProto string, creds Credentials) (*http.Response, error) {
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
	return client.Do(req)
}

// buildInfoRefsURL assembles the discovery URL from u. Userinfo is
// deliberately not propagated: credentials flow through the
// [CredentialResolver] seam, not the URL itself, matching how
// canonical Git separates `transport_anonymize_url` (used for
// display) from the auth path (used for headers).
func buildInfoRefsURL(u *transport.URL) string {
	host := u.Host
	if strings.Contains(host, ":") {
		// IPv6 literal: bracket so the optional port disambiguates.
		host = "[" + host + "]"
	}
	if u.Port != "" {
		host = host + ":" + u.Port
	}
	path := strings.TrimSuffix(u.Path, "/") + "/info/refs"
	out := &url.URL{
		Scheme:   u.Scheme,
		Host:     host,
		Path:     path,
		RawQuery: "service=git-upload-pack",
	}
	return out.String()
}

// infoRefsForResolver returns a [url.URL] pointing at the discovery
// endpoint, intended as the argument to a [CredentialResolver]. The
// resolver typically keys on `Host`, but Path/Scheme are populated
// for resolvers that want a finer key.
func infoRefsForResolver(u *transport.URL) *url.URL {
	host := u.Host
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if u.Port != "" {
		host = host + ":" + u.Port
	}
	return &url.URL{
		Scheme: u.Scheme,
		Host:   host,
		Path:   strings.TrimSuffix(u.Path, "/") + "/info/refs",
	}
}

// resolveUserAgent picks the User-Agent header value per the
// precedence documented on [WithUserAgent]:
// `OpenOptions.UserAgent` (non-empty) wins; otherwise the
// per-Transport value; otherwise the package default.
func resolveUserAgent(transportUA, openUA string) string {
	if openUA != "" {
		return openUA
	}
	if transportUA != "" {
		return transportUA
	}
	return defaultUserAgent
}

// drainAndClose reads up to a small bounded amount from body so the
// underlying connection can be returned to the [http.Client] pool,
// then closes the body. Errors are intentionally swallowed: the
// caller has already decided what error to surface.
func drainAndClose(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 1<<14))
	_ = body.Close()
}

// readServerExcerpt reads up to 1 KiB from body and returns it as a
// string. If more than 1 KiB was available, the returned string ends
// in `"..."` so callers can spot truncation.
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

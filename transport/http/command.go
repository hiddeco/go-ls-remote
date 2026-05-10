package httpt

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/transport"
)

// commandContentType is the request Content-Type a v2 command POST
// uses. The value is fixed by `gitprotocol-http.adoc` lines 386-395
// ("Smart Service git-upload-pack") and matched by canonical Git's
// `remote-curl.c::post_rpc` (around `remote-curl.c:1230`).
const commandContentType = "application/x-git-upload-pack-request"

// commandAcceptType is the response Content-Type a v2 command POST
// expects from the server. Servers MUST emit this exact value per
// `gitprotocol-http.adoc` lines 386-395; clients send it as the
// `Accept` header so an intermediate proxy that content-negotiates
// gets the right shape.
const commandAcceptType = "application/x-git-upload-pack-result"

// Command issues a v2 command POST against the connection's
// upload-pack endpoint and returns a [pktline.Reader] over the
// response body. The returned reader streams the response pkt-lines
// verbatim. Callers should drain the reader to release the underlying
// connection back to the [http.Client]'s pool; the [pktline.Reader]
// does not expose a Close method. [Conn.Close] releases any in-flight
// command body that has not been drained, so a caller that abandons
// the reader and closes the parent [Conn] will not leak.
//
// The on-wire request body is the canonical v2 command-request frame
// (`gitprotocol-v2.adoc` §"Command Request"):
//
//	command-request = command-line *capability-line delim-pkt *arg-line flush-pkt
//	command-line    = PKT-LINE("command=" cmd LF)
//	capability-line = PKT-LINE(cap LF)
//	arg-line        = PKT-LINE(arg LF)
//
// The headers a successful POST carries are fixed: `Content-Type:
// application/x-git-upload-pack-request`, `Accept:
// application/x-git-upload-pack-result`, the `Git-Protocol` header the
// connection negotiated at probe time (today always `version=2`), and
// the [Conn]'s configured `User-Agent`.
//
// Errors map to the package's sentinel set:
//
//   - The connection was opened against a dumb-HTTP server →
//     [ErrUnsupportedProtocol].
//   - The server responded `401` with no credentials applied →
//     [ErrAuthRequired]; with credentials applied → [ErrAuthFailed].
//   - The server responded `403` → [ErrAuthFailed].
//   - The server responded `404` → [ErrNotFound].
//   - Any `5xx` or otherwise unexpected status → [*ProtocolError]
//     with `Op == "command"`.
//
// Unlike the probe path, the command path does NOT retry on `401`
// past the resolver-supplied credentials; the discovery probe owns
// that retry per `remote-curl.c::http_request_reauth`.
func (c *Conn) Command(ctx context.Context, name string, args, caps []string) (*pktline.Reader, error) {
	if c.dumb {
		return nil, ErrUnsupportedProtocol
	}

	postURL, err := commandPostURL(c.url)
	if err != nil {
		return nil, err
	}
	redacted := transport.RedactURL(postURL.String())

	body := encodeCommandBody(name, args, caps)

	creds, err := resolveCommandCreds(ctx, c.creds, postURL, redacted)
	if err != nil {
		return nil, err
	}
	resp, err := doCommandPOST(ctx, c.client, postURL, body, c.userAgent, c.gitProtocolHeader, creds)
	if err != nil {
		// `client.Do` may return both a non-nil response and a non-nil
		// error — most notably when `CheckRedirect` rejects a 3xx hop.
		// Recent `net/http` (Go 1.21+) closes the body itself in that
		// path, but the documented contract leaves the body for the
		// caller, and a future change could revert. Drain-and-close
		// defensively so the underlying connection is released back
		// to the pool regardless of the stdlib's choice.
		if resp != nil && resp.Body != nil {
			drainAndClose(resp.Body)
		}
		if pe, ok := classifyRedirectError(err, resp, redacted, c.redir); ok {
			pe.Op = "command"
			return nil, pe
		}
		return nil, &ProtocolError{URL: redacted, Op: "command", Err: err}
	}

	rdr, err := handleCommandResponse(resp, redacted, creds != nil)
	if err != nil {
		return nil, err
	}
	// Track the response body on the [Conn] so [Conn.Close] can release
	// it if the caller abandons `rdr` without draining. Append BEFORE
	// returning so a caller that drops the result on the floor still
	// has the body covered by the next [Conn.Close].
	c.cmdBodies = append(c.cmdBodies, resp.Body)
	return rdr, nil
}

// commandPostURL derives the v2 command POST URL from the probe's
// final URL by rewriting the trailing `/info/refs` to
// `/git-upload-pack` and discarding the query string. Canonical Git
// performs the equivalent rewrite at `remote-curl.c::post_rpc` —
// the discovery URL is the same `<base>/info/refs?service=...` shape
// and the per-RPC URL drops the query and replaces the suffix.
//
// `RawPath` is cleared deliberately: when set, [url.URL.String]
// prefers it over `Path`, which would silently drop the rewrite if
// the probe URL came in with a percent-encoded path. Clearing
// `RawPath` makes `String` re-encode from `Path`.
func commandPostURL(base *url.URL) (*url.URL, error) {
	if base == nil {
		return nil, fmt.Errorf("transport/http: command: base URL is nil")
	}
	out := *base
	out.Path = strings.TrimSuffix(base.Path, "/info/refs") + "/git-upload-pack"
	out.RawPath = ""
	out.RawQuery = ""
	return &out, nil
}

// encodeCommandBody serialises a v2 command-request to a byte buffer
// using [pktline.Writer]. Layering: the v2 request body is built with
// `pktline.Writer` directly so this package keeps to its documented
// import set. Writing to a [bytes.Buffer] never fails, so the writer's
// error returns are intentionally swallowed.
func encodeCommandBody(name string, args, caps []string) []byte {
	var buf bytes.Buffer
	w := pktline.NewWriter(&buf)
	_ = w.WritePacket([]byte("command=" + name + "\n"))
	for _, cap := range caps {
		_ = w.WritePacket([]byte(cap + "\n"))
	}
	_ = w.WriteDelim()
	for _, a := range args {
		_ = w.WritePacket([]byte(a + "\n"))
	}
	_ = w.WriteFlush()
	return buf.Bytes()
}

// resolveCommandCreds consults the resolver (if any) for the POST
// URL. Returning `(nil, nil)` from the resolver — the documented
// "no credential available" signal — leaves the request anonymous;
// the server's `401` then maps to [ErrAuthRequired] downstream.
func resolveCommandCreds(ctx context.Context, r CredentialResolver, postURL *url.URL, redacted string) (Credentials, error) {
	if r == nil {
		return nil, nil
	}
	creds, err := r.Resolve(ctx, postURL)
	if err != nil {
		return nil, fmt.Errorf("transport/http: resolve credentials for %s: %w", redacted, err)
	}
	return creds, nil
}

// doCommandPOST issues a single POST with the v2 command-request
// body. It mirrors the headers `remote-curl.c::post_rpc` sets and
// then dispatches via the [Conn]'s [http.Client] so the redirect
// policy, cookie jar, and any test hooks the caller installed
// continue to apply.
func doCommandPOST(ctx context.Context, client *http.Client, postURL *url.URL, body []byte,
	ua, gitProto string, creds Credentials) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, postURL.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", commandContentType)
	req.Header.Set("Accept", commandAcceptType)
	req.Header.Set("Git-Protocol", gitProto)
	req.Header.Set("User-Agent", ua)
	if creds != nil {
		if err := creds.Apply(req); err != nil {
			return nil, fmt.Errorf("apply credentials: %w", err)
		}
	}
	return client.Do(req)
}

// handleCommandResponse maps the server's status code to either a
// [pktline.Reader] over the response body (the success case) or the
// matching package sentinel / [*ProtocolError]. It mirrors the probe
// path's status table at the [Conn.Command] doc comment, with the
// single auth-retry semantics inverted: command POSTs do not retry,
// because the probe has already paid the anonymous-then-creds round
// trip and the server's challenge state is not expected to change
// between the probe and the first command.
func handleCommandResponse(resp *http.Response, redacted string, credsApplied bool) (*pktline.Reader, error) {
	switch resp.StatusCode {
	case http.StatusOK:
		return pktline.NewReader(resp.Body), nil
	case http.StatusUnauthorized:
		drainAndClose(resp.Body)
		if !credsApplied {
			return nil, ErrAuthRequired
		}
		return nil, ErrAuthFailed
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
			Op:     "command",
			Status: resp.StatusCode,
			Server: server,
		}
	}

	server := readServerExcerpt(resp.Body)
	_ = resp.Body.Close()
	return nil, &ProtocolError{
		URL:    redacted,
		Op:     "command",
		Status: resp.StatusCode,
		Server: server,
		Err:    fmt.Errorf("unexpected status %d", resp.StatusCode),
	}
}

package httpt

import (
	"errors"
	"fmt"
	"strings"
)

// Sentinel errors returned by [Transport.Open] for the auth and
// not-found branches of the smart-HTTP probe. Callers match with
// [errors.Is].
//
// Each sentinel carries the `transport/http:` prefix so a stray log
// line is grep-friendly. The root `lsremote` package re-wraps these
// into its own public sentinels; inside this package the sentinels
// below are the canonical match targets.
var (
	// ErrAuthRequired is returned when the server demands
	// authentication and the [Transport] has no [CredentialResolver]
	// (or the resolver yielded `(nil, nil)`). The caller is expected
	// to plug in credentials and retry.
	ErrAuthRequired = errors.New("transport/http: authentication required")

	// ErrAuthFailed is returned when credentials were applied but the
	// server rejected them — either a `401` after the auth retry or a
	// `403` on any request. The credential is not retried again.
	ErrAuthFailed = errors.New("transport/http: authentication failed")

	// ErrNotFound is returned when the server reports `404 Not Found`
	// for the discovery URL. Canonical Git surfaces the same condition
	// as `repository '%s' not found` (see `remote-curl.c::discover_refs`,
	// `HTTP_MISSING_TARGET`).
	ErrNotFound = errors.New("transport/http: repository not found")
)

// ProtocolError reports an HTTP-layer protocol failure that does not
// fit one of the [errors.Is]-matched sentinels above — for example a
// `5xx` status, a malformed smart-HTTP preamble, or an unexpected
// status code.
//
// The root `lsremote` package re-wraps this into its own public
// `*ProtocolError` shape; inside this package callers may match the
// wrapped [ProtocolError.Err] with [errors.Is].
//
// Field order clusters the 16-byte string headers ahead of the
// pointer-shaped Err and the int Status so the struct packs without
// padding on 64-bit platforms.
type ProtocolError struct {
	// URL is the request URL with credentials redacted via
	// [transport.RedactURL]. It is included so log lines and error
	// strings are diagnostic without leaking userinfo.
	URL string

	// Op is the operation that failed. The probe path uses `"probe"`;
	// the command path lands in a follow-up change and will use
	// `"command"`.
	Op string

	// Server is a truncated server-sent response body (≤ 1 KiB) that
	// helps diagnose otherwise opaque `5xx` failures. Empty when the
	// body was empty or the failure is not server-message-shaped.
	Server string

	// Err is the wrapped cause. It typically matches one of the
	// codec-level errors from [pktline] or a hand-built `errors.New`
	// describing the protocol violation. Nil when no specific cause
	// applies.
	Err error

	// Status is the HTTP status code, or 0 when the failure occurred
	// before a response was received.
	Status int
}

// Error formats a one-line diagnostic of the form
// `transport/http: <op> <URL>: HTTP <status>: <cause>`. Components
// that are zero-valued are elided so a pre-response failure prints
// without `HTTP 0`.
func (e *ProtocolError) Error() string {
	var b strings.Builder
	b.WriteString("transport/http:")
	if e.Op != "" {
		b.WriteByte(' ')
		b.WriteString(e.Op)
	}
	if e.URL != "" {
		b.WriteByte(' ')
		b.WriteString(e.URL)
	}
	if e.Status != 0 {
		fmt.Fprintf(&b, ": HTTP %d", e.Status)
	}
	cause := ""
	switch {
	case e.Err != nil:
		cause = e.Err.Error()
	case e.Server != "":
		cause = e.Server
	}
	if cause != "" {
		b.WriteString(": ")
		b.WriteString(cause)
	}
	return b.String()
}

// Unwrap returns the wrapped cause so [errors.Is] and [errors.As]
// see through to it.
func (e *ProtocolError) Unwrap() error { return e.Err }

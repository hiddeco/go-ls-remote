package lsremote

import (
	"errors"
	"fmt"
	"strings"

	"github.com/hiddeco/go-ls-remote/transport"
)

// Sentinel errors returned by the discovery surface. Each carries the
// `lsremote:` prefix so a stray log line is grep-friendly, and each is
// matched with [errors.Is] — typically against a [ProtocolError]
// whose [ProtocolError.Err] field wraps the sentinel directly or
// through one or more transport-layer wrappers.
//
// Transport-specific sentinels (e.g. `transport/http.ErrNotFound`)
// are deliberately not re-exported: the [errors.Is] chain walks
// through [ProtocolError.Unwrap], so a caller writes
// `errors.Is(err, lsremote.ErrNotFound)` without having to know which
// transport produced the failure.
var (
	// ErrNotFound signals the remote does not host the requested
	// repository. HTTP transports surface a `404`, SSH transports the
	// `fatal: '<path>' does not appear to be a git repository` error
	// line, and `file://` transports a missing-directory check.
	ErrNotFound = errors.New("lsremote: repository not found")

	// ErrAuthRequired signals the server demanded authentication and
	// none was offered (or the caller's credential resolver yielded
	// nothing). Callers are expected to plug in credentials and retry.
	ErrAuthRequired = errors.New("lsremote: authentication required")

	// ErrAuthFailed signals the server rejected the supplied
	// credentials. The library does not retry on its own; the caller
	// should refresh credentials before another attempt.
	ErrAuthFailed = errors.New("lsremote: authentication failed")

	// ErrUnsupportedProtocol signals the remote cannot honour the
	// requested protocol or operation — for example a dumb-HTTP
	// server when the caller asked for a v2 command, or an
	// `object-info` request against a v2 server that did not
	// advertise the command.
	ErrUnsupportedProtocol = errors.New("lsremote: protocol/operation not supported by server")

	// ErrServerRefused signals the server returned an explicit
	// `ERR <message>` pkt-line or otherwise refused the request after
	// the connection was established. The originating message is
	// preserved on [ProtocolError.Server] when available.
	ErrServerRefused = errors.New("lsremote: server refused")

	// ErrNoDefaultBranch signals that the remote repository is reachable
	// but HEAD has no symbolic target — it is either detached or the
	// server omitted the symref mapping from its advertisement. Returned
	// by [DefaultBranch] when no HEAD symref can be resolved. The
	// surrounding [ProtocolError] carries `Op == "ls-refs"` on a v2
	// server (the mapping is sought via the `ls-refs` command) and
	// `Op == "advertisement"` on a v0/v1 server (the mapping is sought
	// in the capability advertisement). Use [errors.Is] to distinguish
	// this from [ErrNotFound], which means the repository itself is
	// absent.
	ErrNoDefaultBranch = errors.New("lsremote: remote has no default branch")
)

// ProtocolError carries diagnostic context for a protocol-level
// failure raised by the discovery surface. It wraps an underlying
// cause — typically one of the package-level sentinels above — and
// adds enough context (URL, operation, negotiated version, HTTP
// status, server-supplied message) for a caller to log or report the
// failure without having to reach into transport internals.
//
// # Matching with errors.Is
//
// [ProtocolError.Unwrap] returns [ProtocolError.Err], so
// `errors.Is(perr, ErrNotFound)` succeeds when `perr.Err` is
// `ErrNotFound` or transitively wraps it via one or more
// `fmt.Errorf("...: %w", ...)` calls.
//
// # The Op values
//
// [ProtocolError.Op] names the discovery step that failed:
//
//   - `"dial"` — connection setup before the wire handshake began.
//   - `"probe"` — the initial discovery request (`GET .../info/refs`,
//     or the equivalent on non-HTTP transports) used to detect
//     protocol version and capabilities.
//   - `"advertisement"` — parsing the server's capability/ref
//     advertisement.
//   - `"ls-refs"` — the v2 `ls-refs` command exchange.
//   - `"object-info"` — the v2 `object-info` command exchange.
//
// # Field invariants
//
// [ProtocolError.URL] is always credential-redacted via
// [transport.RedactURL] before it surfaces to a caller. The
// formatter in [ProtocolError.Error] applies the redaction on every
// call, so a caller cannot accidentally leak a password by reading
// the field directly and concatenating it into a log line — though
// callers are nevertheless encouraged to populate the field with an
// already-redacted value.
//
// [ProtocolError.Server] is bounded to at most 1 KiB. The library
// truncates server-supplied diagnostics at every construction site;
// the type itself does not re-truncate, so a caller who constructs a
// [ProtocolError] from a server response body should cap the message
// at 1 KiB before storing it.
//
// # Format
//
// [ProtocolError.Error] returns a one-line summary of the form
//
//	lsremote: <Op>: <Err> (URL <redacted-url>) [status N] [server: <truncated>]
//
// The bracketed bits are elided when their backing field is zero:
// `status N` is omitted when [ProtocolError.Status] is zero, and the
// `server:` section is omitted when [ProtocolError.Server] is empty.
// A nil [ProtocolError.Err] is printed as `<nil cause>` so the
// formatter never panics.
type ProtocolError struct {
	// URL is the request URL with credentials redacted via
	// [transport.RedactURL]. The redaction is reapplied by
	// [ProtocolError.Error] so the formatted output is always safe
	// to log.
	URL string

	// Op identifies the discovery step that failed. See the type
	// doc for the allowed values.
	Op string

	// Version is the negotiated [ProtocolVersion], or nil when the
	// error occurred before the handshake completed.
	Version *ProtocolVersion

	// Server is the server-supplied error message (a `ERR` pkt-line
	// payload, an HTTP body excerpt, or similar). Bounded to at most
	// 1 KiB; library construction sites truncate before storing.
	Server string

	// Err is the wrapped cause. It typically matches one of the
	// package sentinels via [errors.Is]; nil when no specific cause
	// is available.
	Err error

	// Status is the HTTP status code, or 0 when the failure
	// occurred over a non-HTTP transport or before a response was
	// received.
	Status int
}

// Error formats e as a single line of the form documented on
// [ProtocolError]. The URL is credential-redacted via
// [transport.RedactURL] on every call, so the returned string is
// always safe to log.
func (e *ProtocolError) Error() string {
	var b strings.Builder
	b.WriteString("lsremote:")
	if e.Op != "" {
		b.WriteByte(' ')
		b.WriteString(e.Op)
		b.WriteByte(':')
	}
	b.WriteByte(' ')
	if e.Err != nil {
		b.WriteString(e.Err.Error())
	} else {
		b.WriteString("<nil cause>")
	}
	if e.URL != "" {
		fmt.Fprintf(&b, " (URL %s)", transport.RedactURL(e.URL))
	}
	if e.Status != 0 {
		fmt.Fprintf(&b, " [status %d]", e.Status)
	}
	if e.Server != "" {
		b.WriteString(" [server: ")
		b.WriteString(e.Server)
		b.WriteByte(']')
	}
	return b.String()
}

// Unwrap returns the wrapped cause so [errors.Is] and [errors.As]
// walk through e to the underlying sentinel or transport error.
func (e *ProtocolError) Unwrap() error { return e.Err }

// truncateServer caps s at 1 KiB so a server-supplied diagnostic
// payload cannot bloat a [ProtocolError]. The library applies it at
// every construction site that copies bytes from a server response
// into [ProtocolError.Server]; callers outside the library do not
// need to invoke it because they do not construct [ProtocolError]
// values directly.
func truncateServer(s string) string {
	const max = 1024
	if len(s) <= max {
		return s
	}
	return s[:max]
}

package ssht

import (
	"strings"

	"github.com/hiddeco/go-ls-remote/transport"
)

// Sentinel errors returned by [Transport.Open]. Callers match with
// [errors.Is]; the wrapping `*ProtocolError` carries the offending URL
// and operation so a stray log line is diagnostic without leaking
// userinfo.
//
// Each is declared as a [*transport.SchemeError] bound to its generic
// parent so `errors.Is(err, transport.ErrAuthFailed)` (and the like)
// matches without the caller having to know which scheme produced the
// failure. The bridging contract is documented on [transport.SchemeError].
var (
	// ErrAuthRequired is returned when the SSH server demands
	// authentication and the [Transport] supplied none — typically the
	// `*ssh.ClientConfig.Auth` field is empty because no
	// [AuthResolver] was wired via [WithAuth] (and the optional
	// [WithClientConfig] template did not carry methods either).
	ErrAuthRequired = &transport.SchemeError{
		Parent: transport.ErrAuthRequired,
		Msg:    "transport/ssh: authentication required",
	}

	// ErrAuthFailed is returned when the SSH handshake reached the
	// server but the server rejected every [ssh.AuthMethod] offered.
	// The underlying x/crypto/ssh error has the shape
	// `ssh: handshake failed: ssh: unable to authenticate, attempted methods [...]`;
	// [Transport.Open] detects it heuristically (see `open.go`) because
	// x/crypto/ssh does not expose a typed client-side auth-failure
	// error in this code path.
	ErrAuthFailed = &transport.SchemeError{
		Parent: transport.ErrAuthFailed,
		Msg:    "transport/ssh: authentication failed",
	}

	// ErrNotFound is returned when the remote `git-upload-pack` exec
	// exits with a "repository not found"-shaped stderr diagnostic.
	// The mapping is best-effort: SSH does not standardise this error
	// shape, so the heuristic matches canonical Git's wording without
	// over-claiming. Unrecognised failure stderr surfaces as a generic
	// `*ProtocolError` instead.
	ErrNotFound = &transport.SchemeError{
		Parent: transport.ErrNotFound,
		Msg:    "transport/ssh: repository not found",
	}
)

// ProtocolError reports a dial-time protocol failure that does not fit
// one of the [errors.Is]-matched sentinels above. Field order clusters
// the 16-byte string headers ahead of the pointer-shaped Err so the
// struct packs without padding on 64-bit platforms.
type ProtocolError struct {
	// URL is the dial URL with credentials redacted via
	// [transport.RedactURL]. SSH URLs typically carry no password
	// component, so the value is usually verbatim; the redaction step
	// runs unconditionally to stay symmetric with the HTTP transport.
	URL string

	// Op identifies the operation. Today the only value is `"dial"`;
	// future per-command failures will reuse the field with a
	// command-specific value.
	Op string

	// Server carries an excerpt of a server-sent diagnostic (e.g. an
	// `ERR` packet or the first stderr line of `git-upload-pack`), when
	// one is available.
	Server string

	// Err is the wrapped cause. It typically matches one of this
	// package's sentinels (`ErrAuthFailed`, `ErrAuthRequired`,
	// `ErrNotFound`) so [errors.Is] sees through the wrapper; for
	// unmapped failures (TCP refused, host-key mismatch, etc.) it
	// carries the underlying error verbatim.
	Err error
}

// Error formats a one-line diagnostic of the form
// `transport/ssh: <op> <URL>: <cause>`. Zero-valued components are
// elided so the rendered form stays compact when only some fields are
// populated.
func (e *ProtocolError) Error() string {
	var b strings.Builder
	b.WriteString("transport/ssh:")
	if e.Op != "" {
		b.WriteByte(' ')
		b.WriteString(e.Op)
	}
	if e.URL != "" {
		b.WriteByte(' ')
		b.WriteString(e.URL)
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

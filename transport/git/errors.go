package gitt

import (
	"strings"

	"github.com/hiddeco/go-ls-remote/transport"
)

// ErrUnsupportedProtocol is returned when
// [transport.OpenOptions.PreferredProtocol] pins v1. The git-daemon
// server advertises v0 and v2 only; v1 is effectively unused in the
// canonical Git ecosystem (`serve.c` advertises v2, `upload-pack.c`
// drives v0 or v2 per negotiation). The sentinel bridges to the
// generic parent so `errors.Is(err, transport.ErrUnsupportedProtocol)`
// matches without the caller having to know which scheme produced the
// failure.
var ErrUnsupportedProtocol = &transport.SchemeError{
	Parent: transport.ErrUnsupportedProtocol,
	Msg:    "transport/git: unsupported protocol version",
}

// ProtocolError reports a dial-time protocol failure that does not fit
// one of the [errors.Is]-matched sentinels above. Field order clusters
// the 16-byte string headers ahead of the pointer-shaped Err so the
// struct packs without padding on 64-bit platforms.
type ProtocolError struct {
	// URL is the dial URL with credentials redacted via
	// [transport.RedactURL]. `git://` URLs carry no userinfo in
	// practice, but the redaction step runs unconditionally to stay
	// symmetric with the HTTP transport.
	URL string

	// Op identifies the operation. Today the only value is `"dial"`;
	// future per-command failures will reuse the field with a
	// command-specific value.
	Op string

	// Server carries an excerpt of a server-sent diagnostic, when one
	// is available. git-daemon does not send structured error packets,
	// so this field is populated only on future extensions.
	Server string

	// Err is the wrapped cause. It typically matches
	// [ErrUnsupportedProtocol] so [errors.Is] sees through the
	// wrapper; for unmapped failures (TCP refused, DNS error, etc.) it
	// carries the underlying error verbatim.
	Err error
}

// Error formats a one-line diagnostic of the form
// `transport/git: <op> <URL>: <cause>`. Components that are
// zero-valued are elided.
func (e *ProtocolError) Error() string {
	var b strings.Builder
	b.WriteString("transport/git")
	if e.Op != "" {
		b.WriteString(": ")
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

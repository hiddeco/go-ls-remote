package filet

import (
	"errors"
	"strings"
)

// Sentinel errors returned by [Transport.Open]. Callers match with
// [errors.Is]; the wrapping `*ProtocolError` carries the offending URL
// and operation so a stray log line is diagnostic without leaking
// userinfo.
var (
	// ErrNotFound is returned when the supplied URL does not name a
	// readable Git repository. The local-filesystem transport surfaces
	// the same condition for two layers: the URL's path is not a
	// repository on disk (`objstore.ErrNotARepo`), or its
	// percent-escapes are malformed and the URL cannot be resolved at
	// all. Both shapes are callable equivalents to canonical Git's
	// `repository '%s' not found` (`remote-curl.c::discover_refs`,
	// `HTTP_MISSING_TARGET`).
	ErrNotFound = errors.New("transport/file: repository not found")

	// ErrUnsupportedProtocol is returned when [transport.OpenOptions.PreferredProtocol]
	// pins a wire version this transport does not implement. The
	// emulator speaks v0 and v2; v1 is effectively unused in the
	// canonical Git ecosystem (`serve.c` and `upload-pack.c` advertise
	// v0 and v2 only). A future reftable-aware variant may surface the
	// same sentinel for unsupported repository formats.
	ErrUnsupportedProtocol = errors.New("transport/file: unsupported repository format")

	// ErrServerRefused is returned when the underlying object store
	// rejects the open with a structural error a fresh-dial caller
	// cannot recover from (e.g. a corrupt object surfaced by
	// `objstore.ErrCorruptObject`). The wrapping `*ProtocolError`
	// carries the corruption message in `Server`.
	ErrServerRefused = errors.New("transport/file: server refused")
)

// ProtocolError reports a dial-time protocol failure that does not fit
// one of the [errors.Is]-matched sentinels above. Field order clusters
// the 16-byte string headers ahead of the pointer-shaped Err so the
// struct packs without padding on 64-bit platforms.
type ProtocolError struct {
	// URL is the dial URL with credentials redacted via
	// [transport.RedactURL]. `file://` URLs typically carry no
	// userinfo, so the value is usually verbatim; the redaction step
	// runs unconditionally to stay symmetric with the HTTP transport.
	URL string

	// Op identifies the operation. Today the only value is `"dial"`;
	// future per-command failures will reuse the field with a
	// command-specific value.
	Op string

	// Server carries an excerpt of a server-sent diagnostic, when one
	// is available. The local-filesystem transport surfaces this only
	// for the corrupt-object branch of `objstore.Open`, where the
	// wrapped `objstore.ErrCorruptObject` carries the offending hash
	// and pack location.
	Server string

	// Err is the wrapped cause. It typically matches one of this
	// package's sentinels (`ErrNotFound`, `ErrUnsupportedProtocol`,
	// `ErrServerRefused`) so [errors.Is] sees through the wrapper.
	Err error
}

// Error formats a one-line diagnostic of the form
// `transport/file: <op> <URL>: <cause>`. Components that are
// zero-valued are elided.
func (e *ProtocolError) Error() string {
	var b strings.Builder
	b.WriteString("transport/file:")
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

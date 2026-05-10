package transport

import "errors"

// Generic transport-level sentinel identities. Scheme-specific
// packages (e.g. `transport/http`, `transport/file`) declare their own
// sentinels as [SchemeError] values whose `Parent` points at one of
// the four below. A caller's `errors.Is(err, transport.ErrNotFound)`
// then walks through the scheme-specific sentinel and matches without
// having to know which scheme produced the failure.
//
// The set is deliberately small: each entry names a well-defined
// failure mode every transport may surface. Transport-specific
// concepts (`filet.ErrServerRefused`, `filet.ErrUnsupportedFormat`)
// stay scheme-local and do not bridge here.
var (
	// ErrNotFound is returned when the named repository does not
	// exist on the server. The HTTP transport surfaces this for a
	// `404` on the discovery URL; the file transport surfaces it for
	// a missing repository path.
	ErrNotFound = errors.New("transport: repository not found")

	// ErrAuthRequired is returned when the server demands
	// authentication and the transport has no credentials to supply
	// (or the supplied credentials were exhausted on a prior probe).
	// The caller is expected to plug in credentials and retry.
	ErrAuthRequired = errors.New("transport: authentication required")

	// ErrAuthFailed is returned when the server rejected an
	// authenticated request: the credentials reached the server but
	// did not satisfy it.
	ErrAuthFailed = errors.New("transport: authentication failed")

	// ErrUnsupportedProtocol is returned when the transport cannot
	// satisfy the requested protocol version or operation. The HTTP
	// transport surfaces this for a dumb-HTTP server asked to handle
	// v2 commands; the file transport surfaces it for a pinned wire
	// version it does not implement.
	ErrUnsupportedProtocol = errors.New("transport: protocol/operation not supported")
)

// SchemeError binds a scheme-specific sentinel to one of the generic
// transport-level identities above so `errors.Is` matches both. The
// rendered `Error()` text is whatever the scheme-specific package
// chose; the bridge to the generic identity happens through `Is`.
//
// Scheme-specific packages declare their sentinels as
// `*SchemeError` values, for example:
//
//	var ErrNotFound = &transport.SchemeError{
//	    Parent: transport.ErrNotFound,
//	    Msg:    "transport/http: repository not found",
//	}
//
// User-defined transports following the same pattern interoperate
// with the root package's `errors.Is` checks automatically.
type SchemeError struct {
	// Parent is the generic transport-level identity this
	// scheme-specific sentinel binds to. It MUST be one of
	// [ErrNotFound], [ErrAuthRequired], [ErrAuthFailed], or
	// [ErrUnsupportedProtocol]; other values defeat the bridge.
	Parent error

	// Msg is the rendered diagnostic, including the scheme prefix
	// (e.g. `transport/http:`). It is returned verbatim by
	// [SchemeError.Error].
	Msg string
}

// Error returns the scheme-specific message verbatim.
func (e *SchemeError) Error() string { return e.Msg }

// Is reports whether target matches this sentinel — either by
// pointer identity, or by being the [SchemeError.Parent] generic
// transport-level identity. The dual match is what bridges a
// scheme-specific sentinel into the public `errors.Is` chain
// without changing its rendered text.
func (e *SchemeError) Is(target error) bool {
	return target == e || target == e.Parent
}

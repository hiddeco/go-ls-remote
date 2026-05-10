package transport

import "strings"

// RedactURL returns s with any password component of its userinfo
// replaced by `***`. The username (without password) is preserved.
// Inputs that do not parse as RFC URLs with userinfo are returned
// unchanged — the function is best-effort, suitable for diagnostic
// output where a complete parse cycle would be overkill.
//
// Specifically:
//
//   - `https://alice:secret@host/path` → `https://alice:***@host/path`
//   - `https://alice@host/path` (no password) → unchanged
//   - `ssh://git@host/path` → unchanged (no password)
//   - `git@github.com:owner/repo` (scp-style) → unchanged (the
//     function only handles RFC URLs; scp-style with passwords is
//     vanishingly rare)
//   - non-RFC inputs → unchanged
//
// The userinfo terminator is the *last* `@` before the path, per
// RFC 3986 §3.2: an unencoded `@` inside a password is technically
// a violation but turns up in real inputs, and splitting on the
// first `@` would leak the password remainder into the host portion
// of the output.
//
// Use this on tracer event URLs, log lines, and error messages
// derived from user-supplied URLs.
func RedactURL(s string) string {
	scheme := strings.Index(s, "://")
	if scheme < 0 {
		return s
	}
	rest := s[scheme+3:]
	authority, _, _ := strings.Cut(rest, "/")
	at := strings.LastIndexByte(authority, '@')
	if at < 0 {
		return s
	}
	userinfo := authority[:at]
	colon := strings.IndexByte(userinfo, ':')
	if colon < 0 {
		// User-only userinfo, no password to redact.
		return s
	}
	return s[:scheme+3] + userinfo[:colon+1] + "***" + rest[at:]
}

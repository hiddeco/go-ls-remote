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
// Use this on tracer event URLs, log lines, and error messages
// derived from user-supplied URLs.
func RedactURL(s string) string {
	scheme := strings.Index(s, "://")
	if scheme < 0 {
		return s
	}
	rest := s[scheme+3:]
	at := strings.IndexByte(rest, '@')
	slash := strings.IndexByte(rest, '/')
	if at < 0 || (slash >= 0 && at > slash) {
		// `@` belongs to the path or is absent; no userinfo.
		return s
	}
	userinfo := rest[:at]
	colon := strings.IndexByte(userinfo, ':')
	if colon < 0 {
		// User-only userinfo, no password to redact.
		return s
	}
	return s[:scheme+3] + userinfo[:colon+1] + "***" + rest[at:]
}

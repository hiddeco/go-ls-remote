package transport

import (
	"errors"
	"fmt"
	"strings"
)

// Sentinel errors returned by [ParseURL]. Match with [errors.Is]; the
// wrapping `fmt.Errorf("%w: ...")` adds the offending input for
// diagnostics.
var (
	// ErrEmptyURL is returned when the input string is empty.
	ErrEmptyURL = errors.New("transport: empty URL")

	// ErrUnsupportedScheme is returned when an explicit `<scheme>://`
	// names a scheme not in {http, https, ssh, git, file}.
	ErrUnsupportedScheme = errors.New("transport: unsupported scheme")

	// ErrInvalidIPv6 is returned when a bracketed IPv6 literal is
	// unterminated or has trailing junk after the closing bracket.
	ErrInvalidIPv6 = errors.New("transport: invalid IPv6 literal")

	// ErrMissingHost is returned when an RFC URL omits its host.
	ErrMissingHost = errors.New("transport: missing host")

	// ErrUnrecognizedURL is returned when the input matches none of
	// the supported URL forms.
	ErrUnrecognizedURL = errors.New("transport: unrecognized URL form")
)

// URL is the parsed form of a Git remote URL. It accepts the same set
// of URL forms canonical Git accepts in `connect.c::parse_connect_url`.
//
// Field order is chosen so 16-byte string headers and the slice-shaped
// fields cluster naturally; the struct has no inter-field padding on
// 64-bit platforms.
type URL struct {
	Scheme string // canonical: "https", "http", "ssh", "git", or "file"
	User   string // userinfo: "user", "user:pass", or ""
	Host   string // hostname or IP literal (no brackets around IPv6)
	Port   string // port without the colon; "" for transport default
	Path   string // repository path (begins with "/" for non-file schemes)
	Raw    string // verbatim input, for diagnostics
}

// ParseURL parses s into a [URL].
//
// # Supported forms
//
//   - `https://[user[:pass]@]host[:port]/path`
//   - `http://...`
//   - `ssh://[user@]host[:port]/path`
//   - scp-style `[user@]host:path` — normalised to Scheme `"ssh"`
//   - `git://host[:port]/path`
//   - `file:///abs/path` and the bare `/abs/path` shorthand
//
// IPv6 literals must be bracketed when followed by a port:
// `[fe80::1]:8443`. In scp-style URLs the bracket form is required
// because the unbracketed colon is the host/path separator.
//
// On error, the returned error wraps one of [ErrEmptyURL],
// [ErrUnsupportedScheme], [ErrInvalidIPv6], [ErrMissingHost], or
// [ErrUnrecognizedURL].
func ParseURL(s string) (*URL, error) {
	if s == "" {
		return nil, ErrEmptyURL
	}
	u := &URL{Raw: s}

	// Detect scheme. "://" anchors RFC URLs.
	if i := strings.Index(s, "://"); i > 0 {
		scheme := strings.ToLower(s[:i])
		rest := s[i+3:]
		switch scheme {
		case "https", "http", "ssh", "git", "file":
			u.Scheme = scheme
		default:
			return nil, fmt.Errorf("%w: %q", ErrUnsupportedScheme, scheme)
		}
		if u.Scheme == "file" {
			u.Path = "/" + strings.TrimPrefix(rest, "/")
			return u, nil
		}
		if err := parseAuthorityPath(u, rest); err != nil {
			return nil, err
		}
		return u, nil
	}

	// Bare absolute path → `file` scheme. Canonical Git treats this as
	// `file://...` in `connect.c::url_is_local`. Relative paths fall
	// through and are rejected as unrecognised — they are ambiguous
	// with scp-style URLs missing the colon and with garbage inputs.
	if strings.HasPrefix(s, "/") {
		u.Scheme = "file"
		u.Path = s
		return u, nil
	}

	// scp-style: `[user@]host:path`. Per `connect.c::parse_connect_url`
	// the first `:` outside an IPv6 bracket and before any `/` is the
	// host/path separator; scp-style has no port, so `host:22:rest`
	// parses as host=`host`, path=`22:rest`.
	if i, ok := scpSeparator(s); ok {
		u.Scheme = "ssh"
		hostPart := s[:i]
		path := s[i+1:]
		if at := strings.LastIndex(hostPart, "@"); at >= 0 {
			u.User = hostPart[:at]
			hostPart = hostPart[at+1:]
		}
		// IPv6 in scp-style requires brackets, since the unbracketed
		// colons would each be candidate separators.
		if strings.HasPrefix(hostPart, "[") && strings.HasSuffix(hostPart, "]") {
			u.Host = hostPart[1 : len(hostPart)-1]
		} else {
			u.Host = hostPart
		}
		if u.Host == "" {
			return nil, fmt.Errorf("%w: %q", ErrMissingHost, u.Raw)
		}
		u.Path = "/" + path
		return u, nil
	}

	return nil, fmt.Errorf("%w: %q", ErrUnrecognizedURL, s)
}

// scpSeparator returns the index of the colon that separates host
// from path in an scp-style URL, or (0, false) if s is not scp-style.
//
// scp-style requires:
//
//   - a colon to be present;
//   - the colon to come before any `/` (an earlier `/` means it's a
//     path, not scp-style);
//   - the colon to be outside any bracketed IPv6 literal.
func scpSeparator(s string) (int, bool) {
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '[':
			depth++
		case ']':
			if depth > 0 {
				depth--
			}
		case '/':
			if depth == 0 {
				return 0, false
			}
		case ':':
			if depth == 0 {
				return i, true
			}
		}
	}
	return 0, false
}

// parseAuthorityPath parses `[user[:pass]@]host[:port]/path` into u.
// An empty path is allowed and yields u.Path == "".
func parseAuthorityPath(u *URL, s string) error {
	// Userinfo, if any, ends at the last `@` before any `/`.
	if at := strings.LastIndex(splitBeforeSlash(s), "@"); at >= 0 {
		u.User = s[:at]
		s = s[at+1:]
	}
	// Host (and optional port) ends at the first `/`; the rest is path.
	host := s
	if slash := strings.Index(s, "/"); slash >= 0 {
		host = s[:slash]
		u.Path = s[slash:]
	}
	// IPv6 bracketed?
	if strings.HasPrefix(host, "[") {
		end := strings.IndexByte(host, ']')
		if end < 0 {
			return fmt.Errorf("%w: %q", ErrInvalidIPv6, u.Raw)
		}
		u.Host = host[1:end]
		rest := host[end+1:]
		switch {
		case rest == "":
			// no port
		case strings.HasPrefix(rest, ":"):
			u.Port = rest[1:]
		default:
			return fmt.Errorf("%w: junk after closing bracket in %q", ErrInvalidIPv6, u.Raw)
		}
		return nil
	}
	// host[:port]
	if colon := strings.LastIndex(host, ":"); colon >= 0 {
		u.Host = host[:colon]
		u.Port = host[colon+1:]
	} else {
		u.Host = host
	}
	if u.Host == "" {
		return fmt.Errorf("%w: %q", ErrMissingHost, u.Raw)
	}
	return nil
}

// splitBeforeSlash returns s up to the first `/`, or all of s if none.
// Used to constrain `@` searches to the authority portion of an RFC URL.
func splitBeforeSlash(s string) string {
	before, _, _ := strings.Cut(s, "/")
	return before
}

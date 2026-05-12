// Package httpt is the HTTP/HTTPS Git transport. The directory is named
// `transport/http` for parity with canonical Git's source layout, while
// the Go package is named `httpt` to avoid colliding with stdlib
// `net/http` at the import site.
//
// The authentication seam lives here: [Credentials] is the interface a
// request-modifying credential satisfies, and [CredentialResolver] is
// the strategy the transport consults to obtain one for a given URL.
// Built-in credentials cover RFC 7617 Basic and RFC 6750 Bearer;
// built-in resolvers cover a constant credential ([Static]) and a
// `~/.netrc` parser ([Netrc]).
package httpt

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Credentials is something that can attach an authentication artefact to
// an outbound HTTP request. The [Transport] resolves a Credentials value
// once per dial via a [CredentialResolver] and applies it to each
// request the connection issues.
//
// Implementations mutate r in place and return any I/O or encoding
// error. They must be safe for concurrent use across distinct requests:
// the [Transport] may pipeline.
type Credentials interface {
	// Apply attaches the credential to r. Implementations typically set
	// the `Authorization` header.
	Apply(r *http.Request) error
}

// Basic returns a [Credentials] that sets HTTP Basic auth (RFC 7617) on
// every request it is applied to.
//
// An empty user or pass is a programmer error: the constructor does not
// validate, so an empty `Basic("", "")` produces an `Authorization:
// Basic Og==` header that almost no server accepts. Validate at the
// call site if the inputs are user-supplied.
func Basic(user, pass string) Credentials {
	return &basicCreds{user: user, pass: pass}
}

type basicCreds struct {
	user, pass string
}

// Apply sets the `Authorization` header on r per RFC 7617.
func (b *basicCreds) Apply(r *http.Request) error {
	r.SetBasicAuth(b.user, b.pass)
	return nil
}

// Bearer returns a [Credentials] that sets the `Authorization: Bearer
// <token>` header (RFC 6750 §2.1) on every request it is applied to.
//
// An empty token is a programmer error: the constructor does not
// validate. Most servers reject `Bearer ` (with no token) outright.
func Bearer(token string) Credentials {
	return &bearerCreds{token: token}
}

type bearerCreds struct {
	token string
}

// Apply sets the `Authorization: Bearer <token>` header on r.
func (b *bearerCreds) Apply(r *http.Request) error {
	r.Header.Set("Authorization", "Bearer "+b.token)
	return nil
}

// CredentialResolver supplies a [Credentials] for a given URL. The
// [Transport] consults a CredentialResolver once per dial.
//
// A resolver may return `(nil, nil)` to signal "no credential
// available for this URL". The transport treats that as "anonymous";
// if the server then demands authentication the transport surfaces
// `ErrAuthRequired` (defined in a later step).
type CredentialResolver interface {
	// Resolve returns the credential (if any) to use for u. ctx may be
	// honoured by resolvers that perform I/O (for example a future
	// `git-credential` helper resolver). u is the URL that triggered
	// the dial; resolvers typically key on `u.Host`.
	Resolve(ctx context.Context, u *url.URL) (Credentials, error)
}

// Static returns a [CredentialResolver] that always yields c. Pass nil
// to model "no credential available" explicitly without writing a
// custom resolver.
func Static(c Credentials) CredentialResolver {
	return staticResolver{c: c}
}

type staticResolver struct {
	c Credentials
}

// Resolve returns the wrapped credential, ignoring ctx and u.
func (s staticResolver) Resolve(_ context.Context, _ *url.URL) (Credentials, error) {
	return s.c, nil
}

// ErrNetrcParse wraps every parse failure produced by [Netrc]. Callers
// can use [errors.Is] to distinguish parse errors from I/O errors.
var ErrNetrcParse = errors.New("httpt: malformed netrc")

// Netrc returns a [CredentialResolver] that reads `~/.netrc` (or the
// platform equivalent) on each [CredentialResolver.Resolve] call and
// returns a [Basic] credential for the entry whose `machine` matches
// `u.Host`. A `default` block matches any host as a fallthrough; the
// first match wins.
//
// The grammar honoured is the traditional one:
//
//	machine <host> login <user> password <pass>
//	default login <user> password <pass>
//
// Tokens are whitespace-separated and lines beginning with `#` are
// comments. The `account` field, `macdef` blocks, and quoted strings
// are deliberately not supported; this is a strict subset of curl's
// netrc grammar (see the curl manpage's NETRC FILE FORMAT section).
//
// If the file does not exist the resolver returns `(nil, nil)`. If the
// file exists but is malformed the resolver returns an error wrapping
// [ErrNetrcParse]. If the file (or a symlink leading to it) is readable
// or writable by group or world the resolver still uses it but emits a
// one-line warning prefixed with `httpt: warning:` to stderr, mirroring
// canonical curl's behaviour.
func Netrc() CredentialResolver {
	return newNetrcResolver(defaultNetrcPath(), nil)
}

// netrcResolver is the implementation behind [Netrc]. The path and warn
// fields are populated by [newNetrcResolver]; tests use that helper to
// inject a temp-dir path and capture the warning.
type netrcResolver struct {
	path string
	// warn receives the loose-permissions warning. nil falls back to
	// [os.Stderr].
	warn io.Writer
}

// newNetrcResolver builds a [netrcResolver] with an explicit path and
// an optional writer for warnings. It is unexported because the public
// surface is [Netrc]; tests in this package use it for hermeticity.
func newNetrcResolver(path string, warn io.Writer) *netrcResolver {
	return &netrcResolver{path: path, warn: warn}
}

// Resolve reads the netrc file and returns a [Basic] credential whose
// `login`/`password` match `u.Host`, or `(nil, nil)` if no entry
// matches.
func (n *netrcResolver) Resolve(_ context.Context, u *url.URL) (Credentials, error) {
	if n.path == "" {
		return nil, nil
	}

	info, err := os.Lstat(n.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("httpt: stat netrc: %w", err)
	}
	n.maybeWarnLoosePermissions(info)

	data, err := os.ReadFile(n.path)
	if err != nil {
		return nil, fmt.Errorf("httpt: read netrc: %w", err)
	}

	entry, err := parseNetrc(data, u.Host)
	if err != nil {
		return nil, err
	}
	if entry == nil {
		return nil, nil
	}
	return Basic(entry.login, entry.password), nil
}

// maybeWarnLoosePermissions emits a stderr warning when the netrc
// file is reachable in a way that lets group or world either read
// or modify it. The check covers world/group read AND write because
// a world-writable netrc is more dangerous than a world-readable
// one: any local user can substitute credentials. The mode is read
// via `os.Lstat` so a world-writable symlink pointing at a
// mode-0600 target still trips the check — the link is the surface
// an attacker controls. Windows is skipped: POSIX mode bits are not
// meaningful there.
func (n *netrcResolver) maybeWarnLoosePermissions(info os.FileInfo) {
	if runtime.GOOS == "windows" {
		return
	}
	mode := info.Mode().Perm()
	if mode&0o077 == 0 {
		return
	}
	w := n.warn
	if w == nil {
		w = os.Stderr
	}
	_, _ = fmt.Fprintf(w, "httpt: warning: netrc file %q is readable or writable by group or world (mode %#o)\n", n.path, mode)
}

// netrcEntry holds the credential extracted from a matched block.
type netrcEntry struct {
	login    string
	password string
}

// parseNetrc walks data token-by-token and returns the first entry
// matching host (or the `default` block if no machine matches). It
// returns `(nil, nil)` when no entry matches.
func parseNetrc(data []byte, host string) (*netrcEntry, error) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var (
		current   *netrcEntry
		matchHost bool
		isDefault bool
		matched   *netrcEntry
		fallback  *netrcEntry
	)

	flush := func() {
		if current == nil {
			return
		}
		switch {
		case matchHost && matched == nil:
			matched = current
		case isDefault && fallback == nil:
			fallback = current
		}
		current = nil
		matchHost = false
		isDefault = false
	}

	for scanner.Scan() {
		line := scanner.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)
		i := 0
		for i < len(fields) {
			tok := fields[i]
			switch tok {
			case "machine":
				flush()
				if i+1 >= len(fields) {
					return nil, fmt.Errorf("%w: `machine` without host", ErrNetrcParse)
				}
				h := fields[i+1]
				current = &netrcEntry{}
				matchHost = h == host
				i += 2
			case "default":
				flush()
				current = &netrcEntry{}
				isDefault = true
				i++
			case "login":
				if current == nil {
					return nil, fmt.Errorf("%w: `login` outside machine/default block", ErrNetrcParse)
				}
				if i+1 >= len(fields) {
					return nil, fmt.Errorf("%w: `login` without value", ErrNetrcParse)
				}
				current.login = fields[i+1]
				i += 2
			case "password":
				if current == nil {
					return nil, fmt.Errorf("%w: `password` outside machine/default block", ErrNetrcParse)
				}
				if i+1 >= len(fields) {
					return nil, fmt.Errorf("%w: `password` without value", ErrNetrcParse)
				}
				current.password = fields[i+1]
				i += 2
			default:
				// Strict grammar: anything that is not a recognised
				// token is an error. The traditional `account` field
				// and `macdef` blocks fall through here intentionally;
				// callers needing them must use a richer resolver.
				return nil, fmt.Errorf("%w: unknown token %q", ErrNetrcParse, tok)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("httpt: scan netrc: %w", err)
	}
	flush()

	if matched != nil {
		return matched, nil
	}
	if fallback != nil {
		return fallback, nil
	}
	return nil, nil
}

// defaultNetrcPath returns the location of the user's netrc file, or ""
// if no home directory can be determined. On Windows curl looks for
// `_netrc` instead of `.netrc`; we honour that convention.
func defaultNetrcPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	name := ".netrc"
	if runtime.GOOS == "windows" {
		name = "_netrc"
	}
	return filepath.Join(home, name)
}

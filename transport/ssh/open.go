package ssht

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/hiddeco/go-ls-remote/internal/wire"
	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/transport"
)

// ErrMissingHostKey is returned when neither [WithKnownHosts] nor a
// `HostKeyCallback` on the supplied [WithClientConfig] template is
// set. Failing fast at dial time produces a clearer diagnostic than
// the mid-handshake panic x/crypto/ssh would otherwise raise. It is
// exported so callers can branch on [errors.Is]; it stays a plain
// `errors.New` rather than bridging through [transport.SchemeError]
// because configuration errors are a different category from the
// server-side conditions the generic transport sentinels cover.
var ErrMissingHostKey = errors.New(
	"ssht: HostKeyCallback required (use WithKnownHosts or set HostKeyCallback on WithClientConfig)")

// Open performs the dial and SSH handshake, exec's `git-upload-pack`
// against the remote path, and returns a [Conn] whose advertisement
// reader is positioned at the first server byte. Nothing is written
// to the session's stdin before the caller issues a [Conn.Command].
//
// # Version negotiation
//
// The requested protocol version is signalled to the remote via the
// SSH `env` channel alone — `GIT_PROTOCOL=version=<N>`, the same
// shape canonical Git emits at [connect.c:1311-1321]
// (`push_ssh_options` adds `SendEnv=GIT_PROTOCOL` to the `ssh`
// command line). The env channel is best-effort: a server whose
// `AcceptEnv` directive does not list `GIT_PROTOCOL` returns failure
// on the request and `Setenv` errors silently. `upload-pack` then
// runs without the hint and falls back to a v0 advertisement, which
// the wire layer parses transparently.
//
// The "extra parameters" pkt-line — `git-upload-pack <path>\0host=…\0\0version=<N>\0`
// — is NOT sent on the SSH transport. That mechanism is scoped to
// the git-daemon transport: canonical Git's SSH branch at
// [connect.c:1484-1508] never routes through `git_connect_git` (the
// only function that emits the pkt-line, via `packet_write` at
// [connect.c:1300]). [gitprotocol-pack.adoc §"SSH Transport"] mirrors
// that scoping; it documents env-channel negotiation only and makes
// no mention of an in-band fallback. Empirically, every forge that
// interposes a shim between sshd and `upload-pack` (Gitea's
// `gitea serv`, GitLab Shell, Gerrit) reads only argv and proxies
// stdin raw, so writing the pkt-line closes the channel before the
// first command frame.
//
// # ClientConfig merge rule
//
// The [Transport]'s configured [ssh.ClientConfig] template (if any)
// is shallow-copied for each dial, then the [WithAuth] and
// [WithKnownHosts] options fill in the `Auth` and `HostKeyCallback`
// fields when (and only when) they are nil on the template. The
// caller-precedence rule is documented on [WithClientConfig].
//
// # Context handling
//
// `ctx` is honoured at three points: an up-front `ctx.Err()` check,
// the TCP dial via `(*net.Dialer).DialContext`, and the SSH
// handshake. `ssh.NewClientConn` itself does not accept a context,
// so a watchdog goroutine closes the underlying TCP conn on
// `<-ctx.Done()`; that closure causes the in-progress handshake to
// return a net-closed error and the function returns `ctx.Err()` in
// preference. Once the handshake commits, cancellation no longer
// applies — subsequent session setup runs synchronously without
// further context plumbing.
//
// # Error mapping
//
// SSH publickey rejection surfaces as `*ProtocolError` wrapping
// [ErrAuthFailed]. TCP dial failures surface as `*ProtocolError` with
// the raw network error wrapped (no sentinel). Context cancellation
// surfaces directly. Configuration errors (missing HostKeyCallback)
// surface as `*ProtocolError` wrapping [ErrMissingHostKey].
//
// `ProtocolError.Op` discriminates the stage that failed: `"dial"`
// for the TCP dial and pre-dial config validation, `"handshake"` for
// the SSH transport-and-auth handshake, and `"session"` for session
// channel setup (env, pipe opens, exec).
//
// [connect.c:1300]: https://github.com/git/git/blob/v2.54.0/connect.c#L1300
// [connect.c:1311-1321]: https://github.com/git/git/blob/v2.54.0/connect.c#L1311-L1321
// [connect.c:1484-1508]: https://github.com/git/git/blob/v2.54.0/connect.c#L1484-L1508
// [gitprotocol-pack.adoc §"SSH Transport"]: https://github.com/git/git/blob/v2.54.0/Documentation/gitprotocol-pack.adoc#ssh-transport
func (t *Transport) Open(ctx context.Context, u *transport.URL, opts transport.OpenOptions) (_ transport.Conn, retErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	redacted := transport.RedactURL(u.Raw)

	user := sshUser(u)
	cfg, cleanup, err := resolveClientConfig(ctx, t, user, u.Host)
	if err != nil {
		return nil, &ProtocolError{URL: redacted, Op: "dial", Err: err}
	}
	defer func() {
		if cleanup != nil {
			_ = cleanup()
		}
	}()

	addr := hostAddress(u)
	tcpConn, err := dialTCP(ctx, t.dialer, addr)
	if err != nil {
		return nil, mapDialError(err, redacted)
	}

	// Watchdog goroutine: x/crypto/ssh's `NewClientConn` does not accept
	// a context, so the only way to unblock a hung handshake is to close
	// the underlying net.Conn from a sibling goroutine. The pattern is
	// the same one `net.Dialer.DialContext` uses internally for the
	// connect operation. The closure-on-success race is benign: if the
	// handshake committed before ctx fired, closing `tcpConn` tears down
	// the freshly built SSH conn — which is what a cancelled caller
	// asked for.
	handshakeDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = tcpConn.Close()
		case <-handshakeDone:
		}
	}()
	sshConn, chans, reqs, err := ssh.NewClientConn(tcpConn, addr, cfg)
	close(handshakeDone)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, mapHandshakeError(err, redacted)
	}
	client := ssh.NewClient(sshConn, chans, reqs)

	// From this point on, every failure path needs to tear down the
	// SSH client (and the session, once it exists). A single deferred
	// error-only cleanup replaces the per-return manual ladder. Setting
	// `session` after each successful step keeps the closer accurate.
	var session *ssh.Session
	defer func() {
		if retErr == nil {
			return
		}
		if session != nil {
			_ = session.Close()
		}
		_ = client.Close()
	}()

	session, err = client.NewSession()
	if err != nil {
		return nil, &ProtocolError{URL: redacted, Op: "session", Err: fmt.Errorf("ssht: open session: %w", err)}
	}

	// Best-effort env channel. A server whose `AcceptEnv` does not list
	// `GIT_PROTOCOL` rejects the request and `Setenv` returns
	// non-nil. The transport swallows it silently because env
	// rejection is normal on restrictive servers, and the wire layer
	// transparently parses whichever advertisement shape (v0 or v2)
	// the unaffected `upload-pack` then emits.
	//
	// Canonical Git's analog is `push_ssh_options` at
	// [connect.c:1311-1321], which appends `SendEnv=GIT_PROTOCOL` to the
	// spawned `ssh` command.
	//
	// [connect.c:1311-1321]: https://github.com/git/git/blob/v2.54.0/connect.c#L1311-L1321
	_ = session.Setenv("GIT_PROTOCOL", wire.HTTPProtocolHeader(opts.PreferredProtocol))

	stdout, err := session.StdoutPipe()
	if err != nil {
		return nil, &ProtocolError{URL: redacted, Op: "session", Err: fmt.Errorf("ssht: open stdout: %w", err)}
	}
	stdin, err := session.StdinPipe()
	if err != nil {
		return nil, &ProtocolError{URL: redacted, Op: "session", Err: fmt.Errorf("ssht: open stdin: %w", err)}
	}
	// Stderr is discarded for this iteration; see [Conn]'s lifecycle
	// note for the rationale. `session.Close()` (in either the cleanup
	// defer above or the [Conn.Close] cascade) closes the underlying
	// channel, returning EOF on the next read and ending this goroutine.
	stderr, err := session.StderrPipe()
	if err != nil {
		return nil, &ProtocolError{URL: redacted, Op: "session", Err: fmt.Errorf("ssht: open stderr: %w", err)}
	}
	go func() { _, _ = io.Copy(io.Discard, stderr) }()

	cmd := "git-upload-pack " + shellQuote(u.Path)
	if err := session.Start(cmd); err != nil {
		return nil, &ProtocolError{URL: redacted, Op: "session", Err: fmt.Errorf("ssht: exec: %w", err)}
	}

	c := &Conn{
		client:      client,
		session:     session,
		reader:      pktline.NewReader(stdout, inboundReaderOpts(opts.Tracer, redacted)...),
		writer:      pktline.NewWriter(stdin, outboundWriterOpts(opts.Tracer, redacted)...),
		redactedURL: redacted,
		done:        make(chan struct{}),
	}

	// Reap the remote `git-upload-pack` in a goroutine so its exit
	// status is observable through [Conn.sessionError]. `session.Wait`
	// must be called exactly once after `session.Start`; the closure
	// here is that single call. Closing `done` releases [Conn.Close]'s
	// wait and signals every reader of `c.waitErr`.
	go func() {
		defer close(c.done)
		c.waitErr = session.Wait()
	}()

	return c, nil
}

// resolveClientConfig merges the [Transport]'s configured client config
// template with the per-dial auth/host-key resolution. The merge
// follows the caller-precedence rule documented on [WithClientConfig]:
// a non-nil field on the template wins; a nil field is filled from the
// matching `With*` option.
//
// The returned cleanup hook releases resources held by the [AuthResolver]
// (e.g. the ssh-agent socket). It is non-nil only when an auth
// resolution actually ran; callers invoke it exactly once after the
// SSH handshake has completed (success or failure).
func resolveClientConfig(ctx context.Context, t *Transport, user, host string) (*ssh.ClientConfig, func() error, error) {
	var cfg ssh.ClientConfig
	if t.clientCfg != nil {
		cfg = *t.clientCfg
	}
	if cfg.User == "" {
		cfg.User = user
	}

	var cleanup func() error
	if cfg.Auth == nil && t.auth != nil {
		methods, c, err := t.auth.Resolve(ctx, host)
		if err != nil {
			return nil, nil, fmt.Errorf("ssht: resolve auth: %w", err)
		}
		cfg.Auth = methods
		cleanup = c
	}

	if cfg.HostKeyCallback == nil {
		cfg.HostKeyCallback = t.hostKey
	}
	if cfg.HostKeyCallback == nil {
		// Release any resources we just acquired before returning.
		if cleanup != nil {
			_ = cleanup()
		}
		return nil, nil, ErrMissingHostKey
	}

	return &cfg, cleanup, nil
}

// sshUser extracts the SSH user from u, stripping any password
// component (SSH never sends it as such). Empty `u.User` falls back to
// the empty string; callers relying on a runtime default supply it via
// [WithClientConfig].
func sshUser(u *transport.URL) string {
	if u.User == "" {
		return ""
	}
	if i := strings.Index(u.User, ":"); i >= 0 {
		return u.User[:i]
	}
	return u.User
}

// hostAddress assembles the `host:port` string for the TCP dial. The
// default port is `22`. IPv6 literals are bracketed so the host/port
// split is unambiguous; the bracketing assumes `u.Host` is unbracketed,
// which is the [transport.URL] invariant established by `ParseURL`.
func hostAddress(u *transport.URL) string {
	port := u.Port
	if port == "" {
		port = "22"
	}
	host := u.Host
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return host + ":" + port
}

// shellQuote returns s wrapped in single quotes with embedded single
// quotes and exclamation marks escaped using the POSIX
// close-escape-reopen idiom:
//
//	'  becomes  '\''
//	!  becomes  '\!'
//
// It is a direct port of canonical Git's [quote.c::sq_quote_buf] and
// matches the wire bytes [connect.c::git_proxy_connect] and
// [connect.c:1476] produce. The bang escape keeps the result safe when
// re-evaluated under csh-derived shells that perform history
// expansion. The output is safe to pass as a single argument through
// any POSIX shell and round-trips through `git-shell`'s
// `sq_dequote_to_argv`.
//
// [quote.c::sq_quote_buf]: https://github.com/git/git/blob/v2.54.0/quote.c#L28
// [connect.c::git_proxy_connect]: https://github.com/git/git/blob/v2.54.0/connect.c#L1038
// [connect.c:1476]: https://github.com/git/git/blob/v2.54.0/connect.c#L1476
func shellQuote(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('\'')
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\'' || c == '!' {
			b.WriteString(`'\`)
			b.WriteByte(c)
			b.WriteByte('\'')
			continue
		}
		b.WriteByte(c)
	}
	b.WriteByte('\'')
	return b.String()
}

// dialTCP issues the TCP dial via the supplied dialer. A nil dialer
// resolves to a zero-value [net.Dialer] so callers can pass nil through
// without an extra branch.
func dialTCP(ctx context.Context, dialer *net.Dialer, addr string) (net.Conn, error) {
	if dialer == nil {
		dialer = &net.Dialer{}
	}
	return dialer.DialContext(ctx, "tcp", addr)
}

// mapDialError maps a TCP dial failure to a `*ProtocolError`. No
// sentinel is attached: a refused or unreachable address is a generic
// network failure, not an authentication or repository-not-found
// condition.
func mapDialError(err error, redacted string) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return &ProtocolError{URL: redacted, Op: "dial", Err: err}
}

// mapHandshakeError maps an `ssh.NewClientConn` failure to a
// `*ProtocolError`. The auth-failure heuristic is a single-substring
// match against the canonical error text x/crypto/ssh emits at
// `client_auth.go:118`:
//
//	fmt.Errorf("ssh: unable to authenticate, attempted methods %v, no supported methods remain", tried)
//
// Both substrings the original alternation tested ("unable to
// authenticate" and "no supported methods remain") come from this same
// `fmt.Errorf`, so the alternation was redundant. We keep the more
// distinctive trailing fragment.
//
// HACK: the substring match is brittle. x/crypto/ssh exposes
// `*ssh.PartialSuccessError` on the server side only; on the client
// side `NewClientConn` returns `fmt.Errorf("ssh: handshake failed: %w", err)`
// wrapping the plain error built at `client_auth.go:118`. A typed
// client error would let us drop the substring match; until then this
// is the only signal short of failing every handshake error into the
// `ErrAuthFailed` bucket (which would over-claim badly).
func mapHandshakeError(err error, redacted string) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	msg := err.Error()
	if strings.Contains(msg, "no supported methods remain") {
		return &ProtocolError{URL: redacted, Op: "handshake", Server: msg, Err: ErrAuthFailed}
	}
	return &ProtocolError{URL: redacted, Op: "handshake", Err: err}
}

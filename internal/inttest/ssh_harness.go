package inttest

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
	"github.com/hiddeco/go-ls-remote/internal/objstore"
	"github.com/hiddeco/go-ls-remote/internal/server"
	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/transport"
)

// sshRepoMount is the fixed POSIX-style path [NewSSHServer] pretends to
// host. Matching the HTTP harness's `httpRepoMount` keeps the
// cross-transport assertions in [Entries] readable: every harness
// returns a URL whose last path component is `/repo.git`.
const sshRepoMount = "/repo.git"

// SSHServer is an in-process SSH upload-pack server backed by an
// `internal/objstore.Store`. It is constructed via [NewSSHServer] and
// exists alongside [NewHTTPServer]; SSH carries crypto state (host key
// and a pubkey callback) that callers must wire into their client, so
// the harness exposes a struct rather than a bare URL.
//
// The zero value is not usable: every method returns state set up by
// [NewSSHServer], and the cleanup that releases the listener is
// registered with the `testing.TB` passed in. Callers always go
// through [NewSSHServer].
type SSHServer struct {
	// listener owns the TCP socket bound to a 127.0.0.1 ephemeral
	// port. Closing it breaks the accept loop.
	listener net.Listener

	// hostSigner is the ed25519 host key generated at construction
	// time. The matching [ssh.PublicKey] is returned by [SSHServer.HostKey]
	// so callers can pin verification through
	// [transport/ssh.WithKnownHosts].
	hostSigner ssh.Signer

	// url is the parseable `ssh://git@host:port/repo.git` URL the
	// harness exports. It is materialised once at construction so the
	// hot path through [SSHServer.URL] never re-formats the address.
	url string

	// serve drives one `internal/server.Serve` invocation against the
	// supplied pkt-line halves. A closure carries the typed store so
	// the [SSHServer] type itself need not be generic — the type
	// parameter survives only in the constructor and the closure body,
	// which is the minimum needed.
	serve func(ctx context.Context, r *pktline.Reader, w *pktline.Writer) error

	// wg tracks the accept loop and every spawned per-connection
	// goroutine so the registered cleanup can `Wait` for them.
	wg sync.WaitGroup
}

// NewSSHServer returns an [*SSHServer] listening on a loopback ephemeral
// port. The server services exactly the `git-upload-pack '<path>'`
// command the production SSH transport issues, peels the
// extra-parameters pkt-line off the session stream, and runs
// [internal/server.Serve] against store for the remainder of the
// channel. The listener is registered for [testing.TB.Cleanup]; callers
// neither defer nor close anything themselves.
//
// The harness diverges from a bare-`url string` return shape (the form
// [NewHTTPServer] uses) because SSH negotiation has caller-side crypto
// state. The client needs the server's host key to verify the
// handshake, and the test code drives that via
// [transport/ssh.WithKnownHosts] or a direct
// `ssh.ClientConfig.HostKeyCallback`. Returning a typed handle keeps
// the host key accessible by name instead of through an out-of-band
// channel.
//
// # Auth and env policy
//
// The server accepts any presented publickey: the publickey callback
// returns `&ssh.Permissions{}, nil` regardless of the offered key.
// Tests that want to exercise auth-rejection paths build their own
// fixture (see `transport/ssh/testserver_test.go`); this harness is for
// the success path.
//
// Channel-level env requests are accepted unconditionally — the
// production transport sends `GIT_PROTOCOL=version=<N>` to signal the
// preferred version, and rejecting that request would emit a stray
// error on the client side without affecting the round trip. The
// server does not act on the env value; the equivalent signal arrives
// in-band on the initial pkt-line, which `internal/server.Serve`
// already understands.
//
// # Wire shape
//
// The session handler accepts one `exec` request whose command parses
// to `git-upload-pack` followed by a single-quoted POSIX-shell-escaped
// path (the form `transport/ssh` emits via `shellQuote`). After
// replying success, it peels the leading extra-parameters pkt-line
// the production client writes to stdin (see canonical Git's
// [connect.c:1288-1298] and [gitprotocol-pack.adoc §"Extra Parameters"])
// and then invokes [internal/server.Serve] with [transport.ProtocolV2]
// for `Options.PreferredProtocol`. SSH is not split like HTTP — one
// channel carries the full advertise-then-loop sequence — so
// `Serve` is the correct entry point rather than `ServeCommandLoop`.
//
// The generic parameter H is inferred from store; cross-fixture
// callers that iterate [Entries] type-switch on [Entry.ObjectFormat]
// once and call this function with the matching concrete store.
//
// [connect.c:1288-1298]: https://github.com/git/git/blob/v2.54.0/connect.c#L1288-L1298
// [gitprotocol-pack.adoc §"Extra Parameters"]: https://github.com/git/git/blob/v2.54.0/Documentation/gitprotocol-pack.adoc#extra-parameters
func NewSSHServer[H objfmt.Hash](t testing.TB, store *objstore.Store[H]) *SSHServer {
	t.Helper()

	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	require.NoError(t, err)

	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)

	host, port, err := net.SplitHostPort(ln.Addr().String())
	require.NoError(t, err)

	s := &SSHServer{
		listener:   ln,
		hostSigner: hostSigner,
		url:        fmt.Sprintf("ssh://git@%s:%s%s", host, port, sshRepoMount),
		serve: func(ctx context.Context, r *pktline.Reader, w *pktline.Writer) error {
			return server.Serve(ctx, r, w, store, server.Options{
				PreferredProtocol: transport.ProtocolV2,
			})
		},
	}

	s.wg.Add(1)
	go s.acceptLoop(t)

	t.Cleanup(func() {
		_ = ln.Close()
		s.wg.Wait()
	})

	return s
}

// URL returns the `ssh://git@host:port/repo.git` URL callers feed to
// [transport.ParseURL] (or to `transport/ssh.Transport.Open` via the
// parsed result). The path component is fixed at `/repo.git`, matching
// the HTTP harness's mount point so cross-transport assertions read
// one path string.
func (s *SSHServer) URL() string { return s.url }

// HostKey returns the server's ed25519 host key. Tests pin the
// handshake to it either through [SSHServer.HostKeyCallback] or by
// constructing their own [ssh.HostKeyCallback] off the returned key.
func (s *SSHServer) HostKey() ssh.PublicKey { return s.hostSigner.PublicKey() }

// HostKeyCallback returns an [ssh.HostKeyCallback] that pins the
// server's host key. It is the convenience wiring for
// `transport/ssh.WithKnownHosts(...)` and for tests that drive
// `golang.org/x/crypto/ssh` directly. A mismatch returns a plain
// `errors.New`; the SSH transport's handshake error mapping wraps the
// failure into its own `*ProtocolError` shape.
func (s *SSHServer) HostKeyCallback() ssh.HostKeyCallback {
	want := s.hostSigner.PublicKey().Marshal()
	return func(_ string, _ net.Addr, got ssh.PublicKey) error {
		if !bytes.Equal(want, got.Marshal()) {
			return errors.New("inttest: SSH host key mismatch")
		}
		return nil
	}
}

// acceptLoop accepts incoming TCP connections and dispatches each to a
// handler goroutine. It exits when [SSHServer.listener.Accept] returns
// an error — the registered cleanup closes the listener, which is the
// canonical termination signal.
func (s *SSHServer) acceptLoop(t testing.TB) {
	defer s.wg.Done()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go s.handle(t, conn)
	}
}

// handle performs the SSH server handshake on conn and services the
// resulting session channels. Auth is publickey-only and accepts any
// presented key — see the publickey-callback policy on [NewSSHServer].
func (s *SSHServer) handle(t testing.TB, conn net.Conn) {
	defer s.wg.Done()
	defer func() { _ = conn.Close() }()

	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, _ ssh.PublicKey) (*ssh.Permissions, error) {
			// Accept any pubkey. The harness exists so tests do not
			// have to pre-share authorised keys with a fixture; the
			// production SSH transport always offers a publickey
			// method, so this matches what the dial expects.
			return &ssh.Permissions{}, nil
		},
	}
	cfg.AddHostKey(s.hostSigner)

	sshConn, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		// A handshake failure is the expected shape when a test closes
		// the listener mid-dial; surfacing it would produce a noisy
		// failure on teardown.
		return
	}
	defer func() { _ = sshConn.Close() }()

	// Drain global out-of-band requests in a goroutine; x/crypto/ssh
	// deadlocks if no one reads from the channel.
	s.wg.Go(func() {
		ssh.DiscardRequests(reqs)
	})

	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			_ = newCh.Reject(ssh.UnknownChannelType, "inttest: only session channels are supported")
			continue
		}
		ch, chReqs, err := newCh.Accept()
		if err != nil {
			continue
		}
		s.wg.Add(1)
		go s.handleSession(t, ch, chReqs)
	}
}

// handleSession services env and exec channel requests. The first
// successful `exec` decommissions the request loop and hands the
// channel to [SSHServer.runUploadPack].
func (s *SSHServer) handleSession(t testing.TB, ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer s.wg.Done()

	type envPayload struct {
		Name  string
		Value string
	}
	type execPayload struct {
		Command string
	}

	var execCmd string
	execAccepted := false

	for req := range reqs {
		switch req.Type {
		case "env":
			// Accept any env request. The production transport sends
			// `GIT_PROTOCOL`; a stricter policy would reject everything
			// else, but the harness has no other env names to defend
			// against. Reply success unconditionally so the in-band
			// pkt-line path remains the single source of truth for
			// the protocol version (`Serve` reads it from there).
			var p envPayload
			_ = ssh.Unmarshal(req.Payload, &p)
			_ = req.Reply(true, nil)

		case "exec":
			var p execPayload
			if err := ssh.Unmarshal(req.Payload, &p); err != nil {
				_ = req.Reply(false, nil)
				continue
			}
			execCmd = p.Command
			_ = req.Reply(true, nil)
			execAccepted = true

		default:
			_ = req.Reply(false, nil)
		}

		if execAccepted {
			break
		}
	}

	if !execAccepted {
		_ = ch.Close()
		return
	}

	// Drain any further in-band requests so the SSH library does not
	// block its sender. The session itself is owned by the
	// upload-pack handler.
	s.wg.Go(func() {
		for req := range reqs {
			_ = req.Reply(false, nil)
		}
	})

	s.runUploadPack(t, ch, execCmd)
}

// runUploadPack parses execCmd as `git-upload-pack '<path>'`, peels the
// extra-parameters pkt-line off the channel reader, and invokes the
// stored `server.Serve` closure with [transport.ProtocolV2] for the
// preferred protocol. Stdin/stdout are the SSH channel; stderr is
// unused for this iteration.
//
// The peel mirrors canonical Git's `upload-pack` entry point: the
// client writes `git-upload-pack <path>\0host=<h>\0\0version=<N>\0` as
// the very first pkt-line on stdin (see [connect.c:1288-1298]), then
// switches into the v2 capability-advertise loop. Stripping that frame
// here lets `Serve` read a clean v2 stream from the next byte without
// having to know about the SSH/git-daemon entry shape.
//
// [connect.c:1288-1298]: https://github.com/git/git/blob/v2.54.0/connect.c#L1288-L1298
func (s *SSHServer) runUploadPack(t testing.TB, ch ssh.Channel, execCmd string) {
	defer func() { _ = ch.Close() }()

	if _, err := parseUploadPackCommand(execCmd); err != nil {
		// A garbled command surfaces on the test through the failing
		// advertisement read; report it through `t` so the diagnostic
		// names the harness rather than a downstream layer.
		t.Errorf("inttest.SSHServer: unrecognised exec command %q: %v", execCmd, err)
		return
	}

	pr := pktline.NewReader(ch)
	if _, err := pr.ReadPacket(); err != nil {
		// The client closed before sending the initial frame; nothing
		// to serve. This shape arises during dial-time teardown and is
		// not actionable, so it stays silent.
		return
	}

	if err := s.serve(t.Context(), pr, pktline.NewWriter(ch)); err != nil {
		if !isClientHangupError(err) {
			t.Errorf("inttest.SSHServer: server.Serve returned %v", err)
		}
		return
	}

	_ = ch.CloseWrite()
}

// isClientHangupError reports whether err is the expected shape when
// the client closes its end of the SSH channel before reading or
// writing the full session. The shapes are [io.EOF],
// [io.ErrUnexpectedEOF], [net.ErrClosed], [syscall.ECONNRESET], and
// [syscall.EPIPE]: x/crypto/ssh and the underlying TCP socket each
// surface a different one depending on whether the peer closes
// cleanly, mid-read, or mid-write. On Windows the runtime adds
// `WSAECONNRESET`/`WSAECONNABORTED` to that set; see
// [isPlatformHangup]. Surfacing any through `t.Errorf` would turn
// well-formed teardown sequences (e.g. a test that dials, accepts
// the advertisement up to the first capability, and closes) into
// spurious harness failures.
func isClientHangupError(err error) bool {
	return errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, context.Canceled) ||
		isPlatformHangup(err)
}

// parseUploadPackCommand validates execCmd as
// `git-upload-pack '<path>'` and returns the dequoted path. The
// production transport emits the path through `shellQuote` (a port of
// canonical Git's [quote.c::sq_quote_buf]); this helper reverses
// only the close-escape-reopen idiom that quoting produces. Commands
// that fail the prefix check or that hold an unterminated quote
// surface a non-nil error.
//
// The harness does not act on the parsed path beyond logging — the
// fixture binds one store per server, mounted at `sshRepoMount` — so
// the validation exists chiefly to fail loudly when a future client
// emits an unexpected shape.
//
// [quote.c::sq_quote_buf]: https://github.com/git/git/blob/v2.54.0/quote.c#L28
func parseUploadPackCommand(execCmd string) (string, error) {
	const prefix = "git-upload-pack "
	if !strings.HasPrefix(execCmd, prefix) {
		return "", fmt.Errorf("missing %q prefix", prefix)
	}
	arg := execCmd[len(prefix):]
	return sqDequote(arg)
}

// sqDequote reverses the single-quote escape canonical Git's
// `sq_quote_buf` produces: a leading and trailing single quote bracket
// the literal, and embedded single quotes round-trip through the
// `'\”` close-escape-reopen idiom (with `'\!'` doing the same for
// `!` so csh history expansion stays disabled). The function rejects
// strings without the bracketing quotes and strings whose escape
// sequence terminates prematurely.
//
// This is the inverse of `transport/ssh.shellQuote` (in turn ported
// from [quote.c:28] in canonical Git). The harness needs only the
// inverse direction; the production client owns the forward path.
//
// [quote.c:28]: https://github.com/git/git/blob/v2.54.0/quote.c#L28
func sqDequote(s string) (string, error) {
	if len(s) < 2 || s[0] != '\'' || s[len(s)-1] != '\'' {
		return "", fmt.Errorf("not a single-quoted literal: %q", s)
	}
	inner := s[1 : len(s)-1]
	var b strings.Builder
	b.Grow(len(inner))
	for i := 0; i < len(inner); i++ {
		c := inner[i]
		if c != '\'' {
			b.WriteByte(c)
			continue
		}
		// `'\''` close-escape-reopen: must be followed by `\`,
		// one escaped char, and a reopening `'`. Same shape for
		// `'\!'`.
		if i+3 >= len(inner) || inner[i+1] != '\\' || inner[i+3] != '\'' {
			return "", fmt.Errorf("malformed escape at offset %d", i)
		}
		b.WriteByte(inner[i+2])
		i += 3
	}
	return b.String(), nil
}

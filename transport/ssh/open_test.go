package ssht

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/transport"
)

// defaultOpenOptions returns the zero-value [transport.OpenOptions]
// used in this package's tests; declared as a function so future
// hooks (tracer, agent string) have a single edit site.
func defaultOpenOptions() transport.OpenOptions { return transport.OpenOptions{} }

// flushAdvertisement returns the bytes for a single `0000` flush packet,
// which the test server writes as its "advertisement" so the client's
// `Advertisement()` reader yields a deterministic EOF after one packet.
func flushAdvertisement() []byte { return []byte("0000") }

// TestOpen_v2_envAccepted exercises the happy path where the SSH
// server accepts the `GIT_PROTOCOL` env request. The transport must:
//
//   - emit `git-upload-pack '<path>'` via the SSH exec request, with
//     single-quoting per canonical Git's [connect.c:1476];
//   - send a `GIT_PROTOCOL=version=2` env request before the exec;
//   - write NOTHING to the session's stdin before the caller issues
//     a `Conn.Command`. Canonical Git's SSH branch at
//     [connect.c:1484-1508] never calls `git_connect_git` (whose
//     `packet_write` at [connect.c:1300] is the only place the
//     extra-parameters pkt-line is emitted), so SSH negotiation runs
//     exclusively over the env channel set up by `push_ssh_options`
//     at [connect.c:1311-1321].
//
// [connect.c:1476]: https://github.com/git/git/blob/v2.54.0/connect.c#L1476
// [connect.c:1484-1508]: https://github.com/git/git/blob/v2.54.0/connect.c#L1484-L1508
// [connect.c:1300]: https://github.com/git/git/blob/v2.54.0/connect.c#L1300
// [connect.c:1311-1321]: https://github.com/git/git/blob/v2.54.0/connect.c#L1311-L1321
func TestOpen_v2_envAccepted(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, testServerOpts{
		acceptEnv:     true,
		advertisement: flushAdvertisement(),
	})

	tr := New(
		WithAuth(Signer(srv.clientSigner)),
		WithKnownHosts(srv.hostKeyCallback()),
	)
	conn, err := tr.Open(t.Context(), srv.URL(), defaultOpenOptions())
	require.NoError(t, err)

	r := conn.Advertisement()
	require.NotNil(t, r)
	pkt, err := r.ReadPacket()
	require.NoError(t, err)
	assert.Equal(t, pktline.Flush, pkt.Kind)

	name, value, ok := srv.lastEnv()
	require.True(t, ok, "server should have received an env request before exec")
	assert.Equal(t, "GIT_PROTOCOL", name)
	assert.Equal(t, "version=2", value)

	cmd, ok := srv.lastExec()
	require.True(t, ok, "server should have received an exec request")
	assert.Equal(t, "git-upload-pack '/repo.git'", cmd,
		"exec command must single-quote the repository path per canonical Git's connect.c:1476")

	// Close before reading stdin: the fixture's drain goroutine reads
	// asynchronously, so the final `stdinBuf` snapshot is only stable
	// after the goroutine has hit EOF (which `Close` provokes).
	require.NoError(t, conn.Close())
	srv.waitStdinDrained(t)
	assert.Empty(t, srv.stdin(),
		"SSH transport must not write the extra-parameters pkt-line on stdin; "+
			"canonical Git restricts that mechanism to the git-daemon transport "+
			"(connect.c:1462 → git_connect_git → connect.c:1300)")
}

// TestOpen_v2_envRejected verifies env-channel rejection is benign:
// a server with restrictive `AcceptEnv` returns failure on the env
// request, Open still succeeds, and the transport writes nothing to
// stdin. The caller is left reading whatever advertisement the
// server emits — typically a v0 ref list when GIT_PROTOCOL did not
// reach `upload-pack`, which the wire layer parses transparently.
//
// This matches canonical Git's behaviour: `push_ssh_options` at
// [connect.c:1311-1321] sets `SendEnv=GIT_PROTOCOL` unconditionally,
// and a server that ignores the variable simply yields a v0
// advertisement. No stdin pkt-line fallback exists on the SSH path.
//
// [connect.c:1311-1321]: https://github.com/git/git/blob/v2.54.0/connect.c#L1311-L1321
func TestOpen_v2_envRejected(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, testServerOpts{
		acceptEnv:     false,
		advertisement: flushAdvertisement(),
	})

	tr := New(
		WithAuth(Signer(srv.clientSigner)),
		WithKnownHosts(srv.hostKeyCallback()),
	)
	conn, err := tr.Open(t.Context(), srv.URL(), defaultOpenOptions())
	require.NoError(t, err)

	r := conn.Advertisement()
	require.NotNil(t, r)
	pkt, err := r.ReadPacket()
	require.NoError(t, err)
	assert.Equal(t, pktline.Flush, pkt.Kind)

	cmd, ok := srv.lastExec()
	require.True(t, ok, "even with env rejected, the exec must still fire")
	assert.Equal(t, "git-upload-pack '/repo.git'", cmd)

	// The env-reject branch in the test fixture replies false BEFORE
	// invoking the onEnv hook, so the server never records the name
	// or value. Documenting the choice: this matches what x/crypto/ssh
	// does on the wire — a failed env request never reaches user code.
	_, _, recorded := srv.lastEnv()
	assert.False(t, recorded, "rejected env requests must never invoke the user callback")

	require.NoError(t, conn.Close())
	srv.waitStdinDrained(t)
	assert.Empty(t, srv.stdin(),
		"env rejection must not trigger an in-band pkt-line fallback: forge "+
			"shims (Gitea, GitLab Shell, Gerrit) that interpose between sshd "+
			"and `git-upload-pack` close the channel on any unexpected stdin")
}

// TestOpen_v0Pinned verifies that pinning v0 propagates through the
// env channel as `GIT_PROTOCOL=version=0`. The SSH transport carries
// the requested version on the env channel only; no stdin pkt-line is
// written, so v0 vs v2 is signalled to the server exclusively via the
// env variable (or, when the server ignores it, by the server's own
// default — which for a stock `git-upload-pack` is v0).
func TestOpen_v0Pinned(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, testServerOpts{
		acceptEnv:     true,
		advertisement: flushAdvertisement(),
	})

	v0 := transport.ProtocolV0
	tr := New(
		WithAuth(Signer(srv.clientSigner)),
		WithKnownHosts(srv.hostKeyCallback()),
	)
	conn, err := tr.Open(t.Context(), srv.URL(), transport.OpenOptions{PreferredProtocol: &v0})
	require.NoError(t, err)

	r := conn.Advertisement()
	_, _ = r.ReadPacket()

	name, value, ok := srv.lastEnv()
	require.True(t, ok)
	assert.Equal(t, "GIT_PROTOCOL", name)
	assert.Equal(t, "version=0", value, "v0 pin must propagate to the env channel")

	require.NoError(t, conn.Close())
	srv.waitStdinDrained(t)
	assert.Empty(t, srv.stdin(),
		"v0 pin must not write to stdin; the env channel is the only "+
			"version-signal route for the SSH transport")
}

// TestOpen_authFailure verifies an SSH publickey rejection surfaces as
// `*ProtocolError` wrapping [ErrAuthFailed] / [transport.ErrAuthFailed].
// The `Op` field is `"handshake"` — auth runs inside the SSH transport
// handshake, distinct from TCP-level dial failures (`"dial"`) and
// session-channel setup (`"session"`).
func TestOpen_authFailure(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, testServerOpts{
		acceptEnv:     true,
		advertisement: flushAdvertisement(),
		rejectClient:  true,
	})

	tr := New(
		WithAuth(Signer(srv.clientSigner)),
		WithKnownHosts(srv.hostKeyCallback()),
	)
	_, err := tr.Open(t.Context(), srv.URL(), defaultOpenOptions())
	require.Error(t, err)

	var pe *ProtocolError
	require.ErrorAs(t, err, &pe, "auth failure must surface as *ProtocolError")
	assert.Equal(t, "handshake", pe.Op)
	require.ErrorIs(t, err, ErrAuthFailed,
		"auth failure must be detectable via errors.Is(err, ssht.ErrAuthFailed)")
	assert.ErrorIs(t, err, transport.ErrAuthFailed,
		"auth failure must bridge to transport.ErrAuthFailed")
}

// TestOpen_dialFailure verifies a TCP-level failure surfaces as
// `*ProtocolError` wrapping the raw network error — without mapping to
// a sentinel.
func TestOpen_dialFailure(t *testing.T) {
	t.Parallel()

	// Reserve a port, close it, then dial: the address is well-formed
	// but the listener is gone, so the kernel refuses the connection.
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().(*net.TCPAddr)
	require.NoError(t, ln.Close())

	u := &transport.URL{
		Scheme: "ssh",
		User:   "git",
		Host:   addr.IP.String(),
		Port:   fmt.Sprintf("%d", addr.Port),
		Path:   "/repo.git",
		Raw:    fmt.Sprintf("ssh://git@%s:%d/repo.git", addr.IP, addr.Port),
	}

	tr := New(WithKnownHosts(ssh.InsecureIgnoreHostKey()))
	_, err = tr.Open(t.Context(), u, defaultOpenOptions())
	require.Error(t, err)

	var pe *ProtocolError
	require.ErrorAs(t, err, &pe, "dial failure must surface as *ProtocolError")
	assert.Equal(t, "dial", pe.Op)
	require.NotErrorIs(t, err, transport.ErrAuthFailed,
		"TCP dial failures must not masquerade as auth failures")
	assert.NotErrorIs(t, err, transport.ErrAuthRequired,
		"TCP dial failures must not masquerade as auth-required")
}

// TestOpen_contextCancelled verifies that a context cancelled before
// Open is invoked surfaces the cancellation error directly.
func TestOpen_contextCancelled(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, testServerOpts{acceptEnv: true, advertisement: flushAdvertisement()})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	tr := New(
		WithAuth(Signer(srv.clientSigner)),
		WithKnownHosts(srv.hostKeyCallback()),
	)
	_, err := tr.Open(ctx, srv.URL(), defaultOpenOptions())
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled,
		"a cancelled context must surface ctx.Err() through Open")
}

// TestOpen_missingHostKey verifies that when neither
// [WithKnownHosts] nor a `HostKeyCallback` on a supplied
// [WithClientConfig] template is set, Open fails fast with a
// configuration error rather than letting x/crypto/ssh fail mid-
// handshake on an empty callback. The wrapped sentinel
// [ErrMissingHostKey] is exported so callers can branch on
// [errors.Is].
func TestOpen_missingHostKey(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, testServerOpts{acceptEnv: true, advertisement: flushAdvertisement()})

	tr := New(WithAuth(Signer(srv.clientSigner)))
	_, err := tr.Open(t.Context(), srv.URL(), defaultOpenOptions())
	require.Error(t, err)

	var pe *ProtocolError
	require.ErrorAs(t, err, &pe,
		"missing-host-key configuration must surface as *ProtocolError")
	assert.Equal(t, "dial", pe.Op,
		"config errors observed during pre-dial validation carry Op=\"dial\"")
	require.ErrorIs(t, err, ErrMissingHostKey,
		"missing-host-key must be detectable via errors.Is(err, ssht.ErrMissingHostKey)")
	assert.Contains(t, err.Error(), "HostKeyCallback",
		"diagnostic must name HostKeyCallback for grep-friendly logs")
}

// TestOpen_handshakeContextCancelled verifies that cancelling the
// context after the TCP dial has completed but during the SSH
// handshake unblocks the handshake via the watchdog goroutine and
// surfaces `ctx.Err()`. The fixture accepts the TCP connection and
// then hangs without writing the SSH banner, which is the canonical
// way to reproduce a hung handshake; the watchdog closes the conn
// from underneath `ssh.NewClientConn` and the caller sees
// `context.Canceled`.
func TestOpen_handshakeContextCancelled(t *testing.T) {
	t.Parallel()

	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	// Accept goroutine: park the conn open without writing anything.
	// `goleak` requires we close every accepted conn, so track them
	// and close in cleanup.
	var (
		mu    sync.Mutex
		conns []net.Conn
		done  = make(chan struct{})
	)
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		<-done
		mu.Lock()
		for _, c := range conns {
			_ = c.Close()
		}
		mu.Unlock()
	})

	addr := ln.Addr().(*net.TCPAddr)
	u := &transport.URL{
		Scheme: "ssh",
		User:   "git",
		Host:   addr.IP.String(),
		Port:   fmt.Sprintf("%d", addr.Port),
		Path:   "/repo.git",
		Raw:    fmt.Sprintf("ssh://git@%s:%d/repo.git", addr.IP, addr.Port),
	}

	ctx, cancel := context.WithCancel(t.Context())
	// Cancel shortly after Open enters the SSH handshake. The exact
	// delay is not load-bearing — any value short of the test timeout
	// works because the handshake never completes against a silent
	// peer.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	tr := New(WithKnownHosts(ssh.InsecureIgnoreHostKey()))
	_, err = tr.Open(ctx, u, defaultOpenOptions())
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled,
		"cancelling mid-handshake must surface ctx.Err() rather than the net-closed error the watchdog synthesised; got %v", err)
}

// TestOpen_pathQuoting verifies the remote exec command quotes the
// repository path through `shellQuote` — a port of canonical Git's
// `sq_quote_buf` — so paths containing `'` or `!` round-trip through
// any POSIX shell and through `git-shell`'s `sq_dequote_to_argv`.
// The test asserts the wire bytes the server captures on the `exec`
// request, which is the only observable artefact for this contract.
func TestOpen_pathQuoting(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		path    string
		wantCmd string
	}{
		{
			name:    "plain path",
			path:    "/repo.git",
			wantCmd: "git-upload-pack '/repo.git'",
		},
		{
			name:    "single quote injection payload",
			path:    "/repo.git'; touch /tmp/x ; #",
			wantCmd: `git-upload-pack '/repo.git'\''; touch /tmp/x ; #'`,
		},
		{
			name:    "bang escaped for csh history expansion",
			path:    "/repo!.git",
			wantCmd: `git-upload-pack '/repo'\!'.git'`,
		},
		{
			name:    "mixed quote and bang",
			path:    "/a'b!c",
			wantCmd: `git-upload-pack '/a'\''b'\!'c'`,
		},
		{
			name:    "inert metacharacters pass through unchanged",
			path:    "/a\\b\"c$d`e",
			wantCmd: "git-upload-pack '/a\\b\"c$d`e'",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := newTestServer(t, testServerOpts{
				acceptEnv:     true,
				advertisement: flushAdvertisement(),
			})

			u := srv.URL()
			u.Path = tc.path
			u.Raw = fmt.Sprintf("ssh://git@%s:%s%s", u.Host, u.Port, tc.path)

			tr := New(
				WithAuth(Signer(srv.clientSigner)),
				WithKnownHosts(srv.hostKeyCallback()),
			)
			conn, err := tr.Open(t.Context(), u, defaultOpenOptions())
			require.NoError(t, err)
			t.Cleanup(func() { _ = conn.Close() })

			// Drain the advertisement so the exec capture flushes.
			_, _ = conn.Advertisement().ReadPacket()

			got, ok := srv.lastExec()
			require.True(t, ok, "server should have received an exec request")
			assert.Equal(t, tc.wantCmd, got,
				"exec command must be `sq_quote_buf`-encoded per canonical Git's connect.c:1313")
		})
	}
}

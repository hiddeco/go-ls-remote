package ssht

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/transport"
)

// bridgeServer is the algo-polymorphic seam between the SSH test
// fixture and an in-process `internal/server.Serve` instance. A
// closure carries the typed [github.com/hiddeco/go-ls-remote/internal/objstore.Store]
// internally so the fixture does not need to know which hash algo
// it is serving — the helper that builds the bridge picks the type
// parameter and captures the store in `serve`. Tests pair this with
// a `bridgeSHA1Store` or analogous helper.
type bridgeServer struct {
	// serve drives one `server.Serve` invocation against the supplied
	// pkt-line reader and writer. The fixture wraps the SSH channel's
	// post-extra-parameters byte stream in those pkt-line halves.
	serve func(ctx context.Context, r *pktline.Reader, w *pktline.Writer) error

	// tb receives any error `serve` returns. A non-nil `tb.Errorf`
	// call fails the test from the fixture goroutine — without it a
	// `server.Serve` failure would degrade the response shape silently
	// and the round-trip assertion would pass against a partial body.
	// `tb` is supplied by the per-test bridge factory and is the
	// running `*testing.T` (or, in `t.Run` sub-tests, the sub-test's
	// `*testing.T`).
	tb testing.TB
}

// testServerOpts configures the in-process SSH server fixture. Each
// field has a zero-valued default chosen so a bare `testServerOpts{}`
// produces a server that accepts a single connection, rejects every
// env request, replies success to one exec request, and serves an
// empty advertisement.
type testServerOpts struct {
	// acceptEnv, when true, makes the server reply success to `env`
	// channel requests and forward the parsed (name, value) pair to
	// the fixture's `onEnv` hook. When false, the server replies
	// failure to env requests BEFORE invoking the hook, so the
	// fixture's `lastEnv` reports no record. The choice mirrors what
	// x/crypto/ssh's server-side code does on the wire: a rejected
	// request never reaches user code.
	acceptEnv bool

	// advertisement is the byte sequence the server writes to the
	// session's stdout channel after accepting the exec request. The
	// default (an empty slice) is permitted; tests that drain the
	// client-side reader usually pass at least a `0000` flush so the
	// reader yields a deterministic terminal packet.
	advertisement []byte

	// rejectClient, when true, makes the server's
	// `PublicKeyCallback` reject every offered key. This simulates an
	// unauthorised dial and exercises [Transport.Open]'s auth-failure
	// mapping path.
	rejectClient bool

	// serveStore, when non-nil, switches the fixture from the "write a
	// fixed `advertisement` byte slice" mode used by the dial-time
	// tests to a "bridge the SSH channel to `internal/server.Serve`"
	// mode used by the round-trip command tests. The callback is
	// invoked once per accepted session; it returns the typed bridge
	// to dispatch on. `advertisement` is ignored when `serveStore` is
	// set: the bridge owns every byte written to stdout.
	serveStore func() bridgeServer
}

// testServer is an in-process SSH server fixture. It listens on a
// dynamically allocated port on 127.0.0.1, accepts one or more
// incoming SSH connections, and records the exec command, env
// requests, and session stdin bytes the client sends. Tests use it
// to drive [Transport.Open] without depending on a real sshd.
//
// The fixture is single-flight per accepted connection: it accepts one
// session channel, services env/exec requests on it, and then drains
// stdin and writes `advertisement` to stdout. Subsequent channel
// opens are rejected with `ssh.UnknownChannelType`. The package's
// `goleak.VerifyTestMain` enforces zero leaked goroutines on teardown.
type testServer struct {
	// listener is the TCP listener. Closing it breaks the accept loop
	// in [testServer.serve].
	listener net.Listener

	// hostSigner is the server's host key (ed25519, in-memory).
	hostSigner ssh.Signer

	// clientSigner is the single pubkey the server accepts (unless
	// `rejectClient` is set). Tests wire this into the SSH transport
	// via [Signer].
	clientSigner ssh.Signer

	// opts records the configuration the fixture was built with.
	opts testServerOpts

	// wg tracks the accept loop and every per-connection goroutine so
	// teardown can `Wait` them to completion under
	// `goleak.VerifyTestMain`.
	wg sync.WaitGroup

	// stdinNotify is a buffered channel (capacity 1) the stdin-drain
	// goroutine sends to after each successful read. A buffer of 1
	// makes the send non-blocking even when no waiter is parked, so
	// the drain goroutine never stalls.
	stdinNotify chan struct{}

	// stdinDrained is closed by the stdin-drain goroutine when its
	// `ch.Read` loop returns (EOF, channel close, or error). A `<-`
	// receive on it is the synchronisation point a test uses to read
	// the final `stdinBuf` snapshot after `Conn.Close`. `sync.Once`
	// guards the close so the channel is safe for a fixture that
	// accepts more than one connection in its lifetime.
	stdinDrained     chan struct{}
	stdinDrainedOnce sync.Once

	// mu guards every field below.
	mu       sync.Mutex
	envName  string
	envValue string
	envSet   bool
	execCmd  string
	execSet  bool
	stdinBuf bytes.Buffer

	// nconns is the running tally of accepted connections, so a test
	// can detect retries or unexpected re-dials.
	nconns int
}

// newTestServer constructs and starts the fixture. It registers a
// `tb.Cleanup` that closes the listener, closes every accepted
// connection, and waits for every spawned goroutine to return —
// matching the package's `goleak.VerifyTestMain` shape. The signature
// is `testing.TB` rather than `*testing.T` so benchmarks can reuse
// the fixture without re-implementing it.
func newTestServer(tb testing.TB, opts testServerOpts) *testServer {
	tb.Helper()

	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(tb, err)
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	require.NoError(tb, err)

	_, clientPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(tb, err)
	clientSigner, err := ssh.NewSignerFromKey(clientPriv)
	require.NoError(tb, err)

	var lc net.ListenConfig
	ln, err := lc.Listen(tb.Context(), "tcp", "127.0.0.1:0")
	require.NoError(tb, err)

	s := &testServer{
		listener:     ln,
		hostSigner:   hostSigner,
		clientSigner: clientSigner,
		opts:         opts,
		stdinNotify:  make(chan struct{}, 1),
		stdinDrained: make(chan struct{}),
	}

	s.wg.Add(1)
	go s.acceptLoop()

	tb.Cleanup(func() {
		_ = ln.Close()
		s.wg.Wait()
	})

	return s
}

// URL returns the [*transport.URL] callers pass to [Transport.Open] to
// reach the fixture. The path component is fixed at `/repo.git` so
// tests have a single string to assert against.
func (s *testServer) URL() *transport.URL {
	host, port, err := net.SplitHostPort(s.listener.Addr().String())
	if err != nil {
		panic(fmt.Sprintf("testServer.URL: SplitHostPort: %v", err))
	}
	return &transport.URL{
		Scheme: "ssh",
		User:   "git",
		Host:   host,
		Port:   port,
		Path:   "/repo.git",
		Raw:    fmt.Sprintf("ssh://git@%s:%s/repo.git", host, port),
	}
}

// hostKeyCallback returns an [ssh.HostKeyCallback] that pins the
// fixture's host key. Tests wire this into the SSH transport via
// [WithKnownHosts] so the handshake validates the in-memory host key
// rather than reading from a `known_hosts` file.
func (s *testServer) hostKeyCallback() ssh.HostKeyCallback {
	want := s.hostSigner.PublicKey().Marshal()
	return func(_ string, _ net.Addr, got ssh.PublicKey) error {
		if !bytes.Equal(want, got.Marshal()) {
			return errors.New("testServer: host key mismatch")
		}
		return nil
	}
}

// lastEnv returns the most recently recorded env request name/value
// pair plus a bool indicating whether one was recorded.
func (s *testServer) lastEnv() (name, value string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.envName, s.envValue, s.envSet
}

// lastExec returns the most recently recorded exec command plus a
// bool indicating whether one was recorded.
func (s *testServer) lastExec() (cmd string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.execCmd, s.execSet
}

// stdin returns a copy of every byte the client wrote to the session
// channel's stdin so far. Copying is necessary because the underlying
// buffer is mutated as more bytes arrive.
func (s *testServer) stdin() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]byte, s.stdinBuf.Len())
	copy(out, s.stdinBuf.Bytes())
	return out
}

// waitStdinDrained blocks until the per-connection stdin-drain
// goroutine has returned, signalling that any bytes the client wrote
// before tearing down its session have been captured in `stdinBuf`.
// Callers invoke it AFTER `conn.Close()` to take the final snapshot
// of `stdin()` — without it, a `len(stdin()) == 0` assertion races
// the drain goroutine and passes even when bytes are in flight on
// the SSH channel.
//
// The deadline is the test's own context, so a fixture that never
// drains fails the test rather than hanging the runner.
func (s *testServer) waitStdinDrained(t *testing.T) {
	t.Helper()
	select {
	case <-s.stdinDrained:
	case <-t.Context().Done():
		t.Fatalf("testServer.waitStdinDrained: test context cancelled before drain completed (captured %d bytes)", len(s.stdin()))
	}
}

// acceptLoop accepts incoming TCP connections and dispatches each to a
// `handle` goroutine. It exits when [testServer.listener.Accept]
// returns an error — the cleanup closes the listener, which is the
// canonical termination signal.
func (s *testServer) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.nconns++
		s.mu.Unlock()
		s.wg.Add(1)
		go s.handle(conn)
	}
}

// handle performs the SSH handshake on conn and services the resulting
// channels. Auth is publickey-only and pinned to `clientSigner`
// (unless `rejectClient` is set). Any error along the way drops the
// connection silently — the client side sees a handshake failure or
// EOF, which the transport's tests check for.
func (s *testServer) handle(conn net.Conn) {
	defer s.wg.Done()
	defer func() { _ = conn.Close() }()

	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if s.opts.rejectClient {
				return nil, errors.New("testServer: client rejected by config")
			}
			if !bytes.Equal(key.Marshal(), s.clientSigner.PublicKey().Marshal()) {
				return nil, errors.New("testServer: unknown public key")
			}
			return &ssh.Permissions{}, nil
		},
	}
	cfg.AddHostKey(s.hostSigner)

	sshConn, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	defer func() { _ = sshConn.Close() }()

	// Drain global out-of-band requests in a goroutine; tests never
	// rely on them but x/crypto/ssh will deadlock if no one is
	// reading from the channel.
	s.wg.Go(func() {
		ssh.DiscardRequests(reqs)
	})

	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			_ = newCh.Reject(ssh.UnknownChannelType, "only session channels are supported by this fixture")
			continue
		}
		ch, chReqs, err := newCh.Accept()
		if err != nil {
			continue
		}
		s.wg.Add(1)
		go s.handleSession(ch, chReqs)
	}
}

// handleSession services env/exec channel requests and, on a
// successful exec, drains stdin into the fixture's buffer while
// writing `opts.advertisement` to stdout.
func (s *testServer) handleSession(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer s.wg.Done()

	type envPayload struct {
		Name  string
		Value string
	}
	type execPayload struct {
		Command string
	}

	execAccepted := false

	for req := range reqs {
		switch req.Type {
		case "env":
			if !s.opts.acceptEnv {
				// Rejected env requests never reach user code, so we
				// reply failure BEFORE recording anything. The test
				// asserts `lastEnv` returns ok=false on this path.
				_ = req.Reply(false, nil)
				continue
			}
			var p envPayload
			if err := ssh.Unmarshal(req.Payload, &p); err != nil {
				_ = req.Reply(false, nil)
				continue
			}
			s.mu.Lock()
			s.envName = p.Name
			s.envValue = p.Value
			s.envSet = true
			s.mu.Unlock()
			_ = req.Reply(true, nil)

		case "exec":
			var p execPayload
			if err := ssh.Unmarshal(req.Payload, &p); err != nil {
				_ = req.Reply(false, nil)
				continue
			}
			s.mu.Lock()
			s.execCmd = p.Command
			s.execSet = true
			s.mu.Unlock()
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

	// After accept: drain in-band requests so the goroutine that
	// produces them doesn't block. The session's request channel is
	// closed when the client closes its side; we keep consuming until
	// then.
	s.wg.Go(func() {
		for req := range reqs {
			_ = req.Reply(false, nil)
		}
	})

	if s.opts.serveStore != nil {
		s.runBridge(ch)
		return
	}

	// Write the advertisement to stdout, then drain stdin into the
	// fixture's buffer. Order matters: writing first lets the client's
	// `Advertisement()` reader unblock independently of how much it
	// sends on stdin.
	if len(s.opts.advertisement) > 0 {
		_, _ = ch.Write(s.opts.advertisement)
	}

	// Drain stdin in a goroutine; `ch.Read` returns when the channel
	// reader hits EOF (client closed its write half) or the channel
	// itself is closed by `ch.Close()` from this handler. After each
	// successful read, signal `stdinNotify` so any waiter wakes; the
	// non-blocking send (channel capacity 1) means the drain never
	// stalls when no waiter is parked. On exit, close `stdinDrained`
	// so a test that has called `conn.Close` can synchronise with the
	// drain and read the final `stdinBuf` snapshot.
	s.wg.Go(func() {
		defer s.stdinDrainedOnce.Do(func() { close(s.stdinDrained) })
		buf := make([]byte, 4096)
		for {
			n, err := ch.Read(buf)
			if n > 0 {
				s.mu.Lock()
				s.stdinBuf.Write(buf[:n])
				s.mu.Unlock()
				select {
				case s.stdinNotify <- struct{}{}:
				default:
				}
			}
			if err != nil {
				// Drop any read error including `io.EOF` and the
				// `ErrUnexpectedEOF` produced when the client's
				// `Conn.Close` races our `Write` — both are the
				// expected shutdown shapes for this fixture.
				return
			}
		}
	})
}

// runBridge wires the SSH channel's stdin/stdout into an in-process
// `internal/server.Serve` invocation. No frame is peeled off the
// inbound stream first: canonical Git's SSH branch (`connect.c:1484-1508`)
// never emits the extra-parameters pkt-line on stdin, and the
// transport mirrors that — version negotiation happens on the env
// channel only — so the bridge reads a v2 command stream from the
// first byte.
//
// The bridge is the round-trip-test counterpart to the fixed-byte
// `advertisement` path: it lets the client see real advertisement and
// command-response bytes generated by `Serve` against a typed
// object store, without depending on canonical Git.
func (s *testServer) runBridge(ch ssh.Channel) {
	bridge := s.opts.serveStore()

	srvErr := bridge.serve(context.Background(),
		pktline.NewReader(ch),
		pktline.NewWriter(ch),
	)
	if srvErr != nil && bridge.tb != nil {
		// Surface bridge-side `server.Serve` failures through the test
		// rather than dropping them on the floor. Without this, a
		// `Serve` error mid-test degrades the response shape silently
		// and the round-trip assertion passes against a partial body.
		bridge.tb.Errorf("testServer.runBridge: server.Serve returned %v", srvErr)
	}
	_ = ch.CloseWrite()
}

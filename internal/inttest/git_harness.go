package inttest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"syscall"
	"testing"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
	"github.com/hiddeco/go-ls-remote/internal/objstore"
	"github.com/hiddeco/go-ls-remote/internal/server"
	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/transport"
)

// gitRepoMount is the fixed path the harness pretends to host.
// Matching the HTTP and SSH harnesses (`httpRepoMount`,
// `sshRepoMount`) keeps cross-transport assertions in [Entries]
// readable: every harness returns a URL whose last path component is
// `/repo.git`.
const gitRepoMount = "/repo.git"

// NewGitServer returns the `git://host:port/repo.git` URL of an
// in-process git-daemon-style server. The server listens on a
// loopback ephemeral port, accepts an arbitrary number of concurrent
// connections, peels the discovery-time pkt-line off each connection
// (the server-side analog of canonical Git's [daemon.c::execute] at
// [daemon.c:749]), and runs [internal/server.Serve] against store
// for the remainder of the stream.
//
// The harness mirrors the [NewHTTPServer] return shape — a bare
// URL — because git-daemon has no caller-side state to expose. The
// listener is registered for [testing.TB.Cleanup]; callers neither
// defer nor close anything themselves.
//
// The generic parameter H is inferred from store; cross-fixture
// callers that iterate [Entries] type-switch on [Entry.ObjectFormat]
// once and call this function with the matching concrete store.
//
// # Wire shape
//
// The initial pkt-line follows the grammar from
// [gitprotocol-pack.adoc §"Extra Parameters"] and matches canonical
// Git's [daemon.c::execute] at [daemon.c:749]:
//
//	git-upload-pack <path> NUL host=<host[:port]> NUL [NUL version=<N> NUL]
//
// The harness validates the `git-upload-pack ` prefix and the
// presence of a `host=` field; the parsed values are discarded
// because the server only hosts one store. Malformed handshakes
// close the connection without invoking [internal/server.Serve].
//
// [daemon.c::execute]: https://github.com/git/git/blob/v2.54.0/daemon.c#L736
// [daemon.c:749]: https://github.com/git/git/blob/v2.54.0/daemon.c#L749
// [gitprotocol-pack.adoc §"Extra Parameters"]: https://github.com/git/git/blob/v2.54.0/Documentation/gitprotocol-pack.adoc#extra-parameters
func NewGitServer[H objfmt.Hash](t testing.TB, store *objstore.Store[H]) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("inttest.NewGitServer: listen: %v", err)
	}

	host, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		_ = ln.Close()
		t.Fatalf("inttest.NewGitServer: split host port %q: %v", ln.Addr().String(), err)
	}

	s := &gitServer{
		listener: ln,
		serve: func(ctx context.Context, r *pktline.Reader, w *pktline.Writer) error {
			return server.Serve(ctx, r, w, store, server.Options{
				PreferredProtocol: transport.ProtocolV2,
			})
		},
	}

	s.wg.Go(func() { s.acceptLoop(t) })

	t.Cleanup(func() {
		_ = ln.Close()
		s.wg.Wait()
	})

	return fmt.Sprintf("git://%s:%s%s", host, port, gitRepoMount)
}

// gitServer is the unexported state behind [NewGitServer]. Unlike
// [SSHServer] there is no caller-side handle: tests interact with
// the harness exclusively through the returned URL, so the type
// stays internal.
type gitServer struct {
	// listener owns the TCP socket bound to a 127.0.0.1 ephemeral
	// port. Closing it breaks the accept loop.
	listener net.Listener

	// serve drives one [internal/server.Serve] invocation against
	// the supplied pkt-line halves. A closure carries the typed
	// store so the [gitServer] type itself need not be generic —
	// the type parameter survives only in the constructor and the
	// closure body, which is the minimum needed.
	serve func(ctx context.Context, r *pktline.Reader, w *pktline.Writer) error

	// wg tracks the accept loop and every spawned per-connection
	// goroutine so the registered cleanup can `Wait` for them.
	wg sync.WaitGroup
}

// acceptLoop accepts incoming TCP connections and dispatches each to
// a handler goroutine. It exits when [gitServer.listener.Accept]
// returns an error — the registered cleanup closes the listener,
// which is the canonical termination signal.
func (s *gitServer) acceptLoop(t testing.TB) {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.wg.Go(func() { s.handle(t, conn) })
	}
}

// handle reads the discovery-time pkt-line off conn, validates it
// against the canonical `git-upload-pack <path>\0host=<h>\0` shape,
// and runs the stored serve closure for the remainder of the
// stream. Malformed handshakes close the connection without
// surfacing an error: the test that drives the malformed frame
// observes the close as its acceptance signal.
func (s *gitServer) handle(t testing.TB, conn net.Conn) {
	defer func() { _ = conn.Close() }()

	r := pktline.NewReader(conn)
	w := pktline.NewWriter(conn)

	// Read the initial discovery-time pkt-line — the server-side
	// analog of [daemon.c::execute] at [daemon.c:749]. A read error
	// here (EOF, net-closed) is the client closing before the
	// handshake completed, which is not actionable.
	//
	// [daemon.c::execute]: https://github.com/git/git/blob/v2.54.0/daemon.c#L736
	// [daemon.c:749]: https://github.com/git/git/blob/v2.54.0/daemon.c#L749
	pkt, err := r.ReadPacket()
	if err != nil {
		return
	}
	if pkt.Kind != pktline.Data {
		return
	}
	if err := validateGitHandshake(pkt.Data); err != nil {
		// A malformed pkt-line is a test bug or an external probe;
		// log on `t` so it surfaces if the integration tests ever
		// produce one accidentally.
		t.Logf("inttest.NewGitServer: malformed handshake %q: %v", pkt.Data, err)
		return
	}

	if err := s.serve(context.Background(), r, w); err != nil {
		if !isGitClientHangupError(err) {
			t.Errorf("inttest.NewGitServer: server.Serve returned %v", err)
		}
	}
}

// validateGitHandshake checks that payload satisfies the canonical
// `git-upload-pack <path>\0host=<h>\0[ \0version=<N>\0 ]` grammar
// from [gitprotocol-pack.adoc §"Extra Parameters"]. The parser
// mirrors [daemon.c::execute] at [daemon.c:752-758] and
// [daemon.c::parse_extra_args] at [daemon.c:623]: a leading
// command, a NUL-terminated path, then NUL-separated extended
// fields beginning with `host=`. The harness discards the parsed
// values; the function returns a non-nil error only when the
// payload deviates from the shape badly enough that
// [internal/server.Serve] would never produce a useful response.
//
// [gitprotocol-pack.adoc §"Extra Parameters"]: https://github.com/git/git/blob/v2.54.0/Documentation/gitprotocol-pack.adoc#extra-parameters
// [daemon.c::execute]: https://github.com/git/git/blob/v2.54.0/daemon.c#L736
// [daemon.c:752-758]: https://github.com/git/git/blob/v2.54.0/daemon.c#L752-L758
// [daemon.c::parse_extra_args]: https://github.com/git/git/blob/v2.54.0/daemon.c#L623
// [daemon.c:623]: https://github.com/git/git/blob/v2.54.0/daemon.c#L623
func validateGitHandshake(payload []byte) error {
	const prefix = "git-upload-pack "
	// pkt-line payloads frequently carry a trailing `\n`; canonical
	// Git's `execute` strips it before parsing (see [daemon.c:752]).
	//
	// [daemon.c:752]: https://github.com/git/git/blob/v2.54.0/daemon.c#L752
	payload = bytes.TrimSuffix(payload, []byte{'\n'})

	if !bytes.HasPrefix(payload, []byte(prefix)) {
		return fmt.Errorf("missing %q prefix", prefix)
	}
	rest := payload[len(prefix):]

	nul := bytes.IndexByte(rest, 0)
	if nul < 0 {
		return errors.New("missing NUL after path")
	}
	if nul == 0 {
		return errors.New("empty path")
	}
	rest = rest[nul+1:]

	// `parse_extra_args` ([daemon.c:623]) reads NUL-separated
	// arguments. The first one must be `host=<value>`; subsequent
	// entries are optional and live behind an additional NUL.
	//
	// [daemon.c:623]: https://github.com/git/git/blob/v2.54.0/daemon.c#L623
	hostField := rest
	if i := bytes.IndexByte(rest, 0); i >= 0 {
		hostField = rest[:i]
	}
	if !bytes.HasPrefix(hostField, []byte("host=")) {
		return fmt.Errorf("missing host= field; got %q", hostField)
	}

	return nil
}

// isGitClientHangupError reports whether err is the expected shape
// when the client closes its end of the TCP connection before the
// session completes. Four shapes arise on this path:
//
//   - [io.EOF] when the peer closes the connection cleanly between
//     pkt-lines, which Go's [net.Conn.Read] surfaces verbatim.
//   - [io.ErrUnexpectedEOF] when the close races a partial read of
//     a pkt-line frame.
//   - [net.ErrClosed] when our own `t.Cleanup` closes the listener
//     while a connection is still in flight, propagating to the
//     server-side read.
//   - [syscall.ECONNRESET] when the client tears the socket down
//     hard — `conn.Close` after a write that the peer never read.
//     Darwin and Linux both surface this as `read: connection
//     reset by peer` instead of EOF.
//
// Surfacing any of them through `t.Errorf` would turn well-formed
// teardown sequences into spurious harness failures.
func isGitClientHangupError(err error) bool {
	return errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, syscall.ECONNRESET)
}

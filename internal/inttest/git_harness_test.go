package inttest_test

import (
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hiddeco/go-ls-remote/internal/inttest"
	"github.com/hiddeco/go-ls-remote/internal/wire"
	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/transport"
	gitt "github.com/hiddeco/go-ls-remote/transport/git"
)

// dialGitHarness opens the production `transport/git` client against
// the harness URL. The helper isolates the URL-parse and dial
// boilerplate every git-daemon harness test repeats.
func dialGitHarness(t *testing.T, rawURL string) transport.Conn {
	t.Helper()

	tr := gitt.New(gitt.WithDialer(&net.Dialer{}))
	u, err := transport.ParseURL(rawURL)
	require.NoError(t, err)
	v := transport.ProtocolV2
	conn, err := tr.Open(t.Context(), u, transport.OpenOptions{
		PreferredProtocol: &v,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// TestNewGitServer_servesAdvertisement asserts that a client dialled
// through the production `transport/git` package sees the v2
// advertisement emitted by `internal/server.Serve`. The first packet
// must be `version 2\n`, and the advertisement must terminate in a
// flush.
func TestNewGitServer_servesAdvertisement(t *testing.T) {
	store := openLooseOnlySHA1Store(t)
	url := inttest.NewGitServer(t, store)
	conn := dialGitHarness(t, url)

	pr := conn.Advertisement()
	pkt, err := pr.ReadPacket()
	require.NoError(t, err)
	assert.Equal(t, pktline.Data, pkt.Kind)
	assert.Equal(t, "version 2\n", string(pkt.Data),
		"v2 advertisement must begin with `version 2\\n`")

	for {
		pkt, err := pr.ReadPacket()
		require.NoError(t, err)
		if pkt.Kind == pktline.Flush {
			return
		}
	}
}

// TestNewGitServer_handlesV2Command asserts that the harness's TCP
// channel can carry both the advertisement and a follow-up `ls-refs`
// command response. git-daemon is not split like HTTP — one TCP
// connection runs the full advertise-then-loop — so this exercises
// the `pktline.Reader` reused by [transport.Conn.Command] for
// command packets after the advertisement flush.
func TestNewGitServer_handlesV2Command(t *testing.T) {
	store := openLooseOnlySHA1Store(t)
	url := inttest.NewGitServer(t, store)
	conn := dialGitHarness(t, url)

	pr := conn.Advertisement()
	for {
		pkt, err := pr.ReadPacket()
		require.NoError(t, err)
		if pkt.Kind == pktline.Flush {
			break
		}
	}

	resp, err := conn.Command(t.Context(), "ls-refs",
		func(w *pktline.Writer) error {
			return wire.EncodeV2CommandRequest(w, "ls-refs",
				[]string{"peel", "symrefs"},
				[]string{"object-format=sha1"})
		})
	require.NoError(t, err)

	var sawHEAD, sawMain bool
	for {
		pkt, err := resp.ReadPacket()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		if pkt.Kind == pktline.Flush {
			break
		}
		if pkt.Kind != pktline.Data {
			continue
		}
		s := string(pkt.Data)
		if strings.Contains(s, " HEAD ") || strings.HasSuffix(s, " HEAD\n") {
			sawHEAD = true
		}
		if strings.Contains(s, " refs/heads/main\n") {
			sawMain = true
		}
	}
	assert.True(t, sawHEAD, "ls-refs response must carry a HEAD line")
	assert.True(t, sawMain, "ls-refs response must carry refs/heads/main")
}

// TestNewGitServer_acceptsMultipleConnections asserts that the
// harness's accept loop services more than one connection. A
// single-shot accept (the pattern in
// `transport/git/conn_roundtrip_test.go::startServer`) would pass
// the first probe and stall the second, so this catches a
// regression to that shape.
func TestNewGitServer_acceptsMultipleConnections(t *testing.T) {
	store := openLooseOnlySHA1Store(t)
	url := inttest.NewGitServer(t, store)

	const dialers = 4
	var wg sync.WaitGroup
	for i := range dialers {
		wg.Go(func() {
			tr := gitt.New(gitt.WithDialer(&net.Dialer{}))
			u, err := transport.ParseURL(url)
			require.NoError(t, err, "dialer %d: parse", i)
			v := transport.ProtocolV2
			conn, err := tr.Open(t.Context(), u, transport.OpenOptions{
				PreferredProtocol: &v,
			})
			require.NoError(t, err, "dialer %d: open", i)
			defer func() { _ = conn.Close() }()

			pr := conn.Advertisement()
			pkt, err := pr.ReadPacket()
			require.NoError(t, err, "dialer %d: read first packet", i)
			assert.Equal(t, "version 2\n", string(pkt.Data),
				"dialer %d: first advertisement packet", i)
		})
	}
	wg.Wait()
}

// TestNewGitServer_rejectsMalformedHandshake asserts that the
// harness closes the connection when the initial pkt-line does not
// satisfy the `git-upload-pack <path>\0host=<h>\0` shape. A
// well-formed transport never produces this frame; the test dials
// directly with `net.Dial` and writes a single junk pkt-line.
//
// The acceptance signal is the read EOF: a server that crashed or
// hung would surface as a timeout or non-EOF error instead.
func TestNewGitServer_rejectsMalformedHandshake(t *testing.T) {
	store := openLooseOnlySHA1Store(t)
	url := inttest.NewGitServer(t, store)

	addr := strings.TrimPrefix(url, "git://")
	if i := strings.Index(addr, "/"); i >= 0 {
		addr = addr[:i]
	}

	var d net.Dialer
	conn, err := d.DialContext(t.Context(), "tcp", addr)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))

	pw := pktline.NewWriter(conn)
	require.NoError(t, pw.WritePacket([]byte("not-a-valid-handshake\n")))

	// A well-behaved harness closes the connection after rejecting
	// the malformed pkt-line. EOF, ErrUnexpectedEOF, or a reset
	// (`connection reset by peer`, sometimes surfaced on Darwin
	// instead of EOF) are all acceptable: each means the server
	// hung up. A timeout means the server kept the connection open
	// and waited, which is the failure mode this test guards
	// against.
	buf := make([]byte, 64)
	_, err = conn.Read(buf)
	require.Error(t, err, "server must close the connection after a malformed handshake")
	var netErr net.Error
	if errors.As(err, &netErr) {
		assert.False(t, netErr.Timeout(),
			"server must close, not hang; got %v", err)
	}
}

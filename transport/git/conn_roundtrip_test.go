package gitt

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
	"github.com/hiddeco/go-ls-remote/internal/objstore"
	"github.com/hiddeco/go-ls-remote/internal/server"
	"github.com/hiddeco/go-ls-remote/internal/testfixture"
	"github.com/hiddeco/go-ls-remote/internal/wire"
	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/transport"
)

// startServer spins up a TCP listener that accepts one connection,
// reads the initial git-daemon pkt-line from the client (the
// server-side analog of `daemon.c::execute():749`), then runs
// `internal/server.Serve` over the connection until the client closes
// or the test ends.
func startServer[H objfmt.Hash](t *testing.T, store *objstore.Store[H]) (host, port string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	serverDone := make(chan struct{})
	t.Cleanup(func() { <-serverDone })
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		defer close(serverDone)
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()

		r := pktline.NewReader(c)
		w := pktline.NewWriter(c)

		// Read the initial discovery-time pkt-line — the server-side
		// analog of `daemon.c::execute():749`. Discard it; the test
		// cares about the post-handshake stream.
		if _, err := r.ReadPacket(); err != nil {
			return
		}

		_ = server.Serve(context.Background(), r, w, store, server.Options{
			PreferredProtocol: transport.ProtocolV2,
		})
	}()

	h, p, err := net.SplitHostPort(ln.Addr().String())
	require.NoError(t, err)
	return h, p
}

// openFixtureStore materialises the named fixture and opens an
// [objstore.Store[objfmt.SHA1Hash]] over it, ensuring objects/pack/
// exists (some ref-only fixtures ship without it). The store is closed
// via [testing.T.Cleanup] when the test ends.
func openFixtureStore(t *testing.T, name string) *objstore.Store[objfmt.SHA1Hash] {
	t.Helper()
	gitdir := testfixture.MaterializeRepo(t, name)
	require.NoError(t, os.MkdirAll(filepath.Join(gitdir, "objects", "pack"), 0o755))
	store, err := objstore.Open[objfmt.SHA1Hash](gitdir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// openRoundtripConn dials the test server that serves store and
// returns the [Conn] with the advertisement reader positioned at the
// first byte. Cleanup closes it.
func openRoundtripConn(t *testing.T, host, port string) *Conn {
	t.Helper()
	tr := New(WithDialer(&net.Dialer{}))
	u := &transport.URL{
		Scheme: "git",
		Host:   host,
		Port:   port,
		Path:   "/repo",
		Raw:    "git://" + host + ":" + port + "/repo",
	}
	conn, err := tr.Open(context.Background(), u, transport.OpenOptions{})
	require.NoError(t, err)
	c, ok := conn.(*Conn)
	require.True(t, ok)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// drainV2Advertisement reads all packets off rdr up to and including
// the trailing flush. The v2 advertisement shape is `version 2\n`
// followed by capability data lines and a flush per
// `serve.c::protocol_v2_advertise_capabilities`.
func drainV2Advertisement(t testing.TB, rdr *pktline.Reader) {
	t.Helper()
	for {
		p, err := rdr.ReadPacket()
		require.NoError(t, err)
		if p.Kind == pktline.Flush {
			return
		}
	}
}

// readRoundtripPackets drains rdr until it observes the response's
// terminating flush. Each [pktline.Packet.Data] slice is cloned
// because [pktline.Reader] reuses a single backing buffer across reads.
// The reader is left positioned at the next packet after the flush,
// ready for another command response — matching the v2 command-response
// framing (`gitprotocol-v2.adoc` §"Command Response").
func readRoundtripPackets(t *testing.T, rdr *pktline.Reader) []pktline.Packet {
	t.Helper()
	var pkts []pktline.Packet
	for {
		p, err := rdr.ReadPacket()
		require.NoError(t, err)
		if p.Data != nil {
			p.Data = bytes.Clone(p.Data)
		}
		pkts = append(pkts, p)
		if p.Kind == pktline.Flush {
			return pkts
		}
	}
}

// cmdBody builds a [transport.CommandBody] closure that calls
// [wire.EncodeV2CommandRequest] with name, args, and caps. This mirrors
// the canonical v2 command-request frame from `gitprotocol-v2.adoc`
// §"Command Request".
func cmdBody(name string, args, caps []string) transport.CommandBody {
	return func(w *pktline.Writer) error {
		return wire.EncodeV2CommandRequest(w, name, args, caps)
	}
}

// TestConn_Roundtrip_Advertisement_V2 verifies that after dialling the
// in-process server the first advertisement packet is `version 2\n`.
func TestConn_Roundtrip_Advertisement_V2(t *testing.T) {
	store := openFixtureStore(t, "empty")
	host, port := startServer(t, store)
	c := openRoundtripConn(t, host, port)

	pkt, err := c.Advertisement().ReadPacket()
	require.NoError(t, err)
	assert.Equal(t, pktline.Data, pkt.Kind)
	assert.Equal(t, "version 2\n", string(pkt.Data),
		"first advertisement packet must be `version 2\\n`")

	// Drain the rest so the server goroutine exits cleanly.
	drainV2Advertisement(t, c.Advertisement())
}

// TestConn_Roundtrip_LSRefs issues an `ls-refs` command over a real
// TCP round-trip to the in-process server. The `loose-only` fixture has
// refs/heads/main and refs/heads/feature/x and refs/tags/v1, so the
// response is non-empty.
func TestConn_Roundtrip_LSRefs(t *testing.T) {
	store := openFixtureStore(t, "loose-only")
	host, port := startServer(t, store)
	c := openRoundtripConn(t, host, port)

	drainV2Advertisement(t, c.Advertisement())

	rdr, err := c.Command(context.Background(), "ls-refs",
		cmdBody("ls-refs", []string{"peel", "symrefs"},
			[]string{"object-format=sha1"}))
	require.NoError(t, err)
	require.NotNil(t, rdr)
	assert.Same(t, c.Advertisement(), rdr,
		"Command must reuse the persistent reader")

	pkts := readRoundtripPackets(t, rdr)
	require.NotEmpty(t, pkts, "ls-refs must emit at least one packet")

	var hasMain bool
	for _, p := range pkts {
		if p.Kind != pktline.Data {
			continue
		}
		if strings.Contains(string(p.Data), " refs/heads/main\n") {
			hasMain = true
		}
	}
	assert.True(t, hasMain,
		"ls-refs response must include refs/heads/main; got %v", pkts)
}

// TestConn_Roundtrip_ObjectInfo issues an `object-info` command over a
// real TCP round-trip. The `loose-only` fixture uses the all-`a` OID as
// its sole ref tip; the server emits the `size\n` attrs line for any
// `size` request, so the response is at least one data packet plus flush.
func TestConn_Roundtrip_ObjectInfo(t *testing.T) {
	store := openFixtureStore(t, "loose-only")
	host, port := startServer(t, store)
	c := openRoundtripConn(t, host, port)

	drainV2Advertisement(t, c.Advertisement())

	oid := strings.Repeat("a", 40)
	rdr, err := c.Command(context.Background(), "object-info",
		cmdBody("object-info",
			[]string{"size", "oid " + oid},
			[]string{"object-format=sha1"}))
	require.NoError(t, err)
	require.NotNil(t, rdr)

	pkts := readRoundtripPackets(t, rdr)
	require.NotEmpty(t, pkts, "object-info must emit at least one packet")

	// Per `protocol-caps.c::send_info`, a `size` request produces a
	// `size\n` attrs line.
	var hasSize bool
	for _, p := range pkts {
		if p.Kind != pktline.Data {
			continue
		}
		if strings.TrimRight(string(p.Data), "\n") == "size" {
			hasSize = true
		}
	}
	assert.True(t, hasSize,
		"object-info response must include the `size` attrs line; got %v", pkts)
}

// TestConn_Command_AfterCloseFails verifies that calling [Conn.Command]
// after [Conn.Close] surfaces a `*ProtocolError{Op: "command"}` whose
// cause is a net-closed shape.
func TestConn_Command_AfterCloseFails(t *testing.T) {
	store := openFixtureStore(t, "empty")
	host, port := startServer(t, store)
	c := openRoundtripConn(t, host, port)

	drainV2Advertisement(t, c.Advertisement())
	require.NoError(t, c.Close())

	rdr, err := c.Command(context.Background(), "ls-refs",
		cmdBody("ls-refs", nil, nil))
	assert.Nil(t, rdr)
	require.Error(t, err)

	var pe *ProtocolError
	require.True(t, errors.As(err, &pe),
		"Command after Close must surface as *ProtocolError; got %T: %v", err, err)
	assert.Equal(t, "command", pe.Op)
	assert.True(t, errors.Is(err, net.ErrClosed),
		"cause must be a net-closed shape; got %v", err)
}

// TestConn_Command_ContextCanceled verifies that a pre-cancelled context
// surfaces a `*ProtocolError{Op: "command"}` wrapping [context.Canceled]
// before any I/O is attempted.
func TestConn_Command_ContextCanceled(t *testing.T) {
	store := openFixtureStore(t, "empty")
	host, port := startServer(t, store)
	c := openRoundtripConn(t, host, port)

	drainV2Advertisement(t, c.Advertisement())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	rdr, err := c.Command(ctx, "ls-refs",
		cmdBody("ls-refs", nil, nil))
	assert.Nil(t, rdr)
	require.Error(t, err)

	var pe *ProtocolError
	require.True(t, errors.As(err, &pe),
		"cancelled context must surface as *ProtocolError; got %T: %v", err, err)
	assert.Equal(t, "command", pe.Op)
	assert.True(t, errors.Is(err, context.Canceled),
		"cause must be context.Canceled; got %v", err)
}

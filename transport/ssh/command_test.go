package ssht

import (
	"bytes"
	"context"
	"errors"
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

// cmdBody mirrors the file transport's helper of the same name: a
// closure over `cmd, args, caps` that hands those to
// [wire.EncodeV2CommandRequest] when the transport's [pktline.Writer]
// is supplied.
func cmdBody(cmd string, args, caps []string) transport.CommandBody {
	return func(w *pktline.Writer) error {
		return wire.EncodeV2CommandRequest(w, cmd, args, caps)
	}
}

// drainAdvertisement reads the v2 advertisement off the [Conn]'s
// reader up to and including the trailing flush so the test is
// positioned to invoke [Conn.Command]. The advertisement shape is
// `version 2\n` plus capability data lines plus a flush per
// `serve.c::protocol_v2_advertise_capabilities`.
func drainAdvertisement(t testing.TB, c *Conn) {
	t.Helper()
	rdr := c.Advertisement()
	for {
		p, err := rdr.ReadPacket()
		require.NoError(t, err)
		if p.Kind == pktline.Flush {
			return
		}
	}
}

// readAllPackets drains rdr until it observes the response's
// terminating flush, collecting every packet (data, flush, delim,
// response-end) it produced. Each [pktline.Packet.Data] slice is
// cloned because [pktline.Reader] reuses a single backing buffer
// across reads.
func readAllPackets(t *testing.T, rdr *pktline.Reader) []pktline.Packet {
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

// bridgeSHA1Store materialises the named fixture and returns a bridge
// callback that runs `server.Serve` against a freshly opened
// [objstore.Store] over [objfmt.SHA1Hash]. The store is closed when
// the test ends; the bridge handler is what
// [testServerOpts.serveStore] invokes after the fixture has peeled
// the initial extra-parameter pkt-line.
func bridgeSHA1Store(t *testing.T, fixture string) bridgeServer {
	t.Helper()

	gitdir := testfixture.MaterializeRepo(t, fixture)
	require.NoError(t, os.MkdirAll(filepath.Join(gitdir, "objects", "pack"), 0o755))
	store, err := objstore.Open[objfmt.SHA1Hash](gitdir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	return bridgeServer{
		tb: t,
		serve: func(ctx context.Context, r *pktline.Reader, w *pktline.Writer) error {
			return server.Serve(ctx, r, w, store, server.Options{
				Agent:             "test-server/0.0",
				PreferredProtocol: transport.ProtocolV2,
			})
		},
	}
}

// openBridgedConn dials the in-process SSH fixture against a bridged
// SHA1 store, drains the advertisement, and returns the typed [Conn]
// ready for [Conn.Command]. Centralising the boilerplate keeps the
// per-test bodies focused on the round-trip assertion.
func openBridgedConn(t *testing.T, fixture string) *Conn {
	t.Helper()

	bridge := bridgeSHA1Store(t, fixture)
	srv := newTestServer(t, testServerOpts{
		acceptEnv: true,
		serveStore: func() bridgeServer {
			return bridge
		},
	})

	tr := New(
		WithAuth(Signer(srv.clientSigner)),
		WithKnownHosts(srv.hostKeyCallback()),
	)
	conn, err := tr.Open(context.Background(), srv.URL(), defaultOpenOptions())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	c, ok := conn.(*Conn)
	require.True(t, ok)
	drainAdvertisement(t, c)
	return c
}

func TestCommand_lsRefs(t *testing.T) {
	c := openBridgedConn(t, "loose-only")

	rdr, err := c.Command(context.Background(), "ls-refs",
		cmdBody("ls-refs", []string{"peel"}, []string{"object-format=sha1"}))
	require.NoError(t, err)
	require.NotNil(t, rdr)

	pkts := readAllPackets(t, rdr)
	require.NotEmpty(t, pkts, "ls-refs must emit at least one packet")

	var hasHead, hasMain bool
	for _, p := range pkts {
		if p.Kind != pktline.Data {
			continue
		}
		s := string(p.Data)
		if strings.Contains(s, " HEAD\n") {
			hasHead = true
		}
		if strings.Contains(s, " refs/heads/main\n") {
			hasMain = true
		}
	}
	assert.True(t, hasHead, "ls-refs response must include a HEAD line; got %q", pkts)
	assert.True(t, hasMain, "ls-refs response must include refs/heads/main; got %q", pkts)
}

func TestCommand_objectInfo(t *testing.T) {
	c := openBridgedConn(t, "loose-only")

	// The `aaaa...` OID is loose-only's ref tip. The handler is not
	// asked to resolve the object on disk — we only need a well-formed
	// `oid <hex>` argument so the server's parser accepts the request
	// and emits its `size\n` attrs line.
	oid := strings.Repeat("a", 40)
	rdr, err := c.Command(context.Background(), "object-info",
		cmdBody("object-info",
			[]string{"size", "oid " + oid}, []string{"object-format=sha1"}))
	require.NoError(t, err)
	require.NotNil(t, rdr)

	pkts := readAllPackets(t, rdr)
	require.NotEmpty(t, pkts, "object-info must emit at least one packet")

	var hasSize bool
	for _, p := range pkts {
		if p.Kind != pktline.Data {
			continue
		}
		if strings.TrimRight(string(p.Data), "\n") == "size" {
			hasSize = true
		}
	}
	assert.True(t, hasSize, "object-info response must include the `size` attrs line")
}

func TestCommand_contextCancelled(t *testing.T) {
	c := openBridgedConn(t, "loose-only")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	rdr, err := c.Command(ctx, "ls-refs",
		cmdBody("ls-refs", nil, []string{"object-format=sha1"}))
	assert.Nil(t, rdr)
	require.Error(t, err)

	var pe *ProtocolError
	require.ErrorAs(t, err, &pe,
		"a pre-cancelled context must surface as *ProtocolError")
	assert.Equal(t, "command", pe.Op)
	assert.True(t, errors.Is(err, context.Canceled),
		"the cancellation must remain matchable through the wrapper")
}

func TestCommand_closedPipe(t *testing.T) {
	c := openBridgedConn(t, "loose-only")

	require.NoError(t, c.Close())

	rdr, err := c.Command(context.Background(), "ls-refs",
		cmdBody("ls-refs", nil, []string{"object-format=sha1"}))
	assert.Nil(t, rdr)
	require.Error(t, err)

	var pe *ProtocolError
	require.ErrorAs(t, err, &pe,
		"a Command after Close must surface as *ProtocolError; got %T: %v", err, err)
	assert.Equal(t, "command", pe.Op)
}

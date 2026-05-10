package filet

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/transport"
)

// drainAdvertisement reads the v2 advertisement off the [Conn]'s reader
// up to and including the trailing flush so the test is positioned to
// invoke [Conn.Command]. The advertisement shape is `version 2\n` plus
// capability data lines plus a flush per
// `serve.c::protocol_v2_advertise_capabilities`.
func drainAdvertisement(t *testing.T, c *Conn) {
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
// response-end) it produced. Each [pktline.Packet.Data] slice is cloned
// because [pktline.Reader] reuses a single backing buffer across reads.
//
// Unlike the HTTP transport's helper of the same name, the file
// transport's reader is shared across commands and never returns EOF on
// a clean response: the server stays parked in its command loop after
// emitting the trailing flush. Returning on the first flush therefore
// matches the v2 command-response framing
// (`gitprotocol-v2.adoc` §"Command Response") and leaves the reader
// positioned at the start of the next response.
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

// materializeServeableFixture mirrors `transport/http`'s
// `openFixtureStore` shape but stops short of opening an
// [internal/objstore.Store]: it materialises the named fixture and
// ensures `objects/pack/` exists (some ref-only fixtures ship without
// one). The transport opens the store itself; this helper only smooths
// over the missing-pack-dir gap.
func materializeServeableFixture(t *testing.T, name string) string {
	t.Helper()
	gitdir := materializeRepoFixture(t, name)
	require.NoError(t, os.MkdirAll(filepath.Join(gitdir, "objects", "pack"), 0o755))
	return gitdir
}

// openTestConn dials the named fixture, drains the advertisement, and
// arranges for the [Conn] to close at test end. Centralising the
// boilerplate keeps the per-test bodies focused on the round-trip
// assertion.
func openTestConn(t *testing.T, fixture string) *Conn {
	t.Helper()
	gitdir := materializeServeableFixture(t, fixture)
	u, err := transport.ParseURL("file://" + gitdir)
	require.NoError(t, err)

	tr := New()
	conn, err := tr.Open(context.Background(), u, transport.OpenOptions{UserAgent: "test/0.0"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	c, ok := conn.(*Conn)
	require.True(t, ok)
	drainAdvertisement(t, c)
	return c
}

func TestConn_Command_LSRefs_RoundTrip(t *testing.T) {
	c := openTestConn(t, "loose-only")

	rdr, err := c.Command(context.Background(), "ls-refs",
		[]string{"peel"}, []string{"object-format=sha1"})
	require.NoError(t, err)
	require.NotNil(t, rdr)
	assert.Same(t, c.Advertisement(), rdr,
		"Command must reuse the persistent reader so the response stream "+
			"continues on the same pipe pair")

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

func TestConn_Command_ObjectInfo_RoundTrip(t *testing.T) {
	c := openTestConn(t, "loose-only")

	// `aaaa...` is loose-only's ref tip. The handler is not asked to
	// resolve the object on disk — we only need a well-formed
	// `oid <hex>` argument so the server's parser accepts the request
	// and emits its `size\n` attrs line.
	oid := strings.Repeat("a", 40)
	rdr, err := c.Command(context.Background(), "object-info",
		[]string{"size", "oid " + oid}, []string{"object-format=sha1"})
	require.NoError(t, err)
	require.NotNil(t, rdr)

	pkts := readAllPackets(t, rdr)
	require.NotEmpty(t, pkts, "object-info must emit at least one packet")

	// Per `protocol-caps.c::send_info`, an object-info request with
	// `size` produces a `size\n` attrs line followed by a per-OID line.
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

func TestConn_Command_SequentialCommandsReuseReader(t *testing.T) {
	c := openTestConn(t, "loose-only")

	// First command: ls-refs.
	rdr1, err := c.Command(context.Background(), "ls-refs",
		nil, []string{"object-format=sha1"})
	require.NoError(t, err)
	_ = readAllPackets(t, rdr1)

	// Second command: object-info, on the same Conn. The single-flight
	// contract requires the previous response be drained before the
	// next request is dispatched; the helper above does that.
	oid := strings.Repeat("a", 40)
	rdr2, err := c.Command(context.Background(), "object-info",
		[]string{"size", "oid " + oid}, []string{"object-format=sha1"})
	require.NoError(t, err)

	assert.Same(t, rdr1, rdr2,
		"sequential commands must reuse the persistent reader")

	pkts := readAllPackets(t, rdr2)
	require.NotEmpty(t, pkts)
}

func TestConn_Command_RejectsOversizePayload(t *testing.T) {
	c := openTestConn(t, "loose-only")

	overlong := strings.Repeat("a", pktline.MaxPayload)
	tests := []struct {
		name string
		cmd  string
		args []string
		caps []string
	}{
		{name: "oversize command name", cmd: overlong},
		{name: "oversize capability", cmd: "ls-refs", caps: []string{overlong}},
		{name: "oversize argument", cmd: "ls-refs", args: []string{overlong}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rdr, err := c.Command(context.Background(), tc.cmd, tc.args, tc.caps)
			assert.Nil(t, rdr)
			require.Error(t, err)
			assert.True(t, errors.Is(err, pktline.ErrPayloadTooLarge),
				"oversize input must wrap pktline.ErrPayloadTooLarge; got %v", err)
		})
	}
}

func TestConn_Command_AfterCloseReturnsProtocolError(t *testing.T) {
	gitdir := materializeServeableFixture(t, "loose-only")
	u, err := transport.ParseURL("file://" + gitdir)
	require.NoError(t, err)

	tr := New()
	conn, err := tr.Open(context.Background(), u, transport.OpenOptions{})
	require.NoError(t, err)
	c, ok := conn.(*Conn)
	require.True(t, ok)
	drainAdvertisement(t, c)

	require.NoError(t, conn.Close())

	rdr, err := c.Command(context.Background(), "ls-refs", nil,
		[]string{"object-format=sha1"})
	assert.Nil(t, rdr)
	require.Error(t, err)
	var pe *ProtocolError
	assert.True(t, errors.As(err, &pe),
		"a Command after Close must surface as *ProtocolError; got %T: %v", err, err)
	if pe != nil {
		assert.Equal(t, "command", pe.Op)
	}
}

func TestConn_Lifecycle_NoGoroutineLeak(t *testing.T) {
	gitdir := materializeServeableFixture(t, "loose-only")
	u, err := transport.ParseURL("file://" + gitdir)
	require.NoError(t, err)

	baseline := runtime.NumGoroutine()

	tr := New()
	conn, err := tr.Open(context.Background(), u, transport.OpenOptions{})
	require.NoError(t, err)
	c, ok := conn.(*Conn)
	require.True(t, ok)

	drainAdvertisement(t, c)

	rdr, err := c.Command(context.Background(), "ls-refs", nil,
		[]string{"object-format=sha1"})
	require.NoError(t, err)
	_ = readAllPackets(t, rdr)

	oid := strings.Repeat("a", 40)
	rdr, err = c.Command(context.Background(), "object-info",
		[]string{"size", "oid " + oid}, []string{"object-format=sha1"})
	require.NoError(t, err)
	_ = readAllPackets(t, rdr)

	require.NoError(t, conn.Close())
	require.NoError(t, conn.Close(), "Close must be idempotent")

	// Allow the runtime a brief window to reclaim the server goroutine.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline {
		if time.Now().After(deadline) {
			t.Fatalf("server goroutine did not exit after Close: have %d > baseline %d",
				runtime.NumGoroutine(), baseline)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestConn_Command_BodyShape pins the on-wire request body
// against the canonical v2 command-request grammar. The test snoops
// the bytes written to `c.writer` by reading them out of a custom
// pipe interposed in front of the server goroutine — but for
// in-process file transport, the simpler proof is to rely on the
// server's correct response (covered by the round-trip tests above)
// and assert here only on the encoder shape that the `Command` body
// produces. To do that without tearing apart the [Conn], we read
// directly from the server-side pipe end via an alternative entry
// point: write the same request through a fresh [pktline.Writer] over
// a [bytes.Buffer] and decode it back.
//
// This guards against future drift: a subtle reorder (delim-before-cap,
// missing trailing LF, missing flush) would still produce a parseable
// response from the lenient server but would diverge from the canonical
// grammar a stricter peer would reject.
func TestConn_Command_BodyShape(t *testing.T) {
	var buf bytes.Buffer
	w := pktline.NewWriter(&buf)
	require.NoError(t, encodeV2CommandRequest(w, "ls-refs",
		[]string{"peel", "symrefs"}, []string{"object-format=sha1"}))

	pr := pktline.NewReader(bytes.NewReader(buf.Bytes()))
	want := []struct {
		kind pktline.Kind
		data string
	}{
		{pktline.Data, "command=ls-refs\n"},
		{pktline.Data, "object-format=sha1\n"},
		{pktline.Delim, ""},
		{pktline.Data, "peel\n"},
		{pktline.Data, "symrefs\n"},
		{pktline.Flush, ""},
	}
	for i, w := range want {
		p, err := pr.ReadPacket()
		require.NoError(t, err, "packet %d", i)
		assert.Equal(t, w.kind, p.Kind, "packet %d kind", i)
		if w.kind == pktline.Data {
			assert.Equal(t, w.data, string(p.Data), "packet %d data", i)
		}
	}

	// Encoder is internally consistent: a writer error from a saturated
	// pipe surfaces verbatim rather than being silently dropped.
	bad := &errorWriter{err: io.ErrClosedPipe}
	bw := pktline.NewWriter(bad)
	err := encodeV2CommandRequest(bw, "ls-refs", nil, nil)
	require.Error(t, err)
	assert.True(t, errors.Is(err, io.ErrClosedPipe))
}

// errorWriter is an [io.Writer] that always fails with err. It is the
// minimal stub for testing the encoder's error-propagation contract.
type errorWriter struct{ err error }

func (e *errorWriter) Write(_ []byte) (int, error) { return 0, e.err }

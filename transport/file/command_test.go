package filet

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
	"github.com/hiddeco/go-ls-remote/internal/testfixture"
	"github.com/hiddeco/go-ls-remote/internal/wire"
	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/transport"
)

// cmdBody is the v2-command body callback the per-test boilerplate
// uses. It mirrors what `*lsremote.Session` builds at the call site:
// a closure over `cmd, args, caps` that hands those to
// [wire.EncodeV2CommandRequest] when the transport's
// [pktline.Writer] is supplied.
func cmdBody(cmd string, args, caps []string) transport.CommandBody {
	return func(w *pktline.Writer) error {
		return wire.EncodeV2CommandRequest(w, cmd, args, caps)
	}
}

// drainAdvertisement reads the v2 advertisement off the [Conn]'s reader
// up to and including the trailing flush so the test is positioned to
// invoke [Conn.Command]. The advertisement shape is `version 2\n` plus
// capability data lines plus a flush per
// [serve.c::protocol_v2_advertise_capabilities].
//
// [serve.c::protocol_v2_advertise_capabilities]: https://github.com/git/git/blob/v2.54.0/serve.c#L186
func drainAdvertisement(t testing.TB, c *Conn[objfmt.SHA1Hash]) {
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
// ([gitprotocol-v2.adoc §"Command Response"]) and leaves the reader
// positioned at the start of the next response.
//
// [gitprotocol-v2.adoc §"Command Response"]: https://github.com/git/git/blob/v2.54.0/Documentation/gitprotocol-v2.adoc#command-request
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
// [internal/objstore.Store[objfmt.SHA1Hash]]: it materialises the named fixture and
// ensures `objects/pack/` exists (some ref-only fixtures ship without
// one). The transport opens the store itself; this helper only smooths
// over the missing-pack-dir gap.
func materializeServeableFixture(t testing.TB, name string) string {
	t.Helper()
	gitdir := testfixture.MaterializeRepo(t, name)
	require.NoError(t, os.MkdirAll(filepath.Join(gitdir, "objects", "pack"), 0o755))
	return gitdir
}

// openTestConn dials the named fixture, drains the advertisement, and
// arranges for the [Conn] to close at test end. Centralising the
// boilerplate keeps the per-test bodies focused on the round-trip
// assertion.
func openTestConn(t *testing.T, fixture string) *Conn[objfmt.SHA1Hash] {
	t.Helper()
	gitdir := materializeServeableFixture(t, fixture)
	u, err := transport.ParseURL("file://" + gitdir)
	require.NoError(t, err)

	tr := New()
	conn, err := tr.Open(t.Context(), u, transport.OpenOptions{UserAgent: "test/0.0"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	c, ok := conn.(*Conn[objfmt.SHA1Hash])
	require.True(t, ok)
	drainAdvertisement(t, c)
	return c
}

func TestConn_Command_LSRefs_RoundTrip(t *testing.T) {
	t.Parallel()
	c := openTestConn(t, "loose-only")

	rdr, err := c.Command(t.Context(), "ls-refs",
		cmdBody("ls-refs", []string{"peel"}, []string{"object-format=sha1"}))
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
	t.Parallel()
	c := openTestConn(t, "loose-only")

	// `aaaa...` is loose-only's ref tip. The handler is not asked to
	// resolve the object on disk — we only need a well-formed
	// `oid <hex>` argument so the server's parser accepts the request
	// and emits its `size\n` attrs line.
	oid := strings.Repeat("a", 40)
	rdr, err := c.Command(t.Context(), "object-info",
		cmdBody("object-info",
			[]string{"size", "oid " + oid}, []string{"object-format=sha1"}))
	require.NoError(t, err)
	require.NotNil(t, rdr)

	pkts := readAllPackets(t, rdr)
	require.NotEmpty(t, pkts, "object-info must emit at least one packet")

	// Per [protocol-caps.c::send_info], an object-info request with
	// `size` produces a `size\n` attrs line followed by a per-OID line.
	//
	// [protocol-caps.c::send_info]: https://github.com/git/git/blob/v2.54.0/protocol-caps.c#L37
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
	t.Parallel()
	c := openTestConn(t, "loose-only")

	// First command: ls-refs.
	rdr1, err := c.Command(t.Context(), "ls-refs",
		cmdBody("ls-refs", nil, []string{"object-format=sha1"}))
	require.NoError(t, err)
	_ = readAllPackets(t, rdr1)

	// Second command: object-info, on the same Conn. The single-flight
	// contract requires the previous response be drained before the
	// next request is dispatched; the helper above does that.
	oid := strings.Repeat("a", 40)
	rdr2, err := c.Command(t.Context(), "object-info",
		cmdBody("object-info",
			[]string{"size", "oid " + oid}, []string{"object-format=sha1"}))
	require.NoError(t, err)

	assert.Same(t, rdr1, rdr2,
		"sequential commands must reuse the persistent reader")

	pkts := readAllPackets(t, rdr2)
	require.NotEmpty(t, pkts)
}

func TestConn_Command_RejectsOversizePayload(t *testing.T) {
	t.Parallel()
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
			t.Parallel()
			// Fresh [Conn] per subtest. Once the [pktline.Writer.WritePacket]
			// cap check trips mid-frame, any partially-emitted bytes have
			// already crossed the pipe (only the failing packet itself is
			// elided); the in-process server's command parser sees a
			// truncated request and the Conn's single-flight contract
			// considers the [Conn] dead. Reusing the [Conn] across
			// subtests would then surface as a server-side framing
			// mismatch on the next call rather than as the cap-rejection
			// the test is pinning.
			c := openTestConn(t, "loose-only")
			rdr, err := c.Command(t.Context(), tc.cmd,
				cmdBody(tc.cmd, tc.args, tc.caps))
			assert.Nil(t, rdr)
			require.Error(t, err)
			assert.ErrorIs(t, err, pktline.ErrPayloadTooLarge,
				"oversize input must wrap pktline.ErrPayloadTooLarge; got %v", err)
		})
	}
}

func TestConn_Command_AfterCloseReturnsProtocolError(t *testing.T) {
	t.Parallel()
	gitdir := materializeServeableFixture(t, "loose-only")
	u, err := transport.ParseURL("file://" + gitdir)
	require.NoError(t, err)

	tr := New()
	conn, err := tr.Open(t.Context(), u, transport.OpenOptions{})
	require.NoError(t, err)
	c, ok := conn.(*Conn[objfmt.SHA1Hash])
	require.True(t, ok)
	drainAdvertisement(t, c)

	require.NoError(t, conn.Close())

	rdr, err := c.Command(t.Context(), "ls-refs",
		cmdBody("ls-refs", nil, []string{"object-format=sha1"}))
	assert.Nil(t, rdr)
	require.Error(t, err)
	var pe *ProtocolError
	require.ErrorAs(t, err, &pe,
		"a Command after Close must surface as *ProtocolError; got %T: %v", err, err)
	if pe != nil {
		assert.Equal(t, "command", pe.Op)
	}
}

func TestConn_Lifecycle_OpenDrainCommandsClose(t *testing.T) {
	t.Parallel()
	gitdir := materializeServeableFixture(t, "loose-only")
	u, err := transport.ParseURL("file://" + gitdir)
	require.NoError(t, err)

	tr := New()
	conn, err := tr.Open(t.Context(), u, transport.OpenOptions{})
	require.NoError(t, err)
	c, ok := conn.(*Conn[objfmt.SHA1Hash])
	require.True(t, ok)

	drainAdvertisement(t, c)

	rdr, err := c.Command(t.Context(), "ls-refs",
		cmdBody("ls-refs", nil, []string{"object-format=sha1"}))
	require.NoError(t, err)
	_ = readAllPackets(t, rdr)

	oid := strings.Repeat("a", 40)
	rdr, err = c.Command(t.Context(), "object-info",
		cmdBody("object-info",
			[]string{"size", "oid " + oid}, []string{"object-format=sha1"}))
	require.NoError(t, err)
	_ = readAllPackets(t, rdr)

	require.NoError(t, conn.Close())
	require.NoError(t, conn.Close(), "Close must be idempotent")

	// The server-goroutine teardown after this full open/drain/two-
	// command/close cycle is asserted package-wide by
	// `goleak.VerifyTestMain` in `main_test.go`.
}

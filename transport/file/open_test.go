package filet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hiddeco/go-ls-remote/internal/testfixture"
	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/transport"
)

func TestTransport_Open_AdvertisementIsV2(t *testing.T) {
	t.Parallel()

	gitdir := testfixture.MaterializeRepo(t, "empty")
	u, err := transport.ParseURL("file://" + gitdir)
	require.NoError(t, err)

	tr := New()
	conn, err := tr.Open(t.Context(), u, transport.OpenOptions{UserAgent: "test/0.0"})
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	rdr := conn.Advertisement()
	pkt, err := rdr.ReadPacket()
	require.NoError(t, err)
	require.Equal(t, pktline.Data, pkt.Kind)
	assert.Equal(t, "version 2\n", string(pkt.Data))
}

func TestTransport_Open_NotARepo(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	u, err := transport.ParseURL("file://" + dir)
	require.NoError(t, err)

	tr := New()
	conn, err := tr.Open(t.Context(), u, transport.OpenOptions{})
	require.Error(t, err)
	assert.Nil(t, conn)
	require.ErrorIs(t, err, ErrNotFound,
		"missing repo must surface ErrNotFound; got %v", err)

	var pe *ProtocolError
	require.ErrorAs(t, err, &pe,
		"missing repo must surface as *ProtocolError; got %T", err)
	assert.Equal(t, "dial", pe.Op)
}

func TestTransport_Open_PathPercentDecodeError(t *testing.T) {
	t.Parallel()

	// `%2g` is not a valid percent-escape: `g` is not a hex digit. The
	// dial path must reject the URL up-front rather than passing the
	// undecoded string to `objstore.Open`.
	u, err := transport.ParseURL("file:///tmp/repo%2gpath")
	require.NoError(t, err)

	tr := New()
	conn, err := tr.Open(t.Context(), u, transport.OpenOptions{})
	require.Error(t, err)
	assert.Nil(t, conn)
	assert.ErrorIs(t, err, ErrNotFound,
		"malformed escape is callable equivalent to non-existent repo; got %v", err)
}

func TestTransport_Open_PathPercentDecodeSucceeds(t *testing.T) {
	t.Parallel()

	// Materialise the fixture under a directory whose name contains a
	// space, then dial with the percent-encoded URL. The dial path must
	// reach `objstore.Open` with the decoded path.
	gitdir := testfixture.MaterializeRepo(t, "empty")

	parent := filepath.Dir(gitdir)
	spaced := filepath.Join(filepath.Dir(parent), "repo with spaces")
	require.NoError(t, os.Rename(parent, spaced))
	gitdir = filepath.Join(spaced, filepath.Base(gitdir))

	encoded := strings.ReplaceAll(gitdir, " ", "%20")
	u, err := transport.ParseURL("file://" + encoded)
	require.NoError(t, err)

	tr := New()
	conn, err := tr.Open(t.Context(), u, transport.OpenOptions{})
	require.NoError(t, err)
	require.NoError(t, conn.Close())
}

func TestTransport_Open_PinV1Rejected(t *testing.T) {
	t.Parallel()

	gitdir := testfixture.MaterializeRepo(t, "empty")
	u, err := transport.ParseURL("file://" + gitdir)
	require.NoError(t, err)

	v1 := transport.ProtocolV1
	tr := New()
	conn, err := tr.Open(t.Context(), u, transport.OpenOptions{
		PreferredProtocol: &v1,
	})
	require.Error(t, err)
	assert.Nil(t, conn)
	assert.ErrorIs(t, err, ErrUnsupportedProtocol,
		"v1 pin must surface ErrUnsupportedProtocol; got %v", err)
}

func TestTransport_Open_UnsupportedFormat(t *testing.T) {
	t.Parallel()

	// A gitdir with `extensions.refStorage = packed` is rejected by
	// `objstore.Open` with `objstore.ErrUnsupportedFormat`. The dial
	// path must surface that as the format-specific sentinel
	// [ErrUnsupportedFormat], not the protocol-pin sentinel.
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "HEAD"),
		[]byte("ref: refs/heads/main\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "objects"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "refs"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config"),
		[]byte("[extensions]\n\trefStorage = packed\n"), 0o644))

	u, err := transport.ParseURL("file://" + dir)
	require.NoError(t, err)

	tr := New()
	conn, err := tr.Open(t.Context(), u, transport.OpenOptions{})
	require.Error(t, err)
	assert.Nil(t, conn)
	require.ErrorIs(t, err, ErrUnsupportedFormat,
		"unsupported repo format must surface ErrUnsupportedFormat; got %v", err)
	require.NotErrorIs(t, err, ErrUnsupportedProtocol,
		"format errors must not match the protocol-pin sentinel; got %v", err)

	var pe *ProtocolError
	require.ErrorAs(t, err, &pe,
		"unsupported format must surface as *ProtocolError; got %T", err)
	assert.Equal(t, "dial", pe.Op)
}

func TestTransport_Open_CloseMidStreamIsIdempotent(t *testing.T) {
	t.Parallel()

	gitdir := testfixture.MaterializeRepo(t, "empty")
	u, err := transport.ParseURL("file://" + gitdir)
	require.NoError(t, err)

	tr := New()
	conn, err := tr.Open(t.Context(), u, transport.OpenOptions{})
	require.NoError(t, err)

	// Read one packet so the goroutine has produced output and is
	// blocked on the next write or the context.
	rdr := conn.Advertisement()
	_, err = rdr.ReadPacket()
	require.NoError(t, err)

	require.NoError(t, conn.Close())

	// Close must be idempotent: a second call returns nil without
	// blocking on the already-closed `done` channel.
	require.NoError(t, conn.Close())

	// The server-goroutine teardown is asserted package-wide by
	// `goleak.VerifyTestMain` in `main_test.go`; this test now only
	// pins the Close-idempotency contract.
}

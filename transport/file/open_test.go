package filet

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hiddeco/go-ls-remote/internal/testfixture"
	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/transport"
)

func TestTransport_Open_AdvertisementIsV2(t *testing.T) {
	gitdir := testfixture.MaterializeRepo(t, "empty")
	u, err := transport.ParseURL("file://" + gitdir)
	require.NoError(t, err)

	tr := New()
	conn, err := tr.Open(context.Background(), u, transport.OpenOptions{UserAgent: "test/0.0"})
	require.NoError(t, err)
	defer conn.Close()

	rdr := conn.Advertisement()
	pkt, err := rdr.ReadPacket()
	require.NoError(t, err)
	require.Equal(t, pktline.Data, pkt.Kind)
	assert.Equal(t, "version 2\n", string(pkt.Data))
}

func TestTransport_Open_NotARepo(t *testing.T) {
	dir := t.TempDir()
	u, err := transport.ParseURL("file://" + dir)
	require.NoError(t, err)

	tr := New()
	conn, err := tr.Open(context.Background(), u, transport.OpenOptions{})
	require.Error(t, err)
	assert.Nil(t, conn)
	assert.True(t, errors.Is(err, ErrNotFound),
		"missing repo must surface ErrNotFound; got %v", err)

	var pe *ProtocolError
	require.True(t, errors.As(err, &pe),
		"missing repo must surface as *ProtocolError; got %T", err)
	assert.Equal(t, "dial", pe.Op)
}

func TestTransport_Open_PathPercentDecodeError(t *testing.T) {
	// `%2g` is not a valid percent-escape: `g` is not a hex digit. The
	// dial path must reject the URL up-front rather than passing the
	// undecoded string to `objstore.Open`.
	u, err := transport.ParseURL("file:///tmp/repo%2gpath")
	require.NoError(t, err)

	tr := New()
	conn, err := tr.Open(context.Background(), u, transport.OpenOptions{})
	require.Error(t, err)
	assert.Nil(t, conn)
	assert.True(t, errors.Is(err, ErrNotFound),
		"malformed escape is callable equivalent to non-existent repo; got %v", err)
}

func TestTransport_Open_PathPercentDecodeSucceeds(t *testing.T) {
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
	conn, err := tr.Open(context.Background(), u, transport.OpenOptions{})
	require.NoError(t, err)
	defer conn.Close()
}

func TestTransport_Open_PinV1Rejected(t *testing.T) {
	gitdir := testfixture.MaterializeRepo(t, "empty")
	u, err := transport.ParseURL("file://" + gitdir)
	require.NoError(t, err)

	v1 := transport.ProtocolV1
	tr := New()
	conn, err := tr.Open(context.Background(), u, transport.OpenOptions{
		PreferredProtocol: &v1,
	})
	require.Error(t, err)
	assert.Nil(t, conn)
	assert.True(t, errors.Is(err, ErrUnsupportedProtocol),
		"v1 pin must surface ErrUnsupportedProtocol; got %v", err)
}

func TestTransport_Open_CloseStopsServerGoroutine(t *testing.T) {
	gitdir := testfixture.MaterializeRepo(t, "empty")
	u, err := transport.ParseURL("file://" + gitdir)
	require.NoError(t, err)

	baseline := runtime.NumGoroutine()

	tr := New()
	conn, err := tr.Open(context.Background(), u, transport.OpenOptions{})
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

	// The server goroutine should exit promptly after Close. Allow a
	// short window for the runtime to reclaim it before comparing
	// against the baseline.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline {
		if time.Now().After(deadline) {
			t.Fatalf("server goroutine did not exit: have %d > baseline %d",
				runtime.NumGoroutine(), baseline)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

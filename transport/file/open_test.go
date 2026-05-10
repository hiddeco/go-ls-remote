package filet

import (
	"context"
	"errors"
	"io/fs"
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

// materializeRepoFixture mirrors the helper of the same name in
// `internal/server` and `transport/http`: it copies the named fixture
// from `testdata/repos/<name>/` into a fresh `t.TempDir()`, renaming
// the committed `dotgit` component to `.git`. Canonical Git refuses
// to track a path containing a literal `.git` component (see
// `path.c::is_dotgit_path`), so the on-disk fixtures store the
// gitdir under a `dotgit/` directory and tests rename it on
// materialization.
//
// A small helper is duplicated rather than extracted into a shared
// test-support package: there is no shared `testdata` helper module
// in the library yet, and the function is short enough that the
// duplication is the lower-friction option than an `internal/testutil`
// crate every test package would have to import.
func materializeRepoFixture(t *testing.T, name string) string {
	t.Helper()

	wd, err := os.Getwd()
	require.NoError(t, err)
	src := filepath.Join(wd, "..", "..", "testdata", "repos", name)
	info, err := os.Stat(src)
	require.NoError(t, err, "fixture %q missing", name)
	require.True(t, info.IsDir())

	dst := t.TempDir()
	require.NoError(t, filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		var parts []string
		if rel != "." {
			parts = strings.Split(filepath.ToSlash(rel), "/")
		}
		for i, part := range parts {
			if part == "dotgit" {
				parts[i] = ".git"
			}
		}
		target := filepath.Join(append([]string{dst}, parts...)...)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	}))
	return filepath.Join(dst, ".git")
}

func TestTransport_Open_AdvertisementIsV2(t *testing.T) {
	gitdir := materializeRepoFixture(t, "empty")
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
	gitdir := materializeRepoFixture(t, "empty")

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
	gitdir := materializeRepoFixture(t, "empty")
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
	gitdir := materializeRepoFixture(t, "empty")
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

package server

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/hiddeco/go-ls-remote/internal/objstore"
	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/transport"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// materializeEmptyRepo copies the `empty` fixture from
// `testdata/repos/empty/` into a fresh `t.TempDir()`, renaming the
// committed `dotgit` component to `.git`. The rename mirrors the
// helper in `internal/objstore` (canonical Git refuses to track a
// path containing a literal `.git` component, see
// `path.c::is_dotgit_path`); duplicating it here keeps this test
// independent of the `objstore` package's test helpers.
func materializeEmptyRepo(t *testing.T) string {
	t.Helper()

	wd, err := os.Getwd()
	require.NoError(t, err)
	src := filepath.Join(wd, "..", "..", "testdata", "repos", "empty")
	info, err := os.Stat(src)
	require.NoError(t, err, "fixture missing; regenerate with testdata/_gen/repos.sh")
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
		parts := splitPath(rel)
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

func splitPath(p string) []string {
	if p == "." {
		return nil
	}
	var parts []string
	for {
		dir, file := filepath.Split(p)
		if file != "" {
			parts = append([]string{file}, parts...)
		}
		if dir == "" {
			break
		}
		p = filepath.Clean(dir)
		if p == "." || p == string(filepath.Separator) {
			break
		}
	}
	return parts
}

func openEmptyStore(t *testing.T) *objstore.Store {
	t.Helper()
	gitdir := materializeEmptyRepo(t)
	s, err := objstore.Open(gitdir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestServe_V2EmitsVersionThenFlush pins the minimum behaviour of the
// Task 1 skeleton: when invoked with `transport.ProtocolV2`, the
// server's first emitted data packet starts with `version 2\n` and
// the stream eventually flushes. The full cap-bearing advertisement
// is byte-pinned by a later task.
func TestServe_V2EmitsVersionThenFlush(t *testing.T) {
	store := openEmptyStore(t)

	// Server reads from clientToServer, writes to serverToClient.
	// The test reads serverToClient (what the client would see) and
	// writes to clientToServer if needed (Task 1 does not consume
	// any client input).
	clientToServer, _ := io.Pipe()
	pr, pw := io.Pipe()

	r := pktline.NewReader(clientToServer)
	w := pktline.NewWriter(pw)

	errCh := make(chan error, 1)
	go func() {
		err := Serve(context.Background(), r, w, store, Options{
			Agent:             "test-agent/0.0",
			PreferredProtocol: transport.ProtocolV2,
		})
		_ = pw.Close()
		errCh <- err
	}()

	cr := pktline.NewReader(pr)

	first, err := cr.ReadPacket()
	require.NoError(t, err)
	require.Equal(t, pktline.Data, first.Kind)
	assert.Equal(t, "version 2\n", string(first.Data))

	// Drain until we see a flush. There may be intervening data
	// packets in later tasks; for Task 1 the next packet should be
	// the flush directly, but we tolerate intervening data so this
	// test does not regress when caps are added in Task 2.
	sawFlush := false
	for i := 0; i < 64; i++ {
		p, err := cr.ReadPacket()
		if err != nil {
			break
		}
		if p.Kind == pktline.Flush {
			sawFlush = true
			break
		}
	}
	assert.True(t, sawFlush, "expected flush packet after version line")

	require.NoError(t, <-errCh)
}

// TestServe_V0EmitsFlush pins the v0 skeleton's behaviour: with
// `transport.ProtocolV0` the server emits a flush — the empty
// advertisement Task 3 will replace with the empty-repo placeholder
// plus a proper ref list.
func TestServe_V0EmitsFlush(t *testing.T) {
	store := openEmptyStore(t)

	clientToServer, _ := io.Pipe()
	pr, pw := io.Pipe()

	r := pktline.NewReader(clientToServer)
	w := pktline.NewWriter(pw)

	errCh := make(chan error, 1)
	go func() {
		err := Serve(context.Background(), r, w, store, Options{
			Agent:             "test-agent/0.0",
			PreferredProtocol: transport.ProtocolV0,
		})
		_ = pw.Close()
		errCh <- err
	}()

	cr := pktline.NewReader(pr)

	p, err := cr.ReadPacket()
	require.NoError(t, err)
	assert.Equal(t, pktline.Flush, p.Kind)

	require.NoError(t, <-errCh)
}

// TestServe_UnknownProtocolReturnsError pins the contract for any
// `PreferredProtocol` value outside the supported set: `Serve` must
// surface an error rather than emit silence.
func TestServe_UnknownProtocolReturnsError(t *testing.T) {
	store := openEmptyStore(t)

	clientToServer, _ := io.Pipe()
	pr, pw := io.Pipe()

	r := pktline.NewReader(clientToServer)
	w := pktline.NewWriter(pw)

	errCh := make(chan error, 1)
	go func() {
		err := Serve(context.Background(), r, w, store, Options{
			Agent:             "test-agent/0.0",
			PreferredProtocol: transport.ProtocolVersion(99),
		})
		_ = pw.Close()
		errCh <- err
	}()

	// Drain the writer side so the goroutine's Close does not block
	// any in-flight write attempts, then assert the error.
	go func() {
		buf := make([]byte, 1024)
		for {
			if _, err := pr.Read(buf); err != nil {
				return
			}
		}
	}()

	err := <-errCh
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrUnsupportedProtocol),
		"want errors.Is(err, ErrUnsupportedProtocol); got %v", err)
}

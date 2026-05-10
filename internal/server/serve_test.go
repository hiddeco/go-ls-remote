package server

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/hiddeco/go-ls-remote/internal/objstore"
	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/transport"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// openEmptyStore materialises the `empty` fixture from
// `testdata/repos/empty/` into a fresh `t.TempDir()` and opens a
// store rooted at it. The majority of tests in this package only
// need the empty repository, so the helper hides the
// fixture-name plumbing and the cleanup wiring at every call site.
func openEmptyStore(t *testing.T) *objstore.Store {
	t.Helper()
	gitdir := materializeRepoFixture(t, "empty")
	s, err := objstore.Open(gitdir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
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

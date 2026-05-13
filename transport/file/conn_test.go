package filet

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/hiddeco/go-ls-remote/transport"
)

// TestConn_Close_NoGoroutineLeaks pins the goroutine-shutdown
// contract for [Conn.Close]: the single server goroutine that
// [Transport.Open] spawns must exit before the second [Conn.Close]
// returns. Wrapping the call sequence in `goleak.VerifyNone` turns
// a leaked server goroutine into a per-test failure; the
// package-wide `goleak.VerifyTestMain` only catches the leak at
// teardown, which obscures the failing call site.
//
// The [transport.Conn] is constructed via the same fixture helpers
// the rest of the package uses so the goroutine reaches its real
// shutdown path (cancel → pipe-close → `<-c.done`), not a
// short-circuit on an uninitialised struct.
//
//nolint:paralleltest // goleak.VerifyNone snapshots live goroutines; parallel siblings register as unexpected stacks.
func TestConn_Close_NoGoroutineLeaks(t *testing.T) {
	defer goleak.VerifyNone(t)

	gitdir := materializeServeableFixture(t, "empty")
	u, err := transport.ParseURL("file://" + gitdir)
	require.NoError(t, err)

	tr := New()
	conn, err := tr.Open(t.Context(), u, transport.OpenOptions{
		UserAgent: "close-leak-test/0.0",
	})
	require.NoError(t, err)

	require.NoError(t, conn.Close(),
		"first Close returns nil on a clean shutdown")
	require.NoError(t, conn.Close(),
		"second Close is a no-op returning nil; goleak verifies the goroutine left")
}

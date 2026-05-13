package ssht

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestConn_Close_Idempotent verifies the [transport.Conn] idempotent
// contract: a second [Conn.Close] is a no-op returning nil even when
// the underlying SSH session and client have already been torn down.
//
// The Conn is constructed via the in-process SSH test server so the
// teardown reaches the real `*ssh.Session.Close` / `*ssh.Client.Close`
// path; a plain struct-literal Conn would let the body short-circuit
// before exercising those calls.
func TestConn_Close_Idempotent(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, testServerOpts{acceptEnv: true, advertisement: []byte("0000")})

	tr := New(
		WithAuth(Signer(srv.clientSigner)),
		WithKnownHosts(srv.hostKeyCallback()),
	)
	conn, err := tr.Open(t.Context(), srv.URL(), defaultOpenOptions())
	require.NoError(t, err)

	require.NoError(t, conn.Close(), "first Close returns the captured teardown error (nil here)")
	require.NoError(t, conn.Close(), "second Close is a no-op returning nil")
	require.NoError(t, conn.Close(), "third Close still nil; idempotency is total")
}

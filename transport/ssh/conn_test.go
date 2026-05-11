package ssht

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hiddeco/go-ls-remote/pktline"
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
	srv := newTestServer(t, testServerOpts{acceptEnv: true, advertisement: []byte("0000")})

	tr := New(
		WithAuth(Signer(srv.clientSigner)),
		WithKnownHosts(srv.hostKeyCallback()),
	)
	conn, err := tr.Open(context.Background(), srv.URL(), defaultOpenOptions())
	require.NoError(t, err)

	require.NoError(t, conn.Close(), "first Close returns the captured teardown error (nil here)")
	require.NoError(t, conn.Close(), "second Close is a no-op returning nil")
	require.NoError(t, conn.Close(), "third Close still nil; idempotency is total")
}

// TestConn_Command_Stub verifies that until Task 4 wires the v2 path,
// [Conn.Command] returns an error advertising the stub rather than
// panicking or returning a bogus reader.
func TestConn_Command_Stub(t *testing.T) {
	srv := newTestServer(t, testServerOpts{acceptEnv: true, advertisement: []byte("0000")})

	tr := New(
		WithAuth(Signer(srv.clientSigner)),
		WithKnownHosts(srv.hostKeyCallback()),
	)
	conn, err := tr.Open(context.Background(), srv.URL(), defaultOpenOptions())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	r, err := conn.Command(context.Background(), "ls-refs", func(_ *pktline.Writer) error { return nil })
	assert.Nil(t, r)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not implemented",
		"Command stub must surface a 'not implemented' diagnostic until Task 4 wires the v2 path")
}

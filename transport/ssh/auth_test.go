package ssht

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// newEd25519Signer generates a fresh ed25519 keypair and returns both the
// raw private key (for marshalling to PEM in [TestKeyFile]) and an
// [ssh.Signer] (for [TestSigner] and the agent keyring in [TestAgent]).
func newEd25519Signer(t *testing.T) (ed25519.PrivateKey, ssh.Signer) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer, err := ssh.NewSignerFromKey(priv)
	require.NoError(t, err)
	return priv, signer
}

// startAgentServer stands up an in-process ssh-agent listening on a Unix
// socket under t.TempDir(). It serves accepted connections from an
// [agent.Keyring] preloaded with priv. The accept loop and every
// per-connection serve goroutine are tracked with a [sync.WaitGroup] and
// joined on cleanup; the listener is closed first to break the accept
// loop, then every still-open client connection is closed to make
// [agent.ServeAgent] return EOF, then the test waits on the group. This
// shape is what `go.uber.org/goleak`'s `VerifyTestMain` in
// [main_test.go] requires of the package.
func startAgentServer(t *testing.T, priv ed25519.PrivateKey) string {
	t.Helper()

	sockPath := filepath.Join(t.TempDir(), "agent.sock")
	ln, err := net.Listen("unix", sockPath)
	require.NoError(t, err)

	keyring := agent.NewKeyring()
	require.NoError(t, keyring.Add(agent.AddedKey{PrivateKey: priv}))

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		conns []net.Conn
	)

	wg.Go(func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
			wg.Go(func() {
				defer func() { _ = conn.Close() }()
				_ = agent.ServeAgent(keyring, conn)
			})
		}
	})

	t.Cleanup(func() {
		// Close the listener first so the accept loop exits.
		_ = ln.Close()
		// Close every accepted client conn so any in-flight
		// `agent.ServeAgent` reader returns EOF and its goroutine
		// can exit. Conn.Close is safe to call concurrently with
		// the deferred close inside the serve goroutine.
		mu.Lock()
		for _, c := range conns {
			_ = c.Close()
		}
		mu.Unlock()
		wg.Wait()
	})

	return sockPath
}

func TestAgent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("ssh-agent Unix-socket convention is Unix-only; Windows uses a named pipe")
	}

	priv, _ := newEd25519Signer(t)
	sockPath := startAgentServer(t, priv)

	t.Setenv("SSH_AUTH_SOCK", sockPath)

	methods, cleanup, err := Agent().Resolve(context.Background(), "example.com")
	require.NoError(t, err)
	require.NotNil(t, cleanup, "Agent must return a non-nil cleanup hook: the resolver holds an open Unix-socket conn to the agent")
	t.Cleanup(func() { _ = cleanup() })
	require.Len(t, methods, 1, "Agent should expose a single publickey AuthMethod backed by the agent's signers")
	assert.NotNil(t, methods[0])
}

func TestAgent_NoSocketEnv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("ssh-agent Unix-socket convention is Unix-only; Windows uses a named pipe")
	}

	// Force the env var to empty so the resolver sees no agent.
	t.Setenv("SSH_AUTH_SOCK", "")

	methods, cleanup, err := Agent().Resolve(context.Background(), "example.com")
	require.Error(t, err, "Agent must error rather than silently return an empty AuthMethod slice when SSH_AUTH_SOCK is unset")
	assert.Nil(t, methods)
	assert.Nil(t, cleanup, "cleanup must be nil on the error path: no resource was acquired")
}

func TestAgent_DialFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("ssh-agent Unix-socket convention is Unix-only; Windows uses a named pipe")
	}

	// Point at a path that does not exist; the Unix dial must fail.
	missing := filepath.Join(t.TempDir(), "does-not-exist.sock")
	t.Setenv("SSH_AUTH_SOCK", missing)

	methods, cleanup, err := Agent().Resolve(context.Background(), "example.com")
	require.Error(t, err, "Agent must surface dial errors, not swallow them")
	assert.Nil(t, methods)
	assert.Nil(t, cleanup, "cleanup must be nil on the error path: no resource was acquired")
}

func TestKeyFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("filesystem and key handling exercised here matches the package's Unix-only scope")
	}

	priv, _ := newEd25519Signer(t)

	block, err := ssh.MarshalPrivateKey(priv, "")
	require.NoError(t, err)
	keyBytes := pem.EncodeToMemory(block)
	require.NotEmpty(t, keyBytes)

	path := filepath.Join(t.TempDir(), "id_ed25519")
	require.NoError(t, os.WriteFile(path, keyBytes, 0o600))

	methods, cleanup, err := KeyFile(path, "").Resolve(context.Background(), "example.com")
	require.NoError(t, err)
	assert.Nil(t, cleanup, "KeyFile holds no resources past the parse; cleanup must be nil")
	require.Len(t, methods, 1, "KeyFile should expose a single publickey AuthMethod")
	assert.NotNil(t, methods[0])
}

func TestKeyFile_Missing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("filesystem semantics here are Unix-only by design for this package")
	}

	missing := filepath.Join(t.TempDir(), "absent")

	methods, cleanup, err := KeyFile(missing, "").Resolve(context.Background(), "example.com")
	require.Error(t, err)
	assert.Nil(t, methods)
	assert.Nil(t, cleanup, "cleanup must be nil on the error path: no resource was acquired")
	assert.ErrorIs(t, err, os.ErrNotExist, "missing-file error must wrap os.ErrNotExist so callers can branch on it")
}

func TestKeyFile_Malformed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("filesystem semantics here are Unix-only by design for this package")
	}

	path := filepath.Join(t.TempDir(), "garbage")
	require.NoError(t, os.WriteFile(path, []byte("not a key"), 0o600))

	methods, cleanup, err := KeyFile(path, "").Resolve(context.Background(), "example.com")
	require.Error(t, err)
	assert.Nil(t, methods)
	assert.Nil(t, cleanup, "cleanup must be nil on the error path: no resource was acquired")
	// The wrapped error must communicate a parse failure. We don't pin
	// the exact substring of x/crypto/ssh's error text but we do require
	// the wrapping verb's contribution so callers see the package prefix.
	assert.Contains(t, err.Error(), "parse", "malformed-key error should mention parse failure")
	// And it must not look like a missing-file error to callers
	// branching on os.ErrNotExist.
	assert.False(t, errors.Is(err, os.ErrNotExist))
}

func TestSigner(t *testing.T) {
	_, signer := newEd25519Signer(t)

	methods, cleanup, err := Signer(signer).Resolve(context.Background(), "example.com")
	require.NoError(t, err)
	assert.Nil(t, cleanup, "Signer holds no resources beyond the signer itself; cleanup must be nil")
	require.Len(t, methods, 1, "Signer should expose a single publickey AuthMethod backed by the provided signer")
	assert.NotNil(t, methods[0])
}

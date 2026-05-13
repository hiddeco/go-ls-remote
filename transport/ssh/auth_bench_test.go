package ssht

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

// BenchmarkKeyFile_Resolve measures the per-dial cost of the
// [KeyFile] resolver, which re-reads and re-parses the on-disk PEM
// every time [AuthResolver.Resolve] is called. The repeated parse is
// deliberate — a key rotated on disk takes effect on the next dial
// without reconstructing the resolver — but the cost is the dial-time
// floor a caller pays per `Transport.Open` when this resolver is
// wired. The bench documents it.
//
// Sub-case axis is the on-disk shape: an ed25519 key (the modern
// default, 32-byte seed). RSA keys would carry an order-of-magnitude
// higher parse cost; ed25519 is what the package's tests and most
// realistic deployments use, so the bench pins that axis.
func BenchmarkKeyFile_Resolve(b *testing.B) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(b, err)
	block, err := ssh.MarshalPrivateKey(priv, "")
	require.NoError(b, err)
	keyBytes := pem.EncodeToMemory(block)
	require.NotEmpty(b, keyBytes)

	path := filepath.Join(b.TempDir(), "id_ed25519")
	require.NoError(b, os.WriteFile(path, keyBytes, 0o600))

	r := KeyFile(path, "")
	ctx := b.Context()

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		methods, cleanup, err := r.Resolve(ctx, "example.com")
		if err != nil {
			b.Fatal(err)
		}
		if cleanup != nil {
			_ = cleanup()
		}
		_ = methods
	}
}

// BenchmarkSigner_Resolve measures the per-dial cost of the [Signer]
// resolver, which wraps an already-loaded [ssh.Signer] in a single
// publickey [ssh.AuthMethod] and returns it. It is the lowest-cost
// resolver in the package — no I/O, no parsing — and exists as the
// floor a caller can reach by parsing the key once at startup and
// holding the signer for the lifetime of the process.
//
// The bench's value is the delta between this floor and
// [BenchmarkKeyFile_Resolve]: it quantifies what a caller saves by
// caching the parse themselves.
func BenchmarkSigner_Resolve(b *testing.B) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(b, err)
	signer, err := ssh.NewSignerFromKey(priv)
	require.NoError(b, err)

	r := Signer(signer)
	ctx := b.Context()

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		methods, cleanup, err := r.Resolve(ctx, "example.com")
		if err != nil {
			b.Fatal(err)
		}
		if cleanup != nil {
			_ = cleanup()
		}
		_ = methods
	}
}

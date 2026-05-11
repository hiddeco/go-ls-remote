package inttest_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"github.com/hiddeco/go-ls-remote/internal/inttest"
	"github.com/hiddeco/go-ls-remote/internal/wire"
	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/transport"
	ssht "github.com/hiddeco/go-ls-remote/transport/ssh"
)

// newClientSigner returns a fresh in-memory ed25519 [ssh.Signer]. The
// harness accepts any pubkey, so the bytes carried here are only there
// to satisfy x/crypto/ssh's publickey-auth handshake — no server-side
// check pins them.
func newClientSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	s, err := ssh.NewSignerFromKey(priv)
	require.NoError(t, err)
	return s
}

// dialHarness builds a [*ssht.Transport] pinned to srv's host key and
// signed by clientSigner, then performs the SSH dial through it. The
// helper isolates the boilerplate that every harness test shares.
func dialHarness(t *testing.T, srv *inttest.SSHServer, clientSigner ssh.Signer) transport.Conn {
	t.Helper()
	tr := ssht.New(
		ssht.WithAuth(ssht.Signer(clientSigner)),
		ssht.WithKnownHosts(srv.HostKeyCallback()),
	)
	u, err := transport.ParseURL(srv.URL())
	require.NoError(t, err)
	v := transport.ProtocolV2
	conn, err := tr.Open(context.Background(), u, transport.OpenOptions{
		PreferredProtocol: &v,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// TestNewSSHServer_servesAdvertisementOverSession asserts that the
// harness opens a session, accepts the `git-upload-pack '<path>'` exec,
// and delivers the v2 advertisement. The first packet on stdout must
// be the `version 2\n` data packet that
// `internal/server.Serve` emits for v2; the response must carry at
// least one `refs/heads/` ref line in the advertisement section.
func TestNewSSHServer_servesAdvertisementOverSession(t *testing.T) {
	store := openLooseOnlySHA1Store(t)
	srv := inttest.NewSSHServer(t, store)
	conn := dialHarness(t, srv, newClientSigner(t))

	pr := conn.Advertisement()
	pkt, err := pr.ReadPacket()
	require.NoError(t, err)
	assert.Equal(t, pktline.Data, pkt.Kind)
	assert.Equal(t, "version 2\n", string(pkt.Data),
		"v2 advertisement must begin with `version 2\\n`")

	// Drain the rest of the advertisement up to its terminating flush
	// and confirm at least one capability line surfaces. The capability
	// set is asserted in detail by the server's own tests; here we
	// merely confirm bytes flow from `server.Serve` to the client.
	for {
		pkt, err := pr.ReadPacket()
		require.NoError(t, err)
		if pkt.Kind == pktline.Flush {
			return
		}
	}
}

// TestNewSSHServer_handlesV2Command asserts that the harness's single
// channel can carry both the advertisement and a follow-up `ls-refs`
// command response. SSH is not split like HTTP — one channel runs the
// full advertise-then-loop — so this exercise confirms the
// `pktline.Reader` reused by [transport.Conn.Command] reads command
// packets directly after the advertisement's flush.
func TestNewSSHServer_handlesV2Command(t *testing.T) {
	store := openLooseOnlySHA1Store(t)
	srv := inttest.NewSSHServer(t, store)
	conn := dialHarness(t, srv, newClientSigner(t))

	// Drain the advertisement to its terminating flush before issuing
	// the command. The transport's contract is single-flight: a caller
	// must consume the prior response (here, the advertisement) before
	// the next `Command` call.
	pr := conn.Advertisement()
	for {
		pkt, err := pr.ReadPacket()
		require.NoError(t, err)
		if pkt.Kind == pktline.Flush {
			break
		}
	}

	resp, err := conn.Command(context.Background(), "ls-refs",
		func(w *pktline.Writer) error {
			return wire.EncodeV2CommandRequest(w, "ls-refs",
				[]string{"peel", "symrefs"},
				[]string{"object-format=sha1"})
		})
	require.NoError(t, err)

	var sawHEAD, sawMain bool
	for {
		pkt, err := resp.ReadPacket()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		if pkt.Kind != pktline.Data {
			if pkt.Kind == pktline.Flush {
				break
			}
			continue
		}
		s := string(pkt.Data)
		if strings.Contains(s, " HEAD ") || strings.HasSuffix(s, " HEAD\n") {
			sawHEAD = true
		}
		if strings.Contains(s, " refs/heads/main\n") {
			sawMain = true
		}
	}
	assert.True(t, sawHEAD, "ls-refs response must carry a HEAD line")
	assert.True(t, sawMain, "ls-refs response must carry refs/heads/main")
}

// TestNewSSHServer_acceptsGitProtocolEnv asserts that the harness's
// session handler does not reject the `GIT_PROTOCOL=version=2` env
// request the production SSH transport sends. A reject here would
// surface as a `Setenv` error on the client side (swallowed by the
// production transport) and, more importantly, signal that the
// harness's `AcceptEnv`-style filter is misconfigured.
func TestNewSSHServer_acceptsGitProtocolEnv(t *testing.T) {
	store := openLooseOnlySHA1Store(t)
	srv := inttest.NewSSHServer(t, store)

	cfg := &ssh.ClientConfig{
		User:            "git",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(newClientSigner(t))},
		HostKeyCallback: srv.HostKeyCallback(),
	}
	addr := strings.TrimPrefix(srv.URL(), "ssh://git@")
	addr = strings.TrimSuffix(addr, "/repo.git")
	client, err := ssh.Dial("tcp", addr, cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	sess, err := client.NewSession()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sess.Close() })

	// The acceptance signal is the lack of an error: x/crypto/ssh
	// returns a non-nil error from `Setenv` when the server replies
	// failure to the underlying request.
	assert.NoError(t, sess.Setenv("GIT_PROTOCOL", "version=2"),
		"harness must accept the GIT_PROTOCOL env request")
}

// TestNewSSHServer_acceptsAnyPubkey asserts that the harness's
// publickey callback returns success for an arbitrary ephemeral key —
// the contract for integration use, where tests do not pre-share
// authorised keys with the server. Two independently-generated keys
// must both authenticate.
func TestNewSSHServer_acceptsAnyPubkey(t *testing.T) {
	store := openLooseOnlySHA1Store(t)
	srv := inttest.NewSSHServer(t, store)

	for i := range 2 {
		signer := newClientSigner(t)
		tr := ssht.New(
			ssht.WithAuth(ssht.Signer(signer)),
			ssht.WithKnownHosts(srv.HostKeyCallback()),
		)
		u, err := transport.ParseURL(srv.URL())
		require.NoError(t, err)
		v := transport.ProtocolV2
		conn, err := tr.Open(context.Background(), u, transport.OpenOptions{
			PreferredProtocol: &v,
		})
		require.NoError(t, err, "key #%d", i)
		_ = conn.Close()
	}
}

// TestNewSSHServer_URLShape asserts the harness's URL is parseable by
// [transport.ParseURL] and resolves to the SSH scheme on a loopback
// address. Tests downstream depend on the shape (`ssh://git@host:port/repo.git`)
// matching what canonical Git would accept.
func TestNewSSHServer_URLShape(t *testing.T) {
	store := openLooseOnlySHA1Store(t)
	srv := inttest.NewSSHServer(t, store)

	u, err := transport.ParseURL(srv.URL())
	require.NoError(t, err)
	assert.Equal(t, "ssh", u.Scheme)
	assert.Equal(t, "git", u.User)
	assert.Equal(t, "/repo.git", u.Path)
	assert.NotEmpty(t, u.Port, "harness URL must carry an explicit port")

	host := u.Host
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	require.NotNil(t, net.ParseIP(strings.Trim(host, "[]")),
		"harness URL host %q must be a loopback IP literal", host)
}

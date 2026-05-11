// Package ssht is the SSH Git transport. The directory is named
// `transport/ssh` for parity with canonical Git's source layout (see
// `connect.c` and `transport.c`), while the Go package is named `ssht`
// to avoid colliding with `golang.org/x/crypto/ssh` at the import site.
//
// SSH authentication is method-list-shaped, not header-shaped: an SSH
// client offers a sequence of `ssh.AuthMethod` values and the server
// chooses which to attempt (RFC 4252 §5). That shape is incompatible
// with the per-request `Credentials.Apply` seam the HTTP transport
// uses, so this package introduces its own [AuthResolver] interface
// alongside three built-in resolvers: [Agent] (delegate to a running
// ssh-agent), [KeyFile] (read a PEM-encoded private key from disk on
// every dial), and [Signer] (wrap a single in-memory [ssh.Signer]).
package ssht

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// AuthResolver supplies SSH authentication methods for a given host.
// Resolvers are consulted once per dial. A nil resolver means
// "no authentication" — the transport will only succeed against
// servers permitting anonymous access (rare in practice).
//
// The returned []ssh.AuthMethod is supplied to ssh.ClientConfig.Auth.
//
// Some resolvers hold open resources for the duration of the SSH auth
// handshake — most notably [Agent], whose signer delegates `Sign()`
// calls to a Unix-socket connection to the running ssh-agent. The
// connection must outlive the handshake but must not leak past it, so
// `Resolve` returns an optional cleanup hook the transport invokes
// once the SSH client has finished consuming the methods (typically
// after a successful `ssh.Dial` or after teardown of a partial dial).
// `cleanup` may be nil when the resolver holds no resources requiring
// release; callers must invoke it exactly once when non-nil.
type AuthResolver interface {
	// Resolve returns the AuthMethods to offer when dialing host, plus
	// an optional cleanup hook (see the interface comment). ctx may be
	// honoured by resolvers that perform I/O (e.g. a future resolver
	// that fetches a key from a remote secret store). host is the
	// hostname the transport is about to dial; resolvers typically
	// ignore it but it is supplied so per-host policies can be
	// expressed without writing a separate resolver per host.
	Resolve(ctx context.Context, host string) (methods []ssh.AuthMethod, cleanup func() error, err error)
}

// Agent returns an [AuthResolver] that connects to the ssh-agent at the
// path named by `$SSH_AUTH_SOCK` and supplies the agent's signers as a
// single publickey [ssh.AuthMethod].
//
// The dial is Unix-socket-only: on Windows the OpenSSH agent listens on
// a named pipe (`\\.\pipe\openssh-ssh-agent`) rather than a Unix
// socket, so `Agent` cannot reach the canonical Windows agent. The
// constructor itself is portable Go and the runtime `net.Dial("unix",
// ...)` is a no-op error path on Windows when `$SSH_AUTH_SOCK` is
// unset, which is the typical Windows state; Windows callers needing
// ssh-agent semantics should wire a named-pipe resolver of their own
// via [AuthResolver] or supply an [ssh.Signer] via [Signer].
//
// If `$SSH_AUTH_SOCK` is unset or the dial fails the returned resolver
// surfaces a wrapped error from its `Resolve` method; it never silently
// returns an empty slice.
//
// The cleanup hook returned alongside the methods closes the Unix-socket
// connection to the agent. The connection must remain open until the
// SSH client has finished its publickey exchange — agent-backed signers
// delegate `Sign()` over this socket — so callers invoke cleanup only
// after `ssh.Dial` returns (success or failure).
func Agent() AuthResolver {
	return agentResolver{}
}

type agentResolver struct{}

// Resolve dials `$SSH_AUTH_SOCK` and offers the agent's signers as a
// publickey method. The dial happens on every call so an agent that is
// restarted between dials is picked up on the next attempt. The
// returned cleanup hook closes the socket connection.
func (agentResolver) Resolve(_ context.Context, _ string) ([]ssh.AuthMethod, func() error, error) {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil, nil, errors.New("ssht: SSH_AUTH_SOCK is unset; no ssh-agent to query")
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return nil, nil, fmt.Errorf("ssht: dial ssh-agent at %q: %w", sock, err)
	}
	client := agent.NewClient(conn)
	cleanup := func() error { return conn.Close() }
	return []ssh.AuthMethod{ssh.PublicKeysCallback(client.Signers)}, cleanup, nil
}

// KeyFile returns an [AuthResolver] that reads the PEM-encoded private
// key at path and parses it on every `Resolve` call. If passphrase is
// non-empty the key is decrypted with [ssh.ParsePrivateKeyWithPassphrase];
// otherwise [ssh.ParsePrivateKey] is used.
//
// Re-reading on each call is deliberate: a key rotated on disk takes
// effect on the next dial without reconstructing the resolver. The cost
// of a single file read and key parse per dial is negligible compared
// to the SSH handshake itself.
//
// Missing-file errors wrap [os.ErrNotExist] so callers can branch with
// [errors.Is]. Parse failures surface the underlying x/crypto/ssh
// error wrapped with a package prefix that mentions "parse".
//
// `KeyFile` holds no resources past the parse, so its `Resolve` returns
// a nil cleanup hook.
func KeyFile(path, passphrase string) AuthResolver {
	return keyFileResolver{path: path, passphrase: passphrase}
}

type keyFileResolver struct {
	path       string
	passphrase string
}

// Resolve reads the key file and parses it on every call. The returned
// cleanup hook is nil — no resources need releasing.
func (k keyFileResolver) Resolve(_ context.Context, _ string) ([]ssh.AuthMethod, func() error, error) {
	data, err := os.ReadFile(k.path)
	if err != nil {
		return nil, nil, fmt.Errorf("ssht: read key file %q: %w", k.path, err)
	}
	var signer ssh.Signer
	if k.passphrase != "" {
		signer, err = ssh.ParsePrivateKeyWithPassphrase(data, []byte(k.passphrase))
	} else {
		signer, err = ssh.ParsePrivateKey(data)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("ssht: parse key file %q: %w", k.path, err)
	}
	return []ssh.AuthMethod{ssh.PublicKeys(signer)}, nil, nil
}

// Signer returns an [AuthResolver] that supplies a single, already-loaded
// [ssh.Signer] as a publickey [ssh.AuthMethod]. This is the lowest-level
// resolver: callers holding a raw private key in memory can wrap it
// with [ssh.NewSignerFromKey] and pass the result through.
//
// `Signer` holds no resources beyond the signer itself, so its
// `Resolve` returns a nil cleanup hook.
func Signer(s ssh.Signer) AuthResolver {
	return signerResolver{s: s}
}

type signerResolver struct {
	s ssh.Signer
}

// Resolve returns the wrapped signer as the sole publickey method. The
// returned cleanup hook is nil — no resources need releasing.
func (r signerResolver) Resolve(_ context.Context, _ string) ([]ssh.AuthMethod, func() error, error) {
	return []ssh.AuthMethod{ssh.PublicKeys(r.s)}, nil, nil
}

package ssht

import (
	"context"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

func TestNew_zeroValue(t *testing.T) {
	tr := New()
	require.NotNil(t, tr)

	assert.Nil(t, tr.auth, "auth is nil; no AuthResolver configured")
	assert.Nil(t, tr.clientCfg, "clientCfg is nil; no caller-supplied ssh.ClientConfig")
	assert.Nil(t, tr.hostKey, "hostKey is nil; no host-key callback configured")
	assert.Nil(t, tr.dialer, "dialer is nil; resolved to default at dial time")
}

func TestTransport_Schemes(t *testing.T) {
	tr := New()
	assert.Equal(t, []string{"ssh"}, tr.Schemes())
}

func TestWithAuth(t *testing.T) {
	want := authResolverFunc(func(_ context.Context, _ string) ([]ssh.AuthMethod, func() error, error) {
		return nil, nil, nil
	})

	tr := New(WithAuth(want))

	require.NotNil(t, tr.auth, "WithAuth stores the AuthResolver")
	// Compare by invoking — funcs can't be compared with ==, but the
	// resolver shape is observable through Resolve.
	methods, cleanup, err := tr.auth.Resolve(context.Background(), "example.com")
	require.NoError(t, err)
	assert.Nil(t, methods)
	assert.Nil(t, cleanup)
}

func TestWithAuth_nilPermitted(t *testing.T) {
	tr := New(WithAuth(nil))
	assert.Nil(t, tr.auth, "WithAuth(nil) means anonymous auth")
}

func TestWithClientConfig(t *testing.T) {
	want := &ssh.ClientConfig{User: "git"}
	tr := New(WithClientConfig(want))

	assert.Same(t, want, tr.clientCfg, "WithClientConfig stores the *ssh.ClientConfig verbatim")
}

func TestWithClientConfig_nilPermitted(t *testing.T) {
	tr := New(WithClientConfig(nil))
	assert.Nil(t, tr.clientCfg)
}

func TestWithKnownHosts(t *testing.T) {
	calls := 0
	want := ssh.HostKeyCallback(func(_ string, _ net.Addr, _ ssh.PublicKey) error {
		calls++
		return nil
	})

	tr := New(WithKnownHosts(want))

	require.NotNil(t, tr.hostKey, "WithKnownHosts stores the callback")
	err := tr.hostKey("example.com:22", nil, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, calls, "stored callback is the one passed in")
}

func TestWithKnownHosts_nilPermitted(t *testing.T) {
	tr := New(WithKnownHosts(nil))
	assert.Nil(t, tr.hostKey)
}

func TestWithDialer(t *testing.T) {
	want := &net.Dialer{}
	tr := New(WithDialer(want))

	assert.Same(t, want, tr.dialer, "WithDialer stores the *net.Dialer verbatim")
}

func TestWithDialer_nilPermitted(t *testing.T) {
	tr := New(WithDialer(nil))
	assert.Nil(t, tr.dialer)
}

func TestNew_nilOptionSkipped(t *testing.T) {
	resolver := authResolverFunc(func(_ context.Context, _ string) ([]ssh.AuthMethod, func() error, error) {
		return nil, nil, nil
	})

	// Mixing a nil Option with a real one must not panic and must
	// still apply the real one.
	tr := New(nil, WithAuth(resolver))
	require.NotNil(t, tr.auth, "non-nil option still applied alongside a nil entry")
}

func TestNew_multipleOptions(t *testing.T) {
	resolver := authResolverFunc(func(_ context.Context, _ string) ([]ssh.AuthMethod, func() error, error) {
		return nil, nil, nil
	})
	cfg := &ssh.ClientConfig{User: "git"}
	cb := ssh.HostKeyCallback(func(_ string, _ net.Addr, _ ssh.PublicKey) error { return nil })
	dialer := &net.Dialer{}

	tr := New(
		WithAuth(resolver),
		WithClientConfig(cfg),
		WithKnownHosts(cb),
		WithDialer(dialer),
	)

	assert.NotNil(t, tr.auth)
	assert.Same(t, cfg, tr.clientCfg)
	assert.NotNil(t, tr.hostKey)
	assert.Same(t, dialer, tr.dialer)
}

func TestNew_lastWins(t *testing.T) {
	first := &ssh.ClientConfig{User: "first"}
	second := &ssh.ClientConfig{User: "second"}

	tr := New(WithClientConfig(first), WithClientConfig(second))

	assert.Same(t, second, tr.clientCfg, "left-to-right last-write-wins; second WithClientConfig overrides first")

	firstDialer := &net.Dialer{Timeout: 1}
	secondDialer := &net.Dialer{Timeout: 2}
	tr = New(WithDialer(firstDialer), WithDialer(secondDialer))
	assert.Same(t, secondDialer, tr.dialer, "WithDialer also follows last-write-wins")
}

// authResolverFunc is a lightweight test adapter that turns a closure
// into an [AuthResolver]. It stays in the test file so the production
// package surface is not polluted.
type authResolverFunc func(ctx context.Context, host string) ([]ssh.AuthMethod, func() error, error)

func (f authResolverFunc) Resolve(ctx context.Context, host string) ([]ssh.AuthMethod, func() error, error) {
	return f(ctx, host)
}

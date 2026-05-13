//go:build live

package livetest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// github returns the curated GitHub entry from the package-level
// [Providers] slice. The helper centralises the lookup so individual
// sub-tests do not embed the slice index, which would fragilely couple
// the tests to provider ordering.
func github(t testing.TB) Provider {
	t.Helper()
	for _, p := range Providers {
		if p.Name == "github" {
			return p
		}
	}
	require.FailNow(t, "github provider missing from Providers")
	return Provider{}
}

// modeNames extracts the ordered sub-test names from a slice of
// [authMode] rows so assertions can compare on a stable shape without
// inspecting each row's options or URL.
func modeNames(modes []authMode) []string {
	names := make([]string, len(modes))
	for i, m := range modes {
		names[i] = m.name
	}
	return names
}

// clearProviderEnv unsets every env var any provider in [Providers]
// reads. `t.Setenv` only saves keys it has been called with, so an
// existing process-level value for an unrelated provider could otherwise
// leak into a sub-test's matrix shape.
func clearProviderEnv(t testing.TB) {
	t.Helper()
	for _, p := range Providers {
		for _, key := range []string{
			p.AuthTokenEnv,
			p.PrivateRepoEnv,
			p.SSHKeyEnv,
			p.SSHAgentEnv,
		} {
			t.Setenv(key, "")
			require.NoError(t, os.Unsetenv(key))
		}
	}
}

func TestProviders_curated(t *testing.T) {
	t.Parallel()
	// Lock the curated set so the surrounding sub-tests can rely on
	// the field shape per provider without re-deriving it.
	require.Len(t, Providers, 5)

	names := make([]string, len(Providers))
	for i, p := range Providers {
		names[i] = p.Name
	}
	assert.Equal(t, []string{
		"github", "gitlab", "codeberg", "bitbucket", "gitea",
	}, names)

	for _, p := range Providers {
		assert.NotEmpty(t, p.PublicHTTPS, "%s: PublicHTTPS", p.Name)
		assert.NotEmpty(t, p.PublicSSH, "%s: PublicSSH", p.Name)
		assert.NotEmpty(t, p.HTTPSBasicUser, "%s: HTTPSBasicUser", p.Name)
		assert.NotEmpty(t, p.AuthTokenEnv, "%s: AuthTokenEnv", p.Name)
		assert.NotEmpty(t, p.PrivateRepoEnv, "%s: PrivateRepoEnv", p.Name)
		assert.NotEmpty(t, p.SSHKeyEnv, "%s: SSHKeyEnv", p.Name)
		assert.NotEmpty(t, p.SSHAgentEnv, "%s: SSHAgentEnv", p.Name)
	}
}

func TestProvider_authModes_noEnv(t *testing.T) {
	clearProviderEnv(t)
	p := github(t)

	modes := p.authModes(t)

	require.Equal(t, []string{"none"}, modeNames(modes))
	assert.Equal(t, p.PublicHTTPS, modes[0].url)
	assert.Nil(t, modes[0].options)
}

func TestProvider_authModes_httpsToken(t *testing.T) {
	clearProviderEnv(t)
	p := github(t)

	t.Setenv(p.AuthTokenEnv, "ghp_dummy")

	modes := p.authModes(t)

	require.Equal(t, []string{"none", "https-token"}, modeNames(modes))
	assert.Equal(t, p.PublicHTTPS, modes[1].url)
	assert.NotEmpty(t, modes[1].options,
		"https-token row must carry a registry option")
}

func TestProvider_authModes_httpsTokenPrivate(t *testing.T) {
	clearProviderEnv(t)
	p := github(t)

	t.Setenv(p.AuthTokenEnv, "ghp_dummy")
	t.Setenv(p.PrivateRepoEnv, "https://github.example/private/repo.git")

	modes := p.authModes(t)

	require.Equal(t, []string{
		"none", "https-token", "https-token-private",
	}, modeNames(modes))
	assert.Equal(t, "https://github.example/private/repo.git", modes[2].url)
	assert.NotEmpty(t, modes[2].options,
		"https-token-private row must carry a registry option")
}

func TestProvider_authModes_privateRepoIgnoredWithoutToken(t *testing.T) {
	// A private-repo URL is unusable without the matching token, so
	// the row is suppressed in that partial-credential state rather
	// than yielding a row whose options carry no credentials.
	clearProviderEnv(t)
	p := github(t)

	t.Setenv(p.PrivateRepoEnv, "https://github.example/private/repo.git")

	modes := p.authModes(t)

	require.Equal(t, []string{"none"}, modeNames(modes))
}

func TestProvider_authModes_sshKey(t *testing.T) {
	clearProviderEnv(t)
	p := github(t)

	keyPath := filepath.Join(t.TempDir(), "id_test")
	require.NoError(t, os.WriteFile(keyPath, []byte("not a real key"), 0o600))
	t.Setenv(p.SSHKeyEnv, keyPath)

	modes := p.authModes(t)

	require.Equal(t, []string{"none", "ssh-key"}, modeNames(modes))
	assert.Equal(t, p.PublicSSH, modes[1].url)
	assert.NotEmpty(t, modes[1].options,
		"ssh-key row must carry a registry option")
}

func TestProvider_authModes_sshKeyMissingFile(t *testing.T) {
	// A configured key path that cannot be stat'd is logged and the
	// row is dropped; other modes continue to surface. The test
	// observes the drop rather than the log line, since `t.Logf`
	// output is not programmatically inspectable from inside the
	// same test, but `authModes` takes `testing.TB` precisely so the
	// hint can land in `-v` output for a human running the matrix.
	clearProviderEnv(t)
	p := github(t)

	t.Setenv(p.SSHKeyEnv, filepath.Join(t.TempDir(), "does-not-exist"))

	modes := p.authModes(t)

	require.Equal(t, []string{"none"}, modeNames(modes))
}

func TestProvider_authModes_sshAgent(t *testing.T) {
	clearProviderEnv(t)
	p := github(t)

	t.Setenv(p.SSHAgentEnv, "1")

	modes := p.authModes(t)

	require.Equal(t, []string{"none", "ssh-agent"}, modeNames(modes))
	assert.Equal(t, p.PublicSSH, modes[1].url)
	assert.NotEmpty(t, modes[1].options,
		"ssh-agent row must carry a registry option")
}

func TestProvider_authModes_sshAgentRequiresExactlyOne(t *testing.T) {
	// Any value other than `"1"` does not enable the agent row. The
	// strict check keeps the toggle predictable when the caller's
	// shell exports an unset variable as the empty string.
	clearProviderEnv(t)
	p := github(t)

	t.Setenv(p.SSHAgentEnv, "true")

	modes := p.authModes(t)

	require.Equal(t, []string{"none"}, modeNames(modes))
}

func TestProvider_authModes_combined(t *testing.T) {
	// A fully provisioned environment surfaces every row in the
	// declared order: anonymous, then HTTPS modes, then SSH modes.
	clearProviderEnv(t)
	p := github(t)

	keyPath := filepath.Join(t.TempDir(), "id_test")
	require.NoError(t, os.WriteFile(keyPath, []byte("not a real key"), 0o600))

	t.Setenv(p.AuthTokenEnv, "ghp_dummy")
	t.Setenv(p.PrivateRepoEnv, "https://github.example/private/repo.git")
	t.Setenv(p.SSHKeyEnv, keyPath)
	t.Setenv(p.SSHAgentEnv, "1")

	modes := p.authModes(t)

	assert.Equal(t, []string{
		"none",
		"https-token",
		"https-token-private",
		"ssh-key",
		"ssh-agent",
	}, modeNames(modes))
}

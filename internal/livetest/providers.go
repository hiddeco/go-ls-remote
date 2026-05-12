//go:build live

package livetest

// Provider describes a public Git hosting provider exercised by the
// live integration suite. The struct is data-only — the matrix-building
// logic lives on [Provider.authModes] and [Provider.skipIfOffline] in
// `matrix.go`.
type Provider struct {
	// Name is the short, lowercase identifier used in env-var
	// prefixes and `t.Run` sub-test names. It is stable across runs
	// so users can target a single provider with `-run github`.
	Name string

	// PublicHTTPS is the canonical HTTPS clone URL of a public
	// repository hosted on the provider. It is used for the
	// anonymous (`none`) row and, when an auth token is configured,
	// for the `https-token` row.
	PublicHTTPS string

	// PublicSSH is the SCP-form SSH URL for the same public
	// repository. It is used for the `ssh-key` and `ssh-agent` rows
	// when the matching env vars are populated. The library
	// normalises SCP-form URLs to `ssh://` internally; the literal
	// form is retained here because it matches what users paste from
	// provider web UIs.
	PublicSSH string

	// HTTPSBasicUser is the username placeholder the provider
	// expects in HTTP Basic credentials when authenticating with a
	// personal-access token. The token itself is supplied via the
	// [Provider.AuthTokenEnv] env var and used as the Basic
	// password.
	//
	// Common conventions:
	//   - GitHub:           `x-access-token`
	//   - GitLab:           `oauth2`
	//   - Codeberg / Gitea: any non-empty placeholder; `token` works
	//   - Bitbucket:        `x-token-auth` (workspace tokens)
	HTTPSBasicUser string

	// AuthTokenEnv names the env var holding the HTTPS
	// personal-access token for this provider, e.g.
	// `LSREMOTE_GITHUB_TOKEN`. When unset the `https-token` and
	// `https-token-private` rows are suppressed.
	AuthTokenEnv string

	// PrivateRepoEnv names the env var holding an HTTPS URL of a
	// token-accessible private repository, e.g.
	// `LSREMOTE_GITHUB_PRIVATE_REPO`. The `https-token-private` row
	// is added only when both [Provider.AuthTokenEnv] and this env
	// var are set.
	PrivateRepoEnv string

	// SSHKeyEnv names the env var holding the filesystem path of an
	// OpenSSH private key, e.g. `LSREMOTE_GITHUB_SSH_KEY`. When set
	// and the file is stat'able the `ssh-key` row is added; a
	// missing or unreadable file is logged via `t.Logf` and the row
	// is dropped without failing the test.
	SSHKeyEnv string

	// SSHAgentEnv names the env var that enables the `ssh-agent`
	// row when set to `"1"`. The agent socket is read from
	// `SSH_AUTH_SOCK` at dial time by the SSH transport.
	SSHAgentEnv string
}

// Providers is the curated list of public Git hosting providers
// exercised by the live integration tests. The set is intentionally
// minimal — surfacing protocol-level regressions, not enumerating
// every public Git host. Azure DevOps is intentionally omitted
// because its HTTPS endpoint requires a workflow incompatible with
// the rest of the matrix.
var Providers = []Provider{
	{
		Name:           "github",
		PublicHTTPS:    "https://github.com/golang/go",
		PublicSSH:      "git@github.com:golang/go.git",
		HTTPSBasicUser: "x-access-token",
		AuthTokenEnv:   "LSREMOTE_GITHUB_TOKEN",
		PrivateRepoEnv: "LSREMOTE_GITHUB_PRIVATE_REPO",
		SSHKeyEnv:      "LSREMOTE_GITHUB_SSH_KEY",
		SSHAgentEnv:    "LSREMOTE_GITHUB_SSH_AGENT",
	},
	{
		Name:           "gitlab",
		PublicHTTPS:    "https://gitlab.com/gitlab-org/gitlab.git",
		PublicSSH:      "git@gitlab.com:gitlab-org/gitlab.git",
		HTTPSBasicUser: "oauth2",
		AuthTokenEnv:   "LSREMOTE_GITLAB_TOKEN",
		PrivateRepoEnv: "LSREMOTE_GITLAB_PRIVATE_REPO",
		SSHKeyEnv:      "LSREMOTE_GITLAB_SSH_KEY",
		SSHAgentEnv:    "LSREMOTE_GITLAB_SSH_AGENT",
	},
	{
		Name:           "codeberg",
		PublicHTTPS:    "https://codeberg.org/forgejo/forgejo",
		PublicSSH:      "git@codeberg.org:forgejo/forgejo.git",
		HTTPSBasicUser: "token",
		AuthTokenEnv:   "LSREMOTE_CODEBERG_TOKEN",
		PrivateRepoEnv: "LSREMOTE_CODEBERG_PRIVATE_REPO",
		SSHKeyEnv:      "LSREMOTE_CODEBERG_SSH_KEY",
		SSHAgentEnv:    "LSREMOTE_CODEBERG_SSH_AGENT",
	},
	{
		Name:           "bitbucket",
		PublicHTTPS:    "https://bitbucket.org/snakeyaml/snakeyaml.git",
		PublicSSH:      "git@bitbucket.org:snakeyaml/snakeyaml.git",
		HTTPSBasicUser: "x-token-auth",
		AuthTokenEnv:   "LSREMOTE_BITBUCKET_TOKEN",
		PrivateRepoEnv: "LSREMOTE_BITBUCKET_PRIVATE_REPO",
		SSHKeyEnv:      "LSREMOTE_BITBUCKET_SSH_KEY",
		SSHAgentEnv:    "LSREMOTE_BITBUCKET_SSH_AGENT",
	},
	{
		Name:           "gitea",
		PublicHTTPS:    "https://gitea.com/gitea/tea",
		PublicSSH:      "git@gitea.com:gitea/tea.git",
		HTTPSBasicUser: "token",
		AuthTokenEnv:   "LSREMOTE_GITEA_TOKEN",
		PrivateRepoEnv: "LSREMOTE_GITEA_PRIVATE_REPO",
		SSHKeyEnv:      "LSREMOTE_GITEA_SSH_KEY",
		SSHAgentEnv:    "LSREMOTE_GITEA_SSH_AGENT",
	},
}

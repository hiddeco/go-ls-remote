//go:build live

package livetest

import (
	"context"
	"net"
	"net/url"
	"os"
	"testing"
	"time"

	lsremote "github.com/hiddeco/go-ls-remote"
	"github.com/hiddeco/go-ls-remote/transport"
	httpt "github.com/hiddeco/go-ls-remote/transport/http"
	ssht "github.com/hiddeco/go-ls-remote/transport/ssh"
	"golang.org/x/crypto/ssh"
)

// authMode is one row of the per-provider auth-mode matrix the live
// suite expands `t.Run` over. The fields together describe a single
// dial: which URL to hit and which [lsremote.Option] values to pass.
type authMode struct {
	// name is the sub-test name fed to `t.Run`, e.g. `none`,
	// `https-token`, `https-token-private`, `ssh-key`, `ssh-agent`.
	name string

	// url is the clone URL this mode targets. For `none` and
	// `https-token` it is [Provider.PublicHTTPS]; for
	// `https-token-private` it is the value of
	// [Provider.PrivateRepoEnv]; for the SSH modes it is
	// [Provider.PublicSSH].
	url string

	// options are the [lsremote.Option] values to pass to
	// [lsremote.Dial]. For `none` it is nil. For credentialed modes
	// it carries a [lsremote.WithTransports] option whose registry
	// has the relevant transport pre-wired with credentials.
	options []lsremote.Option
}

// authModes returns the dynamic matrix of auth modes available for p,
// given the current process environment. The `none` row is always
// present; credentialed rows are added when the matching env vars are
// populated.
//
// Partial-credential conditions — an SSH key path that does not
// exist, for example — log a hint via `t.Logf` and drop the offending
// row rather than failing the test. The signature accepts
// [testing.TB] so this kind of advisory output reaches the surrounding
// test's `-v` log without requiring the caller to thread a separate
// logger.
func (p Provider) authModes(t testing.TB) []authMode {
	t.Helper()

	modes := []authMode{{
		name:    "none",
		url:     p.PublicHTTPS,
		options: nil,
	}}

	if token := os.Getenv(p.AuthTokenEnv); token != "" {
		httpsOpt := lsremote.WithTransports(transport.NewRegistry(
			httpt.New(httpt.WithCredentials(
				httpt.Static(httpt.Basic(p.HTTPSBasicUser, token)),
			)),
		))
		modes = append(modes, authMode{
			name:    "https-token",
			url:     p.PublicHTTPS,
			options: []lsremote.Option{httpsOpt},
		})

		if privateURL := os.Getenv(p.PrivateRepoEnv); privateURL != "" {
			modes = append(modes, authMode{
				name:    "https-token-private",
				url:     privateURL,
				options: []lsremote.Option{httpsOpt},
			})
		}
	}

	if keyPath := os.Getenv(p.SSHKeyEnv); keyPath != "" {
		if _, err := os.Stat(keyPath); err != nil {
			t.Logf("livetest: %s ssh-key skipped: %v", p.Name, err)
		} else {
			sshOpt := lsremote.WithTransports(transport.NewRegistry(
				ssht.New(
					ssht.WithAuth(ssht.KeyFile(keyPath, "")),
					ssht.WithKnownHosts(insecureHostKeyCallback()),
				),
			))
			modes = append(modes, authMode{
				name:    "ssh-key",
				url:     p.PublicSSH,
				options: []lsremote.Option{sshOpt},
			})
		}
	}

	if os.Getenv(p.SSHAgentEnv) == "1" {
		sshOpt := lsremote.WithTransports(transport.NewRegistry(
			ssht.New(
				ssht.WithAuth(ssht.Agent()),
				ssht.WithKnownHosts(insecureHostKeyCallback()),
			),
		))
		modes = append(modes, authMode{
			name:    "ssh-agent",
			url:     p.PublicSSH,
			options: []lsremote.Option{sshOpt},
		})
	}

	return modes
}

// insecureHostKeyCallback returns a [ssh.HostKeyCallback] that accepts
// every host key. The live matrix dials well-known public Git hosts
// whose fingerprints rotate independently of this repository; pinning
// them here would turn a routine host-key rotation into a test
// failure. The trade-off is acceptable because the matrix exercises
// only public read-only endpoints — there is no credential or write
// access at stake.
func insecureHostKeyCallback() ssh.HostKeyCallback {
	return ssh.InsecureIgnoreHostKey()
}

// skipIfOffline performs a short TCP reachability probe against
// `p.PublicHTTPS`'s host on port 443 and calls `t.Skipf` when the
// dial fails. Tests in this package call it at the top of each test
// function so a hermetic CI run without network access skips cleanly
// instead of waiting on long protocol timeouts.
func (p Provider) skipIfOffline(t testing.TB) {
	t.Helper()
	u, err := url.Parse(p.PublicHTTPS)
	if err != nil {
		t.Skipf("livetest: %s: parse PublicHTTPS: %v", p.Name, err)
		return
	}
	host := u.Host
	if host == "" {
		t.Skipf("livetest: %s: PublicHTTPS has no host", p.Name)
		return
	}
	conn, err := net.DialTimeout("tcp", host+":443", 5*time.Second)
	if err != nil {
		t.Skipf("livetest: offline or %s unreachable: %v", host, err)
		return
	}
	_ = conn.Close()
}

// forEachProviderMode runs body for every (provider, auth-mode) cell in
// the live-test matrix. The outer [Providers] loop wraps each provider
// in `t.Run(p.Name, ...)` and calls [Provider.skipIfOffline] once at the
// top so an offline provider skips the whole subtree in one go. The
// inner loop iterates [Provider.authModes] under `t.Run(m.name, ...)`,
// bounding every cell with a 30-second [context.WithTimeout]. The
// resulting sub-test paths read e.g. `TestX/github/none` and
// `TestX/gitlab/ssh-key`, matching the matrix dimensions one-to-one so
// a `-run` filter can target a single cell.
//
// body receives the per-cell `*testing.T`, the live [Provider], the
// active [authMode], and the bounded `context.Context`. Each test
// function's per-cell body is what differs; the iteration scaffold is
// shared here so a new live test only writes the assertion body.
func forEachProviderMode(t *testing.T, body func(t *testing.T, p Provider, m authMode, ctx context.Context)) {
	t.Helper()
	for _, p := range Providers {
		t.Run(p.Name, func(t *testing.T) {
			p.skipIfOffline(t)
			for _, m := range p.authModes(t) {
				t.Run(m.name, func(t *testing.T) {
					ctx, cancel := context.WithTimeout(
						context.Background(), 30*time.Second)
					defer cancel()
					body(t, p, m, ctx)
				})
			}
		})
	}
}

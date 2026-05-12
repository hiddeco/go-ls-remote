//go:build live

package livetest

import (
	"context"
	"strings"
	"testing"
	"time"

	lsremote "github.com/hiddeco/go-ls-remote"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDefaultBranch exercises [lsremote.DefaultBranch] against every
// provider in [Providers] and every auth mode currently surfaced by
// [Provider.authModes]. The outer `t.Run` keys on the provider name;
// the inner `t.Run` keys on the mode name. Sub-test paths therefore
// read e.g. `TestDefaultBranch/github/none` and
// `TestDefaultBranch/gitlab/ssh-key`, matching the matrix dimensions
// one-to-one so a `-run` filter can target a single cell.
//
// The default invocation `go test -tags=live ./internal/livetest/...`
// exports no credential env vars, so [Provider.authModes] returns only
// the `none` row per provider. Credentialed rows only appear when the
// caller exports the matching env vars listed on each [Provider]; the
// body of the sub-test is identical regardless.
//
// Assertions are deliberately shape-only: the returned target must be
// non-empty and prefixed with `refs/heads/`. Per-provider equality
// checks (e.g. `refs/heads/master` for GitHub) are intentionally
// omitted because upstream defaults drift over time and a hard-coded
// expectation would convert a benign rename into a test failure.
func TestDefaultBranch(t *testing.T) {
	for _, p := range Providers {
		t.Run(p.Name, func(t *testing.T) {
			p.skipIfOffline(t)
			for _, m := range p.authModes(t) {
				t.Run(m.name, func(t *testing.T) {
					ctx, cancel := context.WithTimeout(
						context.Background(), 30*time.Second)
					defer cancel()

					branch, err := lsremote.DefaultBranch(
						ctx, m.url, m.options...)
					require.NoErrorf(t, err,
						"%s/%s: DefaultBranch failed",
						p.Name, m.name)
					assert.NotEmptyf(t, branch,
						"%s/%s: expected non-empty default-branch ref",
						p.Name, m.name)
					assert.Truef(t,
						strings.HasPrefix(branch, "refs/heads/"),
						"%s/%s: expected refs/heads/ prefix, got %q",
						p.Name, m.name, branch)
					t.Logf("%s/%s: default branch = %q",
						p.Name, m.name, branch)
				})
			}
		})
	}
}

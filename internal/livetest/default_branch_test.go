//go:build live

package livetest

import (
	"context"
	"strings"
	"testing"

	lsremote "github.com/hiddeco/go-ls-remote"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDefaultBranch exercises [lsremote.DefaultBranch] against every
// cell in the live matrix. Iteration, offline skipping, and per-cell
// context bounding live on [forEachProviderMode]; this test contributes
// only the per-cell assertion body.
//
// Assertions are deliberately shape-only: the returned target must be
// non-empty and prefixed with `refs/heads/`. Per-provider equality
// checks (e.g. `refs/heads/master` for GitHub) are intentionally
// omitted because upstream defaults drift over time and a hard-coded
// expectation would convert a benign rename into a test failure.
func TestDefaultBranch(t *testing.T) {
	forEachProviderMode(t, func(t *testing.T, p Provider, m authMode, ctx context.Context) {
		branch, err := lsremote.DefaultBranch(ctx, m.url, m.options...)
		require.NoErrorf(t, err,
			"%s/%s: DefaultBranch failed", p.Name, m.name)
		assert.NotEmptyf(t, branch,
			"%s/%s: expected non-empty default-branch ref",
			p.Name, m.name)
		assert.Truef(t,
			strings.HasPrefix(branch, "refs/heads/"),
			"%s/%s: expected refs/heads/ prefix, got %q",
			p.Name, m.name, branch)
		t.Logf("%s/%s: default branch = %q", p.Name, m.name, branch)
	})
}

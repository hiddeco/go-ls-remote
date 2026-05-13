//go:build live

package livetest

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	lsremote "github.com/hiddeco/go-ls-remote"
)

// TestTags exercises [lsremote.Tags] against every cell in the live
// matrix. Iteration, offline skipping, and per-cell context bounding
// live on [forEachProviderMode]; this test contributes only the
// per-cell assertion body. Every returned ref must carry the
// `refs/tags/` prefix and at least one tag must be observed.
func TestTags(t *testing.T) {
	t.Parallel()
	forEachProviderMode(t, func(t *testing.T, p Provider, m authMode, ctx context.Context) {
		seq, err := lsremote.Tags(ctx, m.url, m.options...)
		require.NoErrorf(t, err, "%s/%s: Tags dial", p.Name, m.name)

		var count int
		for ref, err := range seq {
			require.NoErrorf(t, err,
				"%s/%s: iterator error", p.Name, m.name)
			assert.Truef(t,
				strings.HasPrefix(ref.Name, "refs/tags/"),
				"%s/%s: expected refs/tags/ prefix, got %q",
				p.Name, m.name, ref.Name)
			count++
		}
		assert.Greaterf(t, count, 0,
			"%s/%s: expected at least one tag", p.Name, m.name)
	})
}

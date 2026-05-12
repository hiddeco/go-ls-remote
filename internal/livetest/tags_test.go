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

// TestTags exercises [lsremote.Tags] against every provider in
// [Providers] and every auth mode currently surfaced by
// [Provider.authModes]. The outer `t.Run` keys on the provider name;
// the inner `t.Run` keys on the mode name. Sub-test paths therefore
// read e.g. `TestTags/github/none` and `TestTags/gitlab/ssh-key`,
// matching the matrix dimensions one-to-one so a `-run` filter can
// target a single cell.
//
// The default invocation `go test -tags=live ./internal/livetest/...`
// exports no credential env vars, so [Provider.authModes] returns only
// the `none` row per provider. The test then dials each provider's
// public HTTPS endpoint anonymously. Credentialed rows only appear
// when the caller exports the matching env vars listed on each
// [Provider]; the body of the sub-test is identical regardless.
func TestTags(t *testing.T) {
	for _, p := range Providers {
		t.Run(p.Name, func(t *testing.T) {
			p.skipIfOffline(t)
			for _, m := range p.authModes(t) {
				t.Run(m.name, func(t *testing.T) {
					ctx, cancel := context.WithTimeout(
						context.Background(), 30*time.Second)
					defer cancel()

					seq, err := lsremote.Tags(ctx, m.url, m.options...)
					require.NoError(t, err,
						"%s/%s: Tags dial", p.Name, m.name)

					var count int
					for ref, err := range seq {
						require.NoError(t, err,
							"%s/%s: iterator error", p.Name, m.name)
						assert.True(t,
							strings.HasPrefix(ref.Name, "refs/tags/"),
							"%s/%s: expected refs/tags/ prefix, got %q",
							p.Name, m.name, ref.Name)
						count++
					}
					assert.Greater(t, count, 0,
						"%s/%s: expected at least one tag",
						p.Name, m.name)
				})
			}
		})
	}
}

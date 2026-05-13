//go:build live

package livetest

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	lsremote "github.com/hiddeco/go-ls-remote"
)

// TestObjectInfo exercises [lsremote.ObjectInfos] against every cell
// in the live matrix. Iteration, offline skipping, and per-cell
// context bounding live on [forEachProviderMode]; this test
// contributes only the per-cell assertion body.
//
// The per-cell body runs in two steps. First it calls [lsremote.Tags]
// to discover one tag's object id from the provider's public repo —
// the call is shape-only here, with its own assertions covered by
// [TestTags]. Second it issues [lsremote.ObjectInfos] for that id and
// asserts the returned size is positive (a real tag object always
// occupies non-zero bytes on disk).
//
// Two skip paths keep the cell graceful in the face of upstream
// drift. A provider whose v2 advertisement omits the `object-info`
// command surfaces as a [*lsremote.ProtocolError] chaining to
// [lsremote.ErrUnsupportedProtocol]; this is the documented "server
// does not speak this command" signal and the cell skips with a
// diagnostic naming the provider and auth mode. A provider whose
// public repo unexpectedly carries no tags skips too — distinct from
// [TestTags]'s behaviour, which fails on zero tags, since this test
// depends on [lsremote.Tags] yielding at least one entry as a
// precondition.
func TestObjectInfo(t *testing.T) {
	t.Parallel()

	forEachProviderMode(t, func(t *testing.T, p Provider, m authMode, ctx context.Context) {
		seq, err := lsremote.Tags(ctx, m.url, m.options...)
		require.NoErrorf(t, err, "%s/%s: Tags failed", p.Name, m.name)

		var firstHash string
		for ref, err := range seq {
			require.NoErrorf(t, err,
				"%s/%s: Tags iterator error", p.Name, m.name)
			if ref.Hash != "" {
				firstHash = ref.Hash
				break
			}
		}
		if firstHash == "" {
			t.Skipf("%s/%s: no tags returned", p.Name, m.name)
		}

		infos, err := lsremote.ObjectInfos(ctx, m.url,
			[]string{firstHash},
			lsremote.ObjectInfoRequest{Size: true}, m.options...)
		if errors.Is(err, lsremote.ErrUnsupportedProtocol) {
			t.Skipf("%s/%s: object-info not advertised", p.Name, m.name)
		}
		require.NoErrorf(t, err,
			"%s/%s: ObjectInfos failed", p.Name, m.name)
		require.Lenf(t, infos, 1,
			"%s/%s: expected exactly one info row", p.Name, m.name)
		assert.Equalf(t, firstHash, infos[0].Hash,
			"%s/%s: expected returned hash to match request",
			p.Name, m.name)
		assert.Positivef(t, infos[0].Size,
			"%s/%s: expected non-zero size for tag %s",
			p.Name, m.name, firstHash)
		t.Logf("%s/%s: tag %s size = %d",
			p.Name, m.name, firstHash, infos[0].Size)
	})
}

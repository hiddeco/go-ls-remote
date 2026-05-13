package objfmt

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIdx_FindOffset_v1(t *testing.T) {
	t.Parallel()
	t.Run("returns the recorded offset for every entry", func(t *testing.T) {
		t.Parallel()
		oid1, err := ParseSHA1Hex("1111111111111111111111111111111111111111")
		require.NoError(t, err)
		oid2, err := ParseSHA1Hex("5555555555555555555555555555555555555555")
		require.NoError(t, err)
		oid3, err := ParseSHA1Hex("ffffffffffffffffffffffffffffffffffffffff")
		require.NoError(t, err)
		entries := []v1Entry{
			{offset: 12, oid: oid1},
			{offset: 256, oid: oid2},
			{offset: 1024, oid: oid3},
		}
		path := writeV1Idx(t, t.TempDir(), entries)

		idx, err := OpenIdx[SHA1Hash](path, SHA1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = idx.Close() })

		for _, e := range entries {
			off, ok := idx.FindOffset(e.oid)
			require.True(t, ok, "missing oid %s", e.oid.Hex())
			assert.Equal(t, int64(e.offset), off)
		}
	})

	t.Run("returns ok=false for an absent oid", func(t *testing.T) {
		t.Parallel()
		oid, err := ParseSHA1Hex("1111111111111111111111111111111111111111")
		require.NoError(t, err)
		path := writeV1Idx(t, t.TempDir(), []v1Entry{{offset: 12, oid: oid}})

		idx, err := OpenIdx[SHA1Hash](path, SHA1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = idx.Close() })

		absent, err := ParseSHA1Hex("2222222222222222222222222222222222222222")
		require.NoError(t, err)
		off, ok := idx.FindOffset(absent)
		assert.False(t, ok)
		assert.Equal(t, int64(-1), off)
	})

	t.Run("v1 idx opened as SHA256 always misses", func(t *testing.T) {
		t.Parallel()
		// Stage a v1 idx (always SHA-1) but open it claiming SHA-256.
		// `findOffsetV2` is the only path the open lands on under
		// SHA-256 and it always misses on a v1 file because the idx
		// version dispatch in `FindOffset` short-circuits — verify the
		// behaviour by probing a SHA-256-shaped lookup OID.
		sha1Oid, err := ParseSHA1Hex("1111111111111111111111111111111111111111")
		require.NoError(t, err)
		path := writeV1Idx(t, t.TempDir(), []v1Entry{{offset: 12, oid: sha1Oid}})

		idx, err := OpenIdx[SHA256Hash](path, SHA256)
		require.NoError(t, err)
		t.Cleanup(func() { _ = idx.Close() })

		// Lift the SHA-1 bytes into a SHA-256-shaped lookup so the call
		// type-checks; the v1 dispatch returns a miss regardless.
		var probe SHA256Hash
		copy(probe[:20], sha1Oid[:])
		off, ok := idx.FindOffset(probe)
		assert.False(t, ok)
		assert.Equal(t, int64(-1), off)
	})
}

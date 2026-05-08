package objfmt

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIdx_FindOffset_v1(t *testing.T) {
	t.Run("returns the recorded offset for every entry", func(t *testing.T) {
		oid1, err := ParseHex("1111111111111111111111111111111111111111", SHA1)
		require.NoError(t, err)
		oid2, err := ParseHex("5555555555555555555555555555555555555555", SHA1)
		require.NoError(t, err)
		oid3, err := ParseHex("ffffffffffffffffffffffffffffffffffffffff", SHA1)
		require.NoError(t, err)
		entries := []v1Entry{
			{offset: 12, oid: oid1},
			{offset: 256, oid: oid2},
			{offset: 1024, oid: oid3},
		}
		path := writeV1Idx(t, t.TempDir(), entries)

		idx, err := OpenIdx(path, SHA1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = idx.Close() })

		for _, e := range entries {
			off, ok := idx.FindOffset(e.oid)
			require.True(t, ok, "missing oid %s", e.oid.Hex(SHA1))
			assert.Equal(t, int64(e.offset), off)
		}
	})

	t.Run("returns ok=false for an absent oid", func(t *testing.T) {
		oid, err := ParseHex("1111111111111111111111111111111111111111", SHA1)
		require.NoError(t, err)
		path := writeV1Idx(t, t.TempDir(), []v1Entry{{offset: 12, oid: oid}})

		idx, err := OpenIdx(path, SHA1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = idx.Close() })

		absent, err := ParseHex("2222222222222222222222222222222222222222", SHA1)
		require.NoError(t, err)
		off, ok := idx.FindOffset(absent)
		assert.False(t, ok)
		assert.Equal(t, int64(-1), off)
	})

	t.Run("v1 idx opened as SHA256 always misses", func(t *testing.T) {
		oid, err := ParseHex("1111111111111111111111111111111111111111", SHA1)
		require.NoError(t, err)
		path := writeV1Idx(t, t.TempDir(), []v1Entry{{offset: 12, oid: oid}})

		idx, err := OpenIdx(path, SHA256)
		require.NoError(t, err)
		t.Cleanup(func() { _ = idx.Close() })

		off, ok := idx.FindOffset(oid)
		assert.False(t, ok)
		assert.Equal(t, int64(-1), off)
	})
}

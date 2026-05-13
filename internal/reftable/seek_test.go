package reftable

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_seekToLeaf(t *testing.T) {
	t.Parallel()
	t.Run("with_index_descends_O_log", func(t *testing.T) {
		t.Parallel()
		file := readFixture(t, "with-index-sha1/0001-0001-aaaaaaaa.ref")
		h, err := parseHeader(file)
		require.NoError(t, err)
		require.NoError(t, verifyTrailer(file, h))

		idxPos := readRefIndexPosition(file, h)
		require.NotZero(t, idxPos, "fixture must have a ref index")

		var c blockProbeCounter
		// "refs/heads/branch-50" sits in the middle of the namespace
		// (HEAD < refs/heads/branch-* < refs/heads/main); a fast seek
		// must descend through the index rather than walking every
		// ref block.
		leaf, firstByte, err := seekToLeaf(file, h, idxPos, []byte("refs/heads/branch-50"), &c, nil)
		require.NoError(t, err)
		require.NotNil(t, leaf)
		assert.Equal(t, byte('r'), leaf[firstByte])

		assert.GreaterOrEqual(t, c.indexBlocks, 1, "must descend at least one index block")
		assert.Equal(t, 1, c.refBlocks, "must read exactly one leaf ref block")
	})

	t.Run("without_index_linear", func(t *testing.T) {
		t.Parallel()
		file := readFixture(t, "without-index-sha1/0001-0001-aaaaaaaa.ref")
		h, err := parseHeader(file)
		require.NoError(t, err)
		require.NoError(t, verifyTrailer(file, h))

		idxPos := readRefIndexPosition(file, h)
		require.Zero(t, idxPos, "fixture must omit the ref index")

		var c blockProbeCounter
		leaf, firstByte, err := seekToLeaf(file, h, idxPos, []byte("refs/heads/main"), &c, nil)
		require.NoError(t, err)
		require.NotNil(t, leaf)
		assert.Equal(t, byte('r'), leaf[firstByte])

		assert.Zero(t, c.indexBlocks, "no index walked")
		assert.GreaterOrEqual(t, c.refBlocks, 1, "must read at least one ref block")
	})

	t.Run("probe_before_first", func(t *testing.T) {
		t.Parallel()
		file := readFixture(t, "with-index-sha1/0001-0001-aaaaaaaa.ref")
		h, err := parseHeader(file)
		require.NoError(t, err)
		require.NoError(t, verifyTrailer(file, h))

		idxPos := readRefIndexPosition(file, h)
		require.NotZero(t, idxPos)

		// "AAA" sorts before every key in the file (smallest is "HEAD").
		// The descent must end on the FIRST leaf ref block (which
		// shares its frame with the file header), so firstByte equals
		// the v1 header size.
		var c blockProbeCounter
		leaf, firstByte, err := seekToLeaf(file, h, idxPos, []byte("AAA"), &c, nil)
		require.NoError(t, err)
		require.NotNil(t, leaf)
		assert.Equal(t, byte('r'), leaf[firstByte])
		assert.Equal(t, uint32(headerSizeV1), firstByte, "first ref block carries firstByteOffset = header size")
	})

	t.Run("probe_after_last", func(t *testing.T) {
		t.Parallel()
		file := readFixture(t, "with-index-sha1/0001-0001-aaaaaaaa.ref")
		h, err := parseHeader(file)
		require.NoError(t, err)
		require.NoError(t, verifyTrailer(file, h))

		idxPos := readRefIndexPosition(file, h)
		require.NotZero(t, idxPos)

		// "~~~" sorts after every key. seekToLeaf still returns a
		// leaf — the LAST one — so callers always have a block to
		// search; the absence is signalled by an empty intra-block
		// search rather than by an error here.
		var c blockProbeCounter
		leaf, firstByte, err := seekToLeaf(file, h, idxPos, []byte("~~~"), &c, nil)
		require.NoError(t, err)
		require.NotNil(t, leaf)
		assert.Equal(t, byte('r'), leaf[firstByte])
	})
}

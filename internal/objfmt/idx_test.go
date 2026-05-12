package objfmt

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func idxFixture(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join("..", "..", "testdata", "objfmt", name)
}

// writeV1Idx hand-rolls a synthetic version-1 SHA-1 pack index for the
// given (offset, oid) pairs and returns the path to the written file.
//
// Canonical Git no longer emits version-1 idx files directly, so the
// fixture set has to be produced by a helper here in order to exercise
// the v1 branch of [OpenIdx]. The layout matches the v1 specification
// in [Documentation/gitformat-pack.adoc lines 196-218]:
//
//	[256 × uint32 fanout]
//	[N × (uint32 offset + 20-byte SHA-1)]
//	[20-byte pack-trailer SHA-1]
//	[20-byte idx-trailer SHA-1]
//
// The pack-trailer SHA-1 is fabricated; nothing in this file references
// a real `.pack`. The idx-trailer SHA-1 is computed over every byte
// preceding it so [Idx.VerifyChecksum] accepts the result.
//
// [Documentation/gitformat-pack.adoc lines 196-218]: https://github.com/git/git/blob/v2.54.0/Documentation/gitformat-pack.adoc?plain=1#L196-L218
func writeV1Idx(t *testing.T, dir string, entries []v1Entry) string {
	t.Helper()
	// Sort by SHA so the binary search invariant holds.
	slices.SortFunc(entries, func(a, b v1Entry) int {
		return bytes.Compare(a.oid[:], b.oid[:])
	})

	buf := new(bytes.Buffer)
	// Fan-out: cumulative count of entries with first byte ≤ N.
	for n := range 256 {
		var count uint32
		for _, e := range entries {
			if e.oid[0] <= byte(n) {
				count++
			}
		}
		_ = binary.Write(buf, binary.BigEndian, count)
	}
	// Main table.
	for _, e := range entries {
		_ = binary.Write(buf, binary.BigEndian, uint32(e.offset))
		buf.Write(e.oid[:])
	}
	// Fake pack-trailer SHA-1.
	var packTrailer [20]byte
	for i := range packTrailer {
		packTrailer[i] = 0x42
	}
	buf.Write(packTrailer[:])
	// Idx-trailer SHA-1 over everything before it.
	sum := sha1.Sum(buf.Bytes())
	buf.Write(sum[:])

	path := filepath.Join(dir, "v1.idx")
	require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o600))
	return path
}

// v1Entry pairs a SHA-1 OID with its pack offset for [writeV1Idx]. v1
// idxs only ever stored SHA-1 ids so the entry is not generic.
type v1Entry struct {
	offset uint32
	oid    SHA1Hash
}

func TestIdx_OpenIdx(t *testing.T) {
	t.Run("v2 SHA-1 idx reports algo, version, count", func(t *testing.T) {
		idx, err := OpenIdx[SHA1Hash](idxFixture(t, "three-objects.idx"), SHA1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = idx.Close() })

		assert.Equal(t, SHA1, idx.Algo())
		assert.Equal(t, uint32(2), idx.Version())
		assert.Equal(t, uint32(3), idx.Count())
		assert.Equal(t, idxFixture(t, "three-objects.idx"), idx.Path())
	})

	t.Run("v2 SHA-256 idx reports algo, version, count", func(t *testing.T) {
		idx, err := OpenIdx[SHA256Hash](idxFixture(t, "sha256-three.idx"), SHA256)
		require.NoError(t, err)
		t.Cleanup(func() { _ = idx.Close() })

		assert.Equal(t, SHA256, idx.Algo())
		assert.Equal(t, uint32(2), idx.Version())
		assert.Equal(t, uint32(3), idx.Count())
	})

	t.Run("v2 SHA-256 empty idx reports zero objects", func(t *testing.T) {
		idx, err := OpenIdx[SHA256Hash](idxFixture(t, "sha256-empty.idx"), SHA256)
		require.NoError(t, err)
		t.Cleanup(func() { _ = idx.Close() })

		assert.Equal(t, SHA256, idx.Algo())
		assert.Equal(t, uint32(2), idx.Version())
		assert.Equal(t, uint32(0), idx.Count())
	})

	t.Run("hand-rolled v1 idx reports version 1", func(t *testing.T) {
		oid, err := ParseSHA1Hex("0123456789abcdef0123456789abcdef01234567")
		require.NoError(t, err)
		path := writeV1Idx(t, t.TempDir(), []v1Entry{{offset: 12, oid: oid}})

		idx, err := OpenIdx[SHA1Hash](path, SHA1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = idx.Close() })

		assert.Equal(t, SHA1, idx.Algo())
		assert.Equal(t, uint32(1), idx.Version())
		assert.Equal(t, uint32(1), idx.Count())
	})

	t.Run("rejects an unsupported v2 version", func(t *testing.T) {
		// 8-byte v2-shaped header with version 99, padded with enough
		// bytes that length checks downstream don't trip first.
		buf := []byte{0xff, 't', 'O', 'c', 0, 0, 0, 99}
		buf = append(buf, make([]byte, 256*4+20+20)...)
		path := filepath.Join(t.TempDir(), "v99.idx")
		require.NoError(t, os.WriteFile(path, buf, 0o600))

		_, err := OpenIdx[SHA1Hash](path, SHA1)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "version")
	})

	t.Run("rejects truncated input", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "tiny.idx")
		require.NoError(t, os.WriteFile(path, []byte{0xff, 't'}, 0o600))

		_, err := OpenIdx[SHA1Hash](path, SHA1)
		require.Error(t, err)
	})

	t.Run("rejects a nil algo", func(t *testing.T) {
		_, err := OpenIdx[SHA1Hash](idxFixture(t, "three-objects.idx"), nil)
		require.Error(t, err)
	})

	t.Run("rejects a missing file", func(t *testing.T) {
		_, err := OpenIdx[SHA1Hash](filepath.Join(t.TempDir(), "nope.idx"), SHA1)
		require.Error(t, err)
	})

	t.Run("Close is idempotent", func(t *testing.T) {
		idx, err := OpenIdx[SHA1Hash](idxFixture(t, "three-objects.idx"), SHA1)
		require.NoError(t, err)
		assert.NoError(t, idx.Close())
		assert.NoError(t, idx.Close())
	})

	t.Run("rejects v1 idx with non-monotonic fanout", func(t *testing.T) {
		// Synthesise a v1 idx then patch fanout[5] to a value larger
		// than fanout[6]. Mirrors [packfile.c:215-220], which rejects
		// non-monotonic indices with "non-monotonic index ...".
		//
		// [packfile.c:215-220]: https://github.com/git/git/blob/v2.54.0/packfile.c#L215-L220
		oid, err := ParseSHA1Hex("1111111111111111111111111111111111111111")
		require.NoError(t, err)
		path := writeV1Idx(t, t.TempDir(), []v1Entry{{offset: 12, oid: oid}})
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		// Fanout entries are at offsets [N*4, N*4+4). Set fanout[5]=100,
		// leaving fanout[6] (and everything above) at 1.
		binary.BigEndian.PutUint32(raw[5*4:6*4], 100)
		require.NoError(t, os.WriteFile(path, raw, 0o600))

		_, err = OpenIdx[SHA1Hash](path, SHA1)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrCorrupt)
		assert.Contains(t, err.Error(), "fanout")
	})

	t.Run("rejects v2 idx with non-monotonic fanout", func(t *testing.T) {
		oid, err := ParseSHA1Hex("1111111111111111111111111111111111111111")
		require.NoError(t, err)
		path := writeV2Idx(t, t.TempDir(), []v2Entry{
			{oid: oid, offset: 12, crc: 0xdeadbeef},
		})
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		// v2 fanout starts at byte 8 (after `\xfftOc` + version uint32).
		const fanoutStart = idxV2HeaderLen
		binary.BigEndian.PutUint32(raw[fanoutStart+5*4:fanoutStart+6*4], 100)
		require.NoError(t, os.WriteFile(path, raw, 0o600))

		_, err = OpenIdx[SHA1Hash](path, SHA1)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrCorrupt)
		assert.Contains(t, err.Error(), "fanout")
	})
}

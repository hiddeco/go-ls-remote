package objfmt

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// offsetEntry is one line of `git verify-pack -v` output: object id,
// type, and the byte offset within the pack file.
type offsetEntry struct {
	oid string
	typ string
	off int64
}

// readOffsets parses a `<stem>.offsets.txt` sidecar so tests can
// cross-check `FindOffset` against canonical `git verify-pack` output
// without reimplementing pack parsing.
func readOffsets(t *testing.T, path string) []offsetEntry {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()

	var out []offsetEntry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		fields := strings.Fields(line)
		// `verify-pack -v` per-object lines look like:
		//   <oid> <type> <size> <packed-size> <offset> [chain depth]
		// Trailer lines ("non delta:", "<file>: ok", "chain length =")
		// have a different shape and must be skipped.
		if len(fields) < 5 {
			continue
		}
		off, err := strconv.ParseInt(fields[4], 10, 64)
		if err != nil {
			continue
		}
		switch fields[1] {
		case "commit", "tree", "blob", "tag":
		default:
			continue
		}
		out = append(out, offsetEntry{oid: fields[0], typ: fields[1], off: off})
	}
	require.NoError(t, sc.Err())
	require.NotEmpty(t, out, "no offsets parsed from %s", path)
	return out
}

// writeV2Idx hand-rolls a synthetic pack-index v2 file for the given
// (oid, offset, crc) triples and returns the path to the written
// file.
//
// The helper exists to exercise the large-offset branch of
// `findOffsetV2` without needing a real >2 GiB pack on disk: callers
// can flag any offset to land in the 64-bit overflow table by
// supplying a value larger than `1<<31`. The resulting idx is only
// loosely connected to a real pack — pack-trailer SHA-1 is fabricated
// — but every other byte matches the canonical layout in
// [Documentation/gitformat-pack.adoc lines 285-319].
//
// [Documentation/gitformat-pack.adoc lines 285-319]: https://github.com/git/git/blob/v2.54.0/Documentation/gitformat-pack.adoc?plain=1#L285-L319
func writeV2Idx(t testing.TB, dir string, entries []v2Entry) string {
	t.Helper()
	// Sort by oid so the binary search invariant holds.
	for i := 1; i < len(entries); i++ {
		for j := i; j > 0 && bytes.Compare(entries[j-1].oid[:], entries[j].oid[:]) > 0; j-- {
			entries[j-1], entries[j] = entries[j], entries[j-1]
		}
	}

	buf := new(bytes.Buffer)
	buf.Write([]byte{0xff, 't', 'O', 'c'})
	_ = binary.Write(buf, binary.BigEndian, uint32(2))
	for n := range 256 {
		var count uint32
		for _, e := range entries {
			if e.oid[0] <= byte(n) {
				count++
			}
		}
		_ = binary.Write(buf, binary.BigEndian, count)
	}
	for _, e := range entries {
		buf.Write(e.oid[:])
	}
	for _, e := range entries {
		_ = binary.Write(buf, binary.BigEndian, e.crc)
	}
	// Two-pass offset/overflow build: any offset ≥ 1<<31 spills into
	// the 64-bit table and the small slot stores `0x80000000 | idx`.
	var overflow []uint64
	for _, e := range entries {
		if e.offset < 1<<31 {
			_ = binary.Write(buf, binary.BigEndian, uint32(e.offset))
		} else {
			idx := uint32(len(overflow))
			overflow = append(overflow, e.offset)
			_ = binary.Write(buf, binary.BigEndian, uint32(0x80000000)|idx)
		}
	}
	for _, off := range overflow {
		_ = binary.Write(buf, binary.BigEndian, off)
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

	path := filepath.Join(dir, "v2.idx")
	require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o600))
	return path
}

// v2Entry pairs a SHA-1 OID, pack offset, and CRC for [writeV2Idx].
// The synthetic v2 idx fixtures the helper produces are SHA-1 only;
// SHA-256 v2 idx tests draw from canonical Git fixtures rather than
// this helper.
type v2Entry struct {
	oid    SHA1Hash
	offset uint64
	crc    uint32
}

func TestIdx_FindOffset_v2(t *testing.T) {
	t.Parallel()
	t.Run("three-objects SHA-1 offsets match verify-pack", func(t *testing.T) {
		t.Parallel()
		idx, err := OpenIdx[SHA1Hash](idxFixture(t, "three-objects.idx"), SHA1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = idx.Close() })

		entries := readOffsets(t, idxFixture(t, "three-objects.offsets.txt"))
		for _, e := range entries {
			oid, err := ParseSHA1Hex(e.oid)
			require.NoError(t, err)
			off, ok := idx.FindOffset(oid)
			require.Truef(t, ok, "missing oid %s", e.oid)
			assert.Equal(t, e.off, off, "oid %s", e.oid)
		}
	})

	t.Run("sha256-three offsets match verify-pack", func(t *testing.T) {
		t.Parallel()
		idx, err := OpenIdx[SHA256Hash](idxFixture(t, "sha256-three.idx"), SHA256)
		require.NoError(t, err)
		t.Cleanup(func() { _ = idx.Close() })

		entries := readOffsets(t, idxFixture(t, "sha256-three.offsets.txt"))
		for _, e := range entries {
			oid, err := ParseSHA256Hex(e.oid)
			require.NoError(t, err)
			off, ok := idx.FindOffset(oid)
			require.Truef(t, ok, "missing oid %s", e.oid)
			assert.Equal(t, e.off, off, "oid %s", e.oid)
		}
	})

	t.Run("absent oid returns ok=false", func(t *testing.T) {
		t.Parallel()
		idx, err := OpenIdx[SHA1Hash](idxFixture(t, "three-objects.idx"), SHA1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = idx.Close() })

		absent, err := ParseSHA1Hex("ffffffffffffffffffffffffffffffffffffffff")
		require.NoError(t, err)
		off, ok := idx.FindOffset(absent)
		assert.False(t, ok)
		assert.Equal(t, int64(-1), off)
	})

	t.Run("offset > 2 GiB resolves via overflow table", func(t *testing.T) {
		t.Parallel()
		small, err := ParseSHA1Hex("1111111111111111111111111111111111111111")
		require.NoError(t, err)
		big, err := ParseSHA1Hex("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
		require.NoError(t, err)
		const bigOffset uint64 = (1 << 31) + 12345
		path := writeV2Idx(t, t.TempDir(), []v2Entry{
			{oid: small, offset: 12, crc: 0x11111111},
			{oid: big, offset: bigOffset, crc: 0x22222222},
		})

		idx, err := OpenIdx[SHA1Hash](path, SHA1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = idx.Close() })

		off, ok := idx.FindOffset(big)
		require.True(t, ok)
		assert.Equal(t, int64(bigOffset), off)

		off, ok = idx.FindOffset(small)
		require.True(t, ok)
		assert.Equal(t, int64(12), off)
	})
}

func TestIdx_FindCRC32(t *testing.T) {
	t.Parallel()
	t.Run("returns the recorded CRC for present oids", func(t *testing.T) {
		t.Parallel()
		small, err := ParseSHA1Hex("1111111111111111111111111111111111111111")
		require.NoError(t, err)
		big, err := ParseSHA1Hex("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
		require.NoError(t, err)
		path := writeV2Idx(t, t.TempDir(), []v2Entry{
			{oid: small, offset: 12, crc: 0xdeadbeef},
			{oid: big, offset: 256, crc: 0xcafef00d},
		})

		idx, err := OpenIdx[SHA1Hash](path, SHA1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = idx.Close() })

		crc, ok := idx.FindCRC32(small)
		require.True(t, ok)
		assert.Equal(t, uint32(0xdeadbeef), crc)

		crc, ok = idx.FindCRC32(big)
		require.True(t, ok)
		assert.Equal(t, uint32(0xcafef00d), crc)
	})

	t.Run("absent oid returns ok=false", func(t *testing.T) {
		t.Parallel()
		idx, err := OpenIdx[SHA1Hash](idxFixture(t, "three-objects.idx"), SHA1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = idx.Close() })

		absent, err := ParseSHA1Hex("ffffffffffffffffffffffffffffffffffffffff")
		require.NoError(t, err)
		crc, ok := idx.FindCRC32(absent)
		assert.False(t, ok)
		assert.Equal(t, uint32(0), crc)
	})

	t.Run("v1 idx returns ok=false for every lookup", func(t *testing.T) {
		t.Parallel()
		oid, err := ParseSHA1Hex("1111111111111111111111111111111111111111")
		require.NoError(t, err)
		path := writeV1Idx(t, t.TempDir(), []v1Entry{{offset: 12, oid: oid}})

		idx, err := OpenIdx[SHA1Hash](path, SHA1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = idx.Close() })

		crc, ok := idx.FindCRC32(oid)
		assert.False(t, ok)
		assert.Equal(t, uint32(0), crc)
	})
}

func TestIdx_PackChecksum(t *testing.T) {
	t.Parallel()
	t.Run("matches the trailer of the paired pack", func(t *testing.T) {
		t.Parallel()
		idx, err := OpenIdx[SHA1Hash](idxFixture(t, "three-objects.idx"), SHA1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = idx.Close() })

		got := idx.PackChecksum()

		// Read the last 20 bytes of the paired pack directly.
		pack, err := os.Open(idxFixture(t, "three-objects.pack"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = pack.Close() })
		st, err := pack.Stat()
		require.NoError(t, err)
		buf := make([]byte, 20)
		_, err = pack.ReadAt(buf, st.Size()-20)
		require.NoError(t, err)

		var want SHA1Hash
		copy(want[:], buf)
		assert.Equal(t, want, got)
	})
}

func TestIdx_VerifyChecksum(t *testing.T) {
	t.Parallel()
	t.Run("intact v2 SHA-1 idx verifies", func(t *testing.T) {
		t.Parallel()
		idx, err := OpenIdx[SHA1Hash](idxFixture(t, "three-objects.idx"), SHA1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = idx.Close() })
		assert.NoError(t, idx.VerifyChecksum())
	})

	t.Run("intact v2 SHA-256 idx verifies", func(t *testing.T) {
		t.Parallel()
		idx, err := OpenIdx[SHA256Hash](idxFixture(t, "sha256-three.idx"), SHA256)
		require.NoError(t, err)
		t.Cleanup(func() { _ = idx.Close() })
		assert.NoError(t, idx.VerifyChecksum())
	})

	t.Run("intact v1 idx verifies", func(t *testing.T) {
		t.Parallel()
		oid, err := ParseSHA1Hex("1111111111111111111111111111111111111111")
		require.NoError(t, err)
		path := writeV1Idx(t, t.TempDir(), []v1Entry{{offset: 12, oid: oid}})
		idx, err := OpenIdx[SHA1Hash](path, SHA1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = idx.Close() })
		assert.NoError(t, idx.VerifyChecksum())
	})

	t.Run("a flipped byte fails verification", func(t *testing.T) {
		t.Parallel()
		// Copy the fixture so the original on-disk file is untouched.
		dst := filepath.Join(t.TempDir(), "three-objects.idx")
		src, err := os.Open(idxFixture(t, "three-objects.idx"))
		require.NoError(t, err)
		w, err := os.Create(dst)
		require.NoError(t, err)
		_, err = io.Copy(w, src)
		require.NoError(t, err)
		require.NoError(t, src.Close())
		require.NoError(t, w.Close())

		// Flip a byte well inside the body, comfortably away from the
		// final 20-byte idx-trailer.
		f, err := os.OpenFile(dst, os.O_RDWR, 0)
		require.NoError(t, err)
		st, err := f.Stat()
		require.NoError(t, err)
		off := st.Size() - 60
		var b [1]byte
		_, err = f.ReadAt(b[:], off)
		require.NoError(t, err)
		b[0] ^= 0xff
		_, err = f.WriteAt(b[:], off)
		require.NoError(t, err)
		require.NoError(t, f.Close())

		idx, err := OpenIdx[SHA1Hash](dst, SHA1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = idx.Close() })
		assert.Error(t, idx.VerifyChecksum())
	})
}

func TestIdx_OffsetAfter(t *testing.T) {
	t.Parallel()
	t.Run("returns next-greater offset across the table", func(t *testing.T) {
		t.Parallel()
		// `three-objects.idx` records three entries at offsets 12, 131,
		// and 179 (per the sidecar). OffsetAfter must return the next
		// strictly-larger value regardless of OID-sort order, and report
		// `false` once asked past the last entry.
		idx, err := OpenIdx[SHA1Hash](idxFixture(t, "three-objects.idx"), SHA1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = idx.Close() })

		cases := []struct {
			in    int64
			want  int64
			found bool
		}{
			{0, 12, true},
			{12, 131, true},
			{131, 179, true},
			{179, 0, false},
			{1 << 30, 0, false},
		}
		for _, tc := range cases {
			got, ok := idx.OffsetAfter(tc.in)
			assert.Equalf(t, tc.found, ok, "OffsetAfter(%d) ok", tc.in)
			assert.Equalf(t, tc.want, got, "OffsetAfter(%d) value", tc.in)
		}
	})

	t.Run("resolves through the v2 large-offset overflow", func(t *testing.T) {
		t.Parallel()
		// One entry sits in the small-offset slot, one spills into the
		// 64-bit overflow table. OffsetAfter must walk both representations
		// and pick the next-larger value across the unified offset space.
		small, err := ParseSHA1Hex("1111111111111111111111111111111111111111")
		require.NoError(t, err)
		big, err := ParseSHA1Hex("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
		require.NoError(t, err)
		const bigOffset uint64 = (1 << 31) + 4096
		path := writeV2Idx(t, t.TempDir(), []v2Entry{
			{oid: small, offset: 12, crc: 0x11111111},
			{oid: big, offset: bigOffset, crc: 0x22222222},
		})
		idx, err := OpenIdx[SHA1Hash](path, SHA1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = idx.Close() })

		got, ok := idx.OffsetAfter(12)
		require.True(t, ok)
		assert.Equal(t, int64(bigOffset), got)

		_, ok = idx.OffsetAfter(int64(bigOffset))
		assert.False(t, ok)
	})

	t.Run("hand-rolled v1 idx walks the offset slot", func(t *testing.T) {
		t.Parallel()
		// v1 has no overflow table; the offset slot is 32-bit and lives at
		// the head of every record. The helper accepts the same shape used
		// elsewhere; assert next-greater across two stable values.
		oidA, err := ParseSHA1Hex("1111111111111111111111111111111111111111")
		require.NoError(t, err)
		oidB, err := ParseSHA1Hex("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
		require.NoError(t, err)
		path := writeV1Idx(t, t.TempDir(), []v1Entry{
			{offset: 12, oid: oidA},
			{offset: 256, oid: oidB},
		})
		idx, err := OpenIdx[SHA1Hash](path, SHA1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = idx.Close() })

		got, ok := idx.OffsetAfter(12)
		require.True(t, ok)
		assert.Equal(t, int64(256), got)

		_, ok = idx.OffsetAfter(256)
		assert.False(t, ok)
	})
}

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

// TestMidx_Find cross-checks `Midx.Find` against the paired `.idx`
// fixtures: every OID listed in either pack's `<stem>.offsets.txt`
// sidecar must round-trip through the midx and resolve to the same
// pack-relative offset and a pack index whose `PackNames` entry
// references the corresponding `.idx`.
func TestMidx_Find(t *testing.T) {
	t.Run("cross-checks every OID against the paired idx", func(t *testing.T) {
		midxPath := idxFixture(t, "multi-pack-index")
		m, err := OpenMidx(midxPath, SHA1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = m.Close() })

		for _, stem := range []string{"midx-pack-1", "midx-pack-2"} {
			idx, err := OpenIdx(idxFixture(t, stem+".idx"), SHA1)
			require.NoError(t, err)
			t.Cleanup(func() { _ = idx.Close() })

			entries := readOffsets(t, idxFixture(t, stem+".offsets.txt"))
			for _, e := range entries {
				oid, err := ParseHex(e.oid, SHA1)
				require.NoError(t, err)

				packIdx, off, ok := m.Find(oid)
				require.Truef(t, ok, "midx miss for %s", e.oid)
				assert.Equal(t, e.off, off,
					"oid %s offset", e.oid)
				assert.Equal(t, stem+".idx",
					m.PackNames()[packIdx],
					"oid %s pack name", e.oid)

				// Sanity: the same offset must come out of the
				// per-pack idx for this OID.
				idxOff, idxOK := idx.FindOffset(oid)
				require.Truef(t, idxOK,
					"idx miss for %s in %s", e.oid, stem)
				assert.Equal(t, idxOff, off,
					"oid %s idx vs midx offset", e.oid)
			}
		}
	})

	t.Run("cross-checks every OID against the paired SHA-256 idx", func(t *testing.T) {
		// Same round-trip as the SHA-1 case but on the SHA-256
		// fixture, exercising the 32-byte OID stride through
		// `OIDF`, `OIDL`, and `OOFF`.
		midxPath := idxFixture(t, "sha256-multi-pack-index")
		m, err := OpenMidx(midxPath, SHA256)
		require.NoError(t, err)
		t.Cleanup(func() { _ = m.Close() })

		for _, stem := range []string{"sha256-midx-pack-1", "sha256-midx-pack-2"} {
			idx, err := OpenIdx(idxFixture(t, stem+".idx"), SHA256)
			require.NoError(t, err)
			t.Cleanup(func() { _ = idx.Close() })

			entries := readOffsets(t, idxFixture(t, stem+".offsets.txt"))
			for _, e := range entries {
				oid, err := ParseHex(e.oid, SHA256)
				require.NoError(t, err)

				packIdx, off, ok := m.Find(oid)
				require.Truef(t, ok, "midx miss for %s", e.oid)
				assert.Equal(t, e.off, off,
					"oid %s offset", e.oid)
				assert.Equal(t, stem+".idx",
					m.PackNames()[packIdx],
					"oid %s pack name", e.oid)

				idxOff, idxOK := idx.FindOffset(oid)
				require.Truef(t, idxOK,
					"idx miss for %s in %s", e.oid, stem)
				assert.Equal(t, idxOff, off,
					"oid %s idx vs midx offset", e.oid)
			}
		}
	})

	t.Run("absent oid returns ok=false", func(t *testing.T) {
		m, err := OpenMidx(idxFixture(t, "multi-pack-index"), SHA1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = m.Close() })

		absent, err := ParseHex("ffffffffffffffffffffffffffffffffffffffff", SHA1)
		require.NoError(t, err)
		_, _, ok := m.Find(absent)
		assert.False(t, ok)
	})

	t.Run("offset > 2 GiB resolves via LOFF", func(t *testing.T) {
		small, err := ParseHex("1111111111111111111111111111111111111111", SHA1)
		require.NoError(t, err)
		big, err := ParseHex("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", SHA1)
		require.NoError(t, err)

		const bigOffset uint64 = (1 << 31) + 12345
		path := writeMidx(t, t.TempDir(), midxFixture{
			algo:  SHA1,
			packs: []string{"a.idx", "b.idx"},
			objs: []midxObj{
				{oid: small, packIdx: 0, offset: 12},
				{oid: big, packIdx: 1, offset: bigOffset},
			},
		})

		m, err := OpenMidx(path, SHA1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = m.Close() })

		packIdx, off, ok := m.Find(big)
		require.True(t, ok)
		assert.Equal(t, uint32(1), packIdx)
		assert.Equal(t, int64(bigOffset), off)

		packIdx, off, ok = m.Find(small)
		require.True(t, ok)
		assert.Equal(t, uint32(0), packIdx)
		assert.Equal(t, int64(12), off)
	})

	t.Run("LOFF bit set without LOFF chunk is treated as a miss", func(t *testing.T) {
		oid, err := ParseHex("1111111111111111111111111111111111111111", SHA1)
		require.NoError(t, err)
		path := writeMidx(t, t.TempDir(), midxFixture{
			algo:  SHA1,
			packs: []string{"a.idx"},
			objs: []midxObj{
				// suppressLOFF forces the writer to set the
				// LOFF MSB on the small offset slot but skip
				// emitting the LOFF chunk, so the lookup must
				// surface ok=false rather than indexing past
				// the file.
				{oid: oid, packIdx: 0, offset: (1 << 31) + 7},
			},
			suppressLOFF: true,
		})

		m, err := OpenMidx(path, SHA1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = m.Close() })

		_, _, ok := m.Find(oid)
		assert.False(t, ok)
	})
}

// midxFixture configures `writeMidx` to assemble a synthetic midx in a
// `t.TempDir()`. It exists so tests can construct edge cases — the
// LOFF >2 GiB path, a corrupt LOFF flag with no LOFF chunk — without
// needing canonical Git to do it for them.
type midxFixture struct {
	algo         Algo
	packs        []string
	objs         []midxObj
	suppressLOFF bool
}

// midxObj is one logical OID record in a synthetic midx.
type midxObj struct {
	oid     Hash
	packIdx uint32
	offset  uint64
}

// writeMidx builds a minimal SHA-1 multi-pack-index out of the given
// fixture and writes it to `<dir>/multi-pack-index`. The output
// contains exactly the four required chunks plus an optional LOFF
// chunk; bitmap and reverse-index chunks are never emitted.
//
// Layout matches `midx-write.c`: a 12-byte header, then a chunk
// lookup table sized to the present chunks, then chunk bodies in
// declaration order, then a 20-byte SHA-1 trailer over every
// preceding byte. SHA-256 is not implemented here because every
// LOFF/edge-case test only needs the SHA-1 stride.
func writeMidx(t testing.TB, dir string, fix midxFixture) string {
	t.Helper()
	require.Equal(t, SHA1, fix.algo,
		"writeMidx only supports SHA-1 fixtures")
	hashLen := fix.algo.Size()

	// Sort objects by OID to satisfy the binary-search invariant in
	// the OIDL chunk; a real midx produced by canonical Git is sorted
	// the same way.
	slices.SortFunc(fix.objs, func(a, b midxObj) int {
		return bytes.Compare(a.oid[:hashLen], b.oid[:hashLen])
	})

	// Build PNAM with NUL-terminated entries, padded to a 4-byte
	// boundary as canonical Git does (`midx-write.c::write_midx_pack_names`).
	var pnam bytes.Buffer
	for _, name := range fix.packs {
		pnam.WriteString(name)
		pnam.WriteByte(0)
	}
	for pnam.Len()%4 != 0 {
		pnam.WriteByte(0)
	}

	// Build OIDF (256 × uint32 BE), OIDL (concatenated OIDs), and
	// OOFF (per-object 4-byte pack id + 4-byte offset). Any offset
	// whose top bit is set, or any offset ≥ 1<<31, spills into LOFF
	// (8 bytes BE per overflow entry).
	var oidf bytes.Buffer
	for n := 0; n < 256; n++ {
		var count uint32
		for _, o := range fix.objs {
			if o.oid[0] <= byte(n) {
				count++
			}
		}
		_ = binary.Write(&oidf, binary.BigEndian, count)
	}

	var oidl bytes.Buffer
	for _, o := range fix.objs {
		oidl.Write(o.oid[:hashLen])
	}

	var ooff bytes.Buffer
	var loffBuf bytes.Buffer
	for _, o := range fix.objs {
		_ = binary.Write(&ooff, binary.BigEndian, o.packIdx)
		if o.offset < 1<<31 {
			_ = binary.Write(&ooff, binary.BigEndian, uint32(o.offset))
		} else {
			idx := uint32(loffBuf.Len() / 8)
			_ = binary.Write(&ooff, binary.BigEndian, uint32(0x80000000)|idx)
			_ = binary.Write(&loffBuf, binary.BigEndian, o.offset)
		}
	}

	// Decide which chunks to emit and in what order. The terminator
	// is one extra entry whose ID is all-zero and whose offset points
	// one past the last chunk's body.
	type emit struct {
		id   [4]byte
		body []byte
	}
	chunks := []emit{
		{id: [4]byte{'P', 'N', 'A', 'M'}, body: pnam.Bytes()},
		{id: [4]byte{'O', 'I', 'D', 'F'}, body: oidf.Bytes()},
		{id: [4]byte{'O', 'I', 'D', 'L'}, body: oidl.Bytes()},
		{id: [4]byte{'O', 'O', 'F', 'F'}, body: ooff.Bytes()},
	}
	if loffBuf.Len() > 0 && !fix.suppressLOFF {
		chunks = append(chunks,
			emit{id: [4]byte{'L', 'O', 'F', 'F'}, body: loffBuf.Bytes()})
	}

	// 12-byte header + (numChunks + 1) × 12 byte TOC entries gives
	// the offset of the first chunk body.
	headerLen := 12
	tocLen := (len(chunks) + 1) * 12
	bodyOff := int64(headerLen + tocLen)

	out := new(bytes.Buffer)
	out.Write([]byte{'M', 'I', 'D', 'X'})
	out.WriteByte(1)                        // version
	out.WriteByte(1)                        // hash version (SHA-1)
	out.WriteByte(byte(len(chunks)))        // num_chunks
	out.WriteByte(0)                        // reserved
	_ = binary.Write(out, binary.BigEndian, // num_packs
		uint32(len(fix.packs)))

	cur := bodyOff
	for _, c := range chunks {
		out.Write(c.id[:])
		_ = binary.Write(out, binary.BigEndian, uint64(cur))
		cur += int64(len(c.body))
	}
	// Terminator: all-zero ID, offset one past the last body.
	out.Write(make([]byte, 4))
	_ = binary.Write(out, binary.BigEndian, uint64(cur))

	for _, c := range chunks {
		out.Write(c.body)
	}
	sum := sha1.Sum(out.Bytes())
	out.Write(sum[:])

	path := filepath.Join(dir, "multi-pack-index")
	require.NoError(t, os.WriteFile(path, out.Bytes(), 0o600))
	return path
}

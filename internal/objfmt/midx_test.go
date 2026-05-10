package objfmt

import (
	"bufio"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// midxPackNames reads the `<midx>.packnames` sidecar produced by the
// fixture generator. The sidecar lists the PNAM chunk's contents in
// PNAM order — `.idx` basenames as canonical Git records them — so
// tests can cross-check `Midx.PackNames` without re-parsing the midx.
func midxPackNames(t *testing.T, midxPath string) []string {
	t.Helper()
	f, err := os.Open(midxPath + ".packnames")
	require.NoError(t, err)
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		out = append(out, line)
	}
	require.NoError(t, sc.Err())
	return out
}

func TestMidx_OpenMidx(t *testing.T) {
	t.Run("SHA-1 fixture reports algo, version, count, packs", func(t *testing.T) {
		path := idxFixture(t, "multi-pack-index")
		m, err := OpenMidx(path, SHA1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = m.Close() })

		assert.Equal(t, SHA1, m.Algo())
		assert.Equal(t, uint32(1), m.Version())
		// Two packs, each with three objects (commit/tree/blob), all
		// distinct — six objects in the fanout.
		assert.Equal(t, uint32(6), m.Count())
		assert.Equal(t, midxPackNames(t, path), m.PackNames())
	})

	t.Run("SHA-256 fixture reports SHA256 and version 1", func(t *testing.T) {
		path := idxFixture(t, "sha256-multi-pack-index")
		m, err := OpenMidx(path, SHA256)
		require.NoError(t, err)
		t.Cleanup(func() { _ = m.Close() })

		assert.Equal(t, SHA256, m.Algo())
		assert.Equal(t, uint32(1), m.Version())
		assert.Equal(t, uint32(6), m.Count())
		assert.Equal(t, midxPackNames(t, path), m.PackNames())
	})

	t.Run("rejects a non-MIDX magic", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "bad.midx")
		buf := make([]byte, 12+20)
		copy(buf, "NOTM")
		require.NoError(t, os.WriteFile(path, buf, 0o600))

		_, err := OpenMidx(path, SHA1)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "magic")
	})

	t.Run("rejects unsupported version 0", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "v0.midx")
		buf := make([]byte, 12+20)
		copy(buf, "MIDX")
		buf[4] = 0
		buf[5] = 1
		require.NoError(t, os.WriteFile(path, buf, 0o600))
		_, err := OpenMidx(path, SHA1)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "version")
	})

	t.Run("rejects unsupported version 99", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "v99.midx")
		buf := make([]byte, 12+20)
		copy(buf, "MIDX")
		buf[4] = 99
		buf[5] = 1
		require.NoError(t, os.WriteFile(path, buf, 0o600))
		_, err := OpenMidx(path, SHA1)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "version")
	})

	t.Run("rejects a non-zero reserved byte", func(t *testing.T) {
		// Canonical Git's `write_midx_header` writes byte 7 as 0; a
		// non-zero value either means a malformed file or a future
		// extension that this reader is not equipped to handle.
		path := filepath.Join(t.TempDir(), "reserved.midx")
		buf := make([]byte, 12+20)
		copy(buf, "MIDX")
		buf[4] = 1 // version
		buf[5] = 1 // sha1
		buf[6] = 0 // num_chunks
		buf[7] = 1 // reserved — must be 0
		require.NoError(t, os.WriteFile(path, buf, 0o600))
		_, err := OpenMidx(path, SHA1)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "reserved")
	})

	t.Run("rejects an algo mismatch", func(t *testing.T) {
		_, err := OpenMidx(idxFixture(t, "multi-pack-index"), SHA256)
		require.Error(t, err)

		_, err = OpenMidx(idxFixture(t, "sha256-multi-pack-index"), SHA1)
		require.Error(t, err)
	})

	t.Run("rejects truncated input", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "tiny.midx")
		require.NoError(t, os.WriteFile(path, []byte("MIDX"), 0o600))
		_, err := OpenMidx(path, SHA1)
		require.Error(t, err)
	})

	t.Run("rejects an unknown algo", func(t *testing.T) {
		_, err := OpenMidx(idxFixture(t, "multi-pack-index"), Algo(0))
		require.Error(t, err)
	})

	t.Run("rejects a missing file", func(t *testing.T) {
		_, err := OpenMidx(filepath.Join(t.TempDir(), "nope.midx"), SHA1)
		require.Error(t, err)
	})

	t.Run("Close is idempotent", func(t *testing.T) {
		m, err := OpenMidx(idxFixture(t, "multi-pack-index"), SHA1)
		require.NoError(t, err)
		assert.NoError(t, m.Close())
		assert.NoError(t, m.Close())
	})

	t.Run("rejects non-monotonic OIDF fanout", func(t *testing.T) {
		// Build a clean midx, locate its OIDF chunk via the TOC, and
		// patch fanout[5] above fanout[6]. Mirrors `midx.c:62-71`,
		// which rejects "oid fanout out of order".
		oid, err := ParseHex("1111111111111111111111111111111111111111", SHA1)
		require.NoError(t, err)
		path := writeMidx(t, t.TempDir(), midxFixture{
			algo:  SHA1,
			packs: []string{"a.idx"},
			objs:  []midxObj{{oid: oid, packIdx: 0, offset: 12}},
		})
		raw, err := os.ReadFile(path)
		require.NoError(t, err)

		oidfOff := findChunkOffset(t, raw, "OIDF")
		// fanout[5] = 100, well above fanout[6] = 1.
		binary.BigEndian.PutUint32(raw[oidfOff+5*4:oidfOff+6*4], 100)
		require.NoError(t, os.WriteFile(path, raw, 0o600))

		_, err = OpenMidx(path, SHA1)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrCorrupt)
		assert.Contains(t, err.Error(), "fanout")
	})

	t.Run("rejects OOFF packIdx out of range", func(t *testing.T) {
		// One pack listed in PNAM, but the object's packIdx is 7 —
		// past the end. A corrupt midx must not become a runtime
		// slice-bounds panic at the downstream `midxBackend.Lookup`
		// layer; reject at parse time instead. Canonical Git's
		// `midx.c::nth_midxed_pack_int_id` likewise validates the
		// pack-int-id against `num_packs`.
		oid, err := ParseHex("1111111111111111111111111111111111111111", SHA1)
		require.NoError(t, err)
		path := writeMidx(t, t.TempDir(), midxFixture{
			algo:  SHA1,
			packs: []string{"a.idx"},
			objs:  []midxObj{{oid: oid, packIdx: 7, offset: 12}},
		})

		_, err = OpenMidx(path, SHA1)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrCorrupt)
		assert.Contains(t, err.Error(), "pack")
	})

	t.Run("rejects v1 non-ascending pack names", func(t *testing.T) {
		// Canonical Git rejects v1 midx files whose PNAM entries are
		// not in strict-ascending lexicographic order. Mirrors
		// `midx.c:213-218` ("multi-pack-index pack names out of order").
		oid, err := ParseHex("1111111111111111111111111111111111111111", SHA1)
		require.NoError(t, err)
		path := writeMidx(t, t.TempDir(), midxFixture{
			algo:  SHA1,
			packs: []string{"b.idx", "a.idx"},
			objs:  []midxObj{{oid: oid, packIdx: 0, offset: 12}},
		})

		_, err = OpenMidx(path, SHA1)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrCorrupt)
		assert.Contains(t, err.Error(), "pack name")
	})

	t.Run("rejects v1 duplicate pack names", func(t *testing.T) {
		// Canonical Git's check is `strcmp(...) <= 0`, which also
		// rejects duplicate basenames. Mirrors `midx.c:213-218`.
		oid, err := ParseHex("1111111111111111111111111111111111111111", SHA1)
		require.NoError(t, err)
		path := writeMidx(t, t.TempDir(), midxFixture{
			algo:  SHA1,
			packs: []string{"a.idx", "a.idx"},
			objs:  []midxObj{{oid: oid, packIdx: 0, offset: 12}},
		})

		_, err = OpenMidx(path, SHA1)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrCorrupt)
		assert.Contains(t, err.Error(), "pack name")
	})

	t.Run("accepts v2 unsorted pack names", func(t *testing.T) {
		// Canonical Git relaxes the ordering check for v2; only v1 is
		// strict. Build a fixture with packs out of order, flip the
		// version byte to 2, and confirm the parser accepts it.
		oid, err := ParseHex("1111111111111111111111111111111111111111", SHA1)
		require.NoError(t, err)
		path := writeMidx(t, t.TempDir(), midxFixture{
			algo:  SHA1,
			packs: []string{"b.idx", "a.idx"},
			objs:  []midxObj{{oid: oid, packIdx: 0, offset: 12}},
		})
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		raw[4] = 2 // version 2 — relaxed pack-name ordering
		require.NoError(t, os.WriteFile(path, raw, 0o600))

		m, err := OpenMidx(path, SHA1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = m.Close() })
		assert.Equal(t, uint32(2), m.Version())
		assert.Equal(t, []string{"b.idx", "a.idx"}, m.PackNames())
	})

	t.Run("rejects non-ascending OIDL", func(t *testing.T) {
		// Build a clean two-object midx, locate its OIDL chunk via the
		// TOC, and swap the two OIDs so the lookup table is no longer
		// strictly ascending. The fanout-bounded binary search in
		// `Midx.Find` (mirroring `bsearch_one_midx` in `midx.c`) only
		// returns correct answers when OIDL is sorted; an unsorted OIDL
		// is silent corruption at lookup time. Canonical Git's
		// `midx_read_oid_lookup` (`midx.c:76-84`) does not validate
		// ordering at load — v0 adds this defense-in-depth check at
		// parse time so a malformed file is rejected immediately.
		oidLow, err := ParseHex("1111111111111111111111111111111111111111", SHA1)
		require.NoError(t, err)
		oidHigh, err := ParseHex("2222222222222222222222222222222222222222", SHA1)
		require.NoError(t, err)
		path := writeMidx(t, t.TempDir(), midxFixture{
			algo:  SHA1,
			packs: []string{"a.idx"},
			objs: []midxObj{
				{oid: oidLow, packIdx: 0, offset: 12},
				{oid: oidHigh, packIdx: 0, offset: 24},
			},
		})
		raw, err := os.ReadFile(path)
		require.NoError(t, err)

		oidlOff := findChunkOffset(t, raw, "OIDL")
		hashLen := int64(SHA1.Size())
		// Swap the two consecutive 20-byte OIDs.
		first := append([]byte{}, raw[oidlOff:oidlOff+hashLen]...)
		second := append([]byte{}, raw[oidlOff+hashLen:oidlOff+2*hashLen]...)
		copy(raw[oidlOff:oidlOff+hashLen], second)
		copy(raw[oidlOff+hashLen:oidlOff+2*hashLen], first)
		require.NoError(t, os.WriteFile(path, raw, 0o600))

		_, err = OpenMidx(path, SHA1)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrCorrupt)
		assert.Contains(t, err.Error(), "OIDL")
	})

	t.Run("rejects duplicate OIDL entries", func(t *testing.T) {
		// Stamp the second OID record on top of the first so OIDL
		// holds the same OID twice. The midx invariant requires
		// unique OIDs (each object appears once); equal consecutive
		// hashes are corruption just like an inversion.
		oidLow, err := ParseHex("1111111111111111111111111111111111111111", SHA1)
		require.NoError(t, err)
		oidHigh, err := ParseHex("2222222222222222222222222222222222222222", SHA1)
		require.NoError(t, err)
		path := writeMidx(t, t.TempDir(), midxFixture{
			algo:  SHA1,
			packs: []string{"a.idx"},
			objs: []midxObj{
				{oid: oidLow, packIdx: 0, offset: 12},
				{oid: oidHigh, packIdx: 0, offset: 24},
			},
		})
		raw, err := os.ReadFile(path)
		require.NoError(t, err)

		oidlOff := findChunkOffset(t, raw, "OIDL")
		hashLen := int64(SHA1.Size())
		// Overwrite slot 1 with slot 0's OID — strict-ascending
		// rejects equal consecutive entries, not just inversions.
		copy(raw[oidlOff+hashLen:oidlOff+2*hashLen],
			raw[oidlOff:oidlOff+hashLen])
		require.NoError(t, os.WriteFile(path, raw, 0o600))

		_, err = OpenMidx(path, SHA1)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrCorrupt)
		assert.Contains(t, err.Error(), "OIDL")
	})

	t.Run("rejects misaligned chunk offset", func(t *testing.T) {
		// Patch the TOC entry for OIDF to a misaligned absolute
		// offset (off by 1). Mirrors `chunk-format.c:127-130`, which
		// rejects "chunk id ... not 4-byte aligned" with
		// `MIDX_CHUNK_ALIGNMENT = 4` in `midx.h`.
		oid, err := ParseHex("1111111111111111111111111111111111111111", SHA1)
		require.NoError(t, err)
		path := writeMidx(t, t.TempDir(), midxFixture{
			algo:  SHA1,
			packs: []string{"a.idx"},
			objs:  []midxObj{{oid: oid, packIdx: 0, offset: 12}},
		})
		raw, err := os.ReadFile(path)
		require.NoError(t, err)

		// Locate the TOC entry for OIDF and shift its offset by 1.
		// The body is now misaligned but still inside the file, so
		// only the alignment check should reject the open.
		tocStart := 12
		numChunks := int(raw[6])
		var oidfTOC int
		found := false
		for i := range numChunks {
			base := tocStart + i*12
			if string(raw[base:base+4]) == "OIDF" {
				oidfTOC = base
				found = true
				break
			}
		}
		require.True(t, found, "OIDF entry missing from TOC")
		curOff := binary.BigEndian.Uint64(raw[oidfTOC+4 : oidfTOC+12])
		binary.BigEndian.PutUint64(raw[oidfTOC+4:oidfTOC+12], curOff+1)
		require.NoError(t, os.WriteFile(path, raw, 0o600))

		_, err = OpenMidx(path, SHA1)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrCorrupt)
		assert.Contains(t, err.Error(), "align")
	})
}

// findChunkOffset returns the absolute byte offset of the named chunk's
// body in raw by walking the chunk TOC. Used by tests that need to
// surgically corrupt one chunk's contents.
func findChunkOffset(t *testing.T, raw []byte, id string) int64 {
	t.Helper()
	require.Equal(t, 4, len(id))
	tocStart := 12
	numChunks := int(raw[6])
	for i := range numChunks {
		base := tocStart + i*12
		if string(raw[base:base+4]) == id {
			return int64(binary.BigEndian.Uint64(raw[base+4 : base+12]))
		}
	}
	t.Fatalf("chunk %s not found", id)
	return 0
}

func TestMidx_PackNames(t *testing.T) {
	t.Run("returned slice is a copy", func(t *testing.T) {
		m, err := OpenMidx(idxFixture(t, "multi-pack-index"), SHA1)
		require.NoError(t, err)
		t.Cleanup(func() { _ = m.Close() })

		first := m.PackNames()
		require.NotEmpty(t, first)

		// Mutate the returned slice; the next call should still see the
		// original contents.
		mutated := first[0]
		first[0] = "tampered.idx"

		second := m.PackNames()
		assert.Equal(t, mutated, second[0])
		assert.NotEqual(t, "tampered.idx", second[0])
	})
}

package objfmt

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// readFixture loads a fixture from `testdata/objfmt/` so the fuzz
// engine can seed itself from real-world inputs without embedding the
// bytes into a Go source file.
func readFixture(tb testing.TB, name string) []byte {
	tb.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "objfmt", name))
	require.NoError(tb, err)
	return b
}

// writeFuzzInput writes raw fuzzer-supplied bytes to a temporary file
// so file-oriented openers ([OpenPack], [OpenIdx], [OpenMidx]) can
// consume them without growing a `[]byte`-typed entry point.
func writeFuzzInput(t *testing.T, name string, in []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(path, in, 0o600))
	return path
}

// minimalPackStub returns the smallest byte sequence that is shaped
// like a valid pack header — `PACK` + version 2 + zero objects, with
// no trailer. It is included as a fuzz seed so the engine starts from
// a structure that drives [OpenPack] past its magic check.
func minimalPackStub() []byte {
	b := make([]byte, 12)
	copy(b, "PACK")
	binary.BigEndian.PutUint32(b[4:8], 2)
	binary.BigEndian.PutUint32(b[8:12], 0)
	return b
}

// FuzzOpenPack drives [OpenPack] over arbitrary bytes. The contract
// is "no panic": any error is acceptable, and a successful open must
// be paired with a [Pack.Close] so the underlying reader is released.
//
// Run with `go test -fuzz=FuzzOpenPack ./internal/objfmt/...`.
func FuzzOpenPack(f *testing.F) {
	seeds := [][]byte{
		nil,
		{},
		minimalPackStub(),
		readFixture(f, "empty.pack"),
		readFixture(f, "three-objects.pack"),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, in []byte) {
		path := writeFuzzInput(t, "fuzz.pack", in)
		p, err := OpenPack(path, SHA1)
		if err != nil {
			return
		}
		_ = p.Close()
	})
}

// FuzzOpenIdx drives [OpenIdx] over arbitrary bytes. As with
// [FuzzOpenPack] the contract is "no panic"; on success the index is
// closed to release its in-memory body.
//
// Run with `go test -fuzz=FuzzOpenIdx ./internal/objfmt/...`.
func FuzzOpenIdx(f *testing.F) {
	seeds := [][]byte{
		nil,
		{},
		readFixture(f, "three-objects.idx"),
		readFixture(f, "ofs-delta.idx"),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, in []byte) {
		path := writeFuzzInput(t, "fuzz.idx", in)
		i, err := OpenIdx(path, SHA1)
		if err != nil {
			return
		}
		_ = i.Close()
	})
}

// FuzzOpenMidx drives [OpenMidx] over arbitrary bytes. The contract
// is "no panic"; on success the midx is closed.
//
// Run with `go test -fuzz=FuzzOpenMidx ./internal/objfmt/...`.
func FuzzOpenMidx(f *testing.F) {
	seeds := [][]byte{
		nil,
		{},
		readFixture(f, "multi-pack-index"),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, in []byte) {
		path := writeFuzzInput(t, "multi-pack-index", in)
		m, err := OpenMidx(path, SHA1)
		if err != nil {
			return
		}
		_ = m.Close()
	})
}

// FuzzPack_ReadHeader opens `three-objects.pack` once and fuzzes the
// `at` argument to [Pack.ReadHeader]. The contract is "no panic"; an
// error from a wild offset is expected and discarded.
//
// Seeds are the three real entry offsets parsed from the matching
// `.offsets.txt`, plus a few boundary values that exercise the
// in-range / out-of-range branches.
//
// Run with `go test -fuzz=FuzzPack_ReadHeader ./internal/objfmt/...`.
func FuzzPack_ReadHeader(f *testing.F) {
	p, err := OpenPack(filepath.Join("..", "..", "testdata", "objfmt", "three-objects.pack"), SHA1)
	require.NoError(f, err)
	f.Cleanup(func() { _ = p.Close() })

	// Real entry offsets from `three-objects.offsets.txt` (column 3):
	//   commit  @ 12
	//   tree    @ 131
	//   blob    @ 179
	for _, off := range []int64{0, 12, 131, 179, -1, 1 << 30} {
		f.Add(off)
	}

	f.Fuzz(func(t *testing.T, at int64) {
		_, _ = p.ReadHeader(at)
	})
}

// FuzzPack_ReadDeltaHeader opens `ofs-delta.pack` once and fuzzes the
// `bodyAt` argument to [Pack.ReadDeltaHeader]. The contract is "no
// panic"; errors from non-delta or out-of-range offsets are expected
// and discarded.
//
// The seed corpus includes the real delta-payload `BodyAt` of the
// pack's one delta, derived by walking the pack header at offset 207
// (per `ofs-delta.offsets.txt`) before fuzzing begins.
//
// Run with `go test -fuzz=FuzzPack_ReadDeltaHeader ./internal/objfmt/...`.
func FuzzPack_ReadDeltaHeader(f *testing.F) {
	p, err := OpenPack(filepath.Join("..", "..", "testdata", "objfmt", "ofs-delta.pack"), SHA1)
	require.NoError(f, err)
	f.Cleanup(func() { _ = p.Close() })

	// The delta entry sits at offset 207 (column 3 of
	// `ofs-delta.offsets.txt`); resolve its [ObjectHeader.BodyAt] so
	// the fuzz seed lands on a well-formed zlib stream.
	hdr, err := p.ReadHeader(207)
	require.NoError(f, err)
	for _, off := range []int64{0, hdr.BodyAt, 12, 207, -1, 1 << 30} {
		f.Add(off)
	}

	f.Fuzz(func(t *testing.T, bodyAt int64) {
		_, _, _ = p.ReadDeltaHeader(bodyAt)
	})
}

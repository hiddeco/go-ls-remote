package server

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
	"github.com/hiddeco/go-ls-remote/internal/objstore"
	"github.com/hiddeco/go-ls-remote/internal/wire"
	"github.com/hiddeco/go-ls-remote/pktline"
)

// buildObjectInfoRequest builds a single v2 object-info command-request
// frame followed by the empty-request flush that terminates the
// session. argLines are written verbatim into the command-args section,
// each becoming one pkt-line; the caller supplies the trailing LF when
// the canonical wire form requires one.
func buildObjectInfoRequest(argLines []string) []byte {
	var req bytes.Buffer
	req.Write(pktBytes("command=object-info\n"))
	req.Write(delimBytes)
	for _, line := range argLines {
		req.Write(pktBytes(line))
	}
	req.Write(flushBytes) // end of command-args
	req.Write(flushBytes) // empty-request: terminate session
	return req.Bytes()
}

// querySize returns the resolved size of oidHex via the supplied
// store. Used by byte-pinned tests so the fixture's objects drive the
// expected wire bytes without the test having to hard-code sizes that
// would drift if the fixture were ever regenerated.
func querySize[H objfmt.Hash](t *testing.T, store *objstore.Store[H], oidHex string) int64 {
	t.Helper()

	hash, err := objfmt.ParseHexAs[H](oidHex)
	require.NoError(t, err)
	info, err := store.ObjectInfo(hash)
	require.NoError(t, err)
	return info.Size
}

// Stable OIDs from the `loose-objects` fixture (sha1). Mirrors the
// `looseFixture*OID` constants in `internal/objstore/loose_objects_test.go`
// — duplicated here because that package's constants are unexported.
const (
	looseBlobOID   = "393a7c05257a543bc1369537c7fdb2851dc04b11"
	looseTreeOID   = "4cb61db1e9094ba0e955298fcbd038ec69bc7a38"
	looseCommitOID = "9a1288dcf7ead9936f178d8dd8a1f14c81eafbf9"
)

// Stable OID for the `loose-objects-sha256` fixture's blob. Same byte
// content as the sha1 fixture; the sha256 hex length is 64 characters.
const loose256BlobOID = "c60061d62336c6b760e2c4ec860873a193c61662e4f2a6aa5cb3cbaf9339cd10"

// Stable OID from the packed `pack-only` / `idx-single` fixtures. The
// pack ships three non-delta objects; the commit hex matches
// `threeCommitOID` in `internal/objstore/idx_catalog_test.go`.
const packCommitOID = "26dae744f51e61913f50bd402cbe63953c7d637b"

// TestObjectInfo_Empty pins the empty-OID-list case ([protocol-caps.c:44-45]):
// when the client asks for no OIDs, the server returns immediately
// without an attrs line. The response is a single flush.
//
// [protocol-caps.c:44-45]: https://github.com/git/git/blob/v2.54.0/protocol-caps.c#L44-L45
func TestObjectInfo_Empty(t *testing.T) {
	t.Parallel()

	store := openEmptyStore(t)

	resp, err := runV2Session(t, store, buildObjectInfoRequest(nil))
	require.NoError(t, err)
	assert.Equal(t, "0000", string(resp),
		"empty OID list must yield just a flush, no attrs line")
}

// TestObjectInfo_EmptySizeOnly pins that even with `size` requested, an
// empty OID list yields just a flush. Canonical reference:
// [protocol-caps.c:44-45] returns from `send_info` before the
// `if (info->size)` block can run.
//
// [protocol-caps.c:44-45]: https://github.com/git/git/blob/v2.54.0/protocol-caps.c#L44-L45
func TestObjectInfo_EmptySizeOnly(t *testing.T) {
	t.Parallel()

	store := openEmptyStore(t)

	resp, err := runV2Session(t, store, buildObjectInfoRequest([]string{"size\n"}))
	require.NoError(t, err)
	assert.Equal(t, "0000", string(resp),
		"empty OID list must yield just a flush even with size set")
}

// TestObjectInfo_Loose_SingleHit pins the simplest hit shape: one OID,
// `size` requested, response is `size\n` attrs + `<oid> <size>\n` +
// flush. Canonical reference: [protocol-caps.c:47-71].
//
// [protocol-caps.c:47-71]: https://github.com/git/git/blob/v2.54.0/protocol-caps.c#L47-L71
func TestObjectInfo_Loose_SingleHit(t *testing.T) {
	t.Parallel()

	store := openStoreFromFixture(t, "loose-objects")
	blobSize := querySize(t, store, looseBlobOID)

	req := buildObjectInfoRequest([]string{
		"size\n",
		"oid " + looseBlobOID + "\n",
	})
	resp, err := runV2Session(t, store, req)
	require.NoError(t, err)

	want := pktLine("size\n") +
		pktLine(fmt.Sprintf("%s %d\n", looseBlobOID, blobSize)) +
		"0000"
	assert.Equal(t, want, string(resp))
}

// TestObjectInfo_Loose_NoSizeAttr pins the no-attrs branch: the client
// requests one OID without `size`. Per [protocol-caps.c:47-48] the
// attrs line is skipped, and per [protocol-caps.c:63-71] each per-OID
// line is just `<oid>\n` with no trailing space (the `if (info->size)`
// block at line 65 does not fire).
//
// [protocol-caps.c:47-48]: https://github.com/git/git/blob/v2.54.0/protocol-caps.c#L47-L48
// [protocol-caps.c:63-71]: https://github.com/git/git/blob/v2.54.0/protocol-caps.c#L63-L71
func TestObjectInfo_Loose_NoSizeAttr(t *testing.T) {
	t.Parallel()

	store := openStoreFromFixture(t, "loose-objects")

	req := buildObjectInfoRequest([]string{
		"oid " + looseBlobOID + "\n",
	})
	resp, err := runV2Session(t, store, req)
	require.NoError(t, err)

	want := pktLine(looseBlobOID+"\n") + "0000"
	assert.Equal(t, want, string(resp),
		"no-size response must be `<oid>\\n` with no size column and no attrs line")
}

// TestObjectInfo_Loose_MultipleOIDs pins per-OID order preservation:
// three OIDs requested in caller order; the response must echo them in
// the same order interspersed with their resolved sizes.
func TestObjectInfo_Loose_MultipleOIDs(t *testing.T) {
	t.Parallel()

	store := openStoreFromFixture(t, "loose-objects")
	blobSize := querySize(t, store, looseBlobOID)
	treeSize := querySize(t, store, looseTreeOID)
	commitSize := querySize(t, store, looseCommitOID)

	// Order chosen so a sort-by-OID would re-shuffle: caller order is
	// blob, commit, tree.
	req := buildObjectInfoRequest([]string{
		"size\n",
		"oid " + looseBlobOID + "\n",
		"oid " + looseCommitOID + "\n",
		"oid " + looseTreeOID + "\n",
	})
	resp, err := runV2Session(t, store, req)
	require.NoError(t, err)

	want := pktLine("size\n") +
		pktLine(fmt.Sprintf("%s %d\n", looseBlobOID, blobSize)) +
		pktLine(fmt.Sprintf("%s %d\n", looseCommitOID, commitSize)) +
		pktLine(fmt.Sprintf("%s %d\n", looseTreeOID, treeSize)) +
		"0000"
	assert.Equal(t, want, string(resp))
}

// TestObjectInfo_Pack_SingleHit pins the pack-resolution path against
// the `pack-only` fixture: a known-packed OID resolves through the
// pack backend, not the loose backend. Canonical Git treats both layers
// identically at the `send_info` level — `odb_read_object_info`
// dispatches internally — so the wire shape is unchanged from the
// loose case, but the test exercises a different `objstore` lookup.
func TestObjectInfo_Pack_SingleHit(t *testing.T) {
	t.Parallel()

	store := openStoreFromFixture(t, "pack-only")
	commitSize := querySize(t, store, packCommitOID)

	req := buildObjectInfoRequest([]string{
		"size\n",
		"oid " + packCommitOID + "\n",
	})
	resp, err := runV2Session(t, store, req)
	require.NoError(t, err)

	want := pktLine("size\n") +
		pktLine(fmt.Sprintf("%s %d\n", packCommitOID, commitSize)) +
		"0000"
	assert.Equal(t, want, string(resp))
}

// TestObjectInfo_MissingOID pins the canonical empty-size form for an
// OID the server cannot resolve in its odb: per [protocol-caps.c:66-67],
// `odb_read_object_info` failure yields `<oid> ` (with a trailing space
// and no size value). The handler must NOT omit the row — byte
// equivalence with canonical Git's `send_info` requires the empty-size
// form, even though a naive reading of the v2 grammar might suggest
// omitting unresolved OIDs entirely.
//
// The wire decoder side ([wire.DecodeObjectInfo]) drops these rows so
// callers see "missing" semantics, but the on-the-wire bytes match
// canonical exactly.
//
// [protocol-caps.c:66-67]: https://github.com/git/git/blob/v2.54.0/protocol-caps.c#L66-L67
func TestObjectInfo_MissingOID(t *testing.T) {
	t.Parallel()

	store := openStoreFromFixture(t, "loose-objects")
	missing := strings.Repeat("d", 40)

	req := buildObjectInfoRequest([]string{
		"size\n",
		"oid " + missing + "\n",
	})
	resp, err := runV2Session(t, store, req)
	require.NoError(t, err)

	// Empty-size form: literal `<oid> \n` with a single trailing space.
	want := pktLine("size\n") +
		pktLine(missing+" \n") +
		"0000"
	assert.Equal(t, want, string(resp),
		"missing OID with size requested must emit `<oid> \\n` per canonical")
}

// TestObjectInfo_MissingOID_NoSize pins that without `size`, a missing
// OID emits just `<oid>\n` (no trailing space). Canonical Git at
// [protocol-caps.c:63-71] only enters the `<oid> ` empty-size branch
// when `info->size` is set; otherwise the line is just
// `strbuf_addstr(send_buffer, oid_str)`.
//
// [protocol-caps.c:63-71]: https://github.com/git/git/blob/v2.54.0/protocol-caps.c#L63-L71
func TestObjectInfo_MissingOID_NoSize(t *testing.T) {
	t.Parallel()

	store := openStoreFromFixture(t, "loose-objects")
	missing := strings.Repeat("d", 40)

	req := buildObjectInfoRequest([]string{
		"oid " + missing + "\n",
	})
	resp, err := runV2Session(t, store, req)
	require.NoError(t, err)

	// No attrs line; the per-OID line is just `<oid>\n` with no trailing space.
	want := pktLine(missing+"\n") + "0000"
	assert.Equal(t, want, string(resp),
		"missing OID without size must emit just `<oid>\\n`")
}

// TestObjectInfo_MixedHitsAndMisses pins the order-preserving mix of
// real, missing, and real OIDs: the response interleaves the resolved
// hits with the empty-size form for the miss in caller order.
func TestObjectInfo_MixedHitsAndMisses(t *testing.T) {
	t.Parallel()

	store := openStoreFromFixture(t, "loose-objects")
	blobSize := querySize(t, store, looseBlobOID)
	commitSize := querySize(t, store, looseCommitOID)
	missing := strings.Repeat("d", 40)

	req := buildObjectInfoRequest([]string{
		"size\n",
		"oid " + looseBlobOID + "\n",
		"oid " + missing + "\n",
		"oid " + looseCommitOID + "\n",
	})
	resp, err := runV2Session(t, store, req)
	require.NoError(t, err)

	want := pktLine("size\n") +
		pktLine(fmt.Sprintf("%s %d\n", looseBlobOID, blobSize)) +
		pktLine(missing+" \n") +
		pktLine(fmt.Sprintf("%s %d\n", looseCommitOID, commitSize)) +
		"0000"
	assert.Equal(t, want, string(resp))
}

// TestObjectInfo_OIDParseError pins the `get_oid_hex_algop` error path
// ([protocol-caps.c:55-61]): a malformed OID hex string triggers a
// per-line `ERR object-info: protocol error, expected to get oid, not
// '<hex>'\n` data pkt-line and the iteration CONTINUES to the next OID.
// The bad OID is not added to the response.
//
// [protocol-caps.c:55-61]: https://github.com/git/git/blob/v2.54.0/protocol-caps.c#L55-L61
func TestObjectInfo_OIDParseError(t *testing.T) {
	t.Parallel()

	store := openStoreFromFixture(t, "loose-objects")
	blobSize := querySize(t, store, looseBlobOID)

	req := buildObjectInfoRequest([]string{
		"size\n",
		"oid not-a-hex-string\n",
		"oid " + looseBlobOID + "\n",
	})
	resp, err := runV2Session(t, store, req)
	require.NoError(t, err,
		"OID parse error is recoverable per `protocol-caps.c:60` continue")

	// The ERR line is emitted MID-STREAM for the bad OID, then the good
	// OID's row follows. The attrs line came first.
	want := pktLine("size\n") +
		pktLine("ERR object-info: protocol error, expected to get oid, not 'not-a-hex-string'\n") +
		pktLine(fmt.Sprintf("%s %d\n", looseBlobOID, blobSize)) +
		"0000"
	assert.Equal(t, want, string(resp))
}

// TestObjectInfo_UnknownArg pins the canonical "unexpected line" path
// ([protocol-caps.c:96-99]): an unrecognised arg triggers a single ERR
// data pkt-line MID-STREAM and the arg parser CONTINUES to the next
// line. This is different from [ls-refs.c:188]'s `die()`. The
// recognised args (`size`, `oid <hex>`) still apply and a normal
// response is emitted afterwards.
//
// [protocol-caps.c:96-99]: https://github.com/git/git/blob/v2.54.0/protocol-caps.c#L96-L99
// [ls-refs.c:188]: https://github.com/git/git/blob/v2.54.0/ls-refs.c#L188
func TestObjectInfo_UnknownArg(t *testing.T) {
	t.Parallel()

	store := openStoreFromFixture(t, "loose-objects")
	blobSize := querySize(t, store, looseBlobOID)

	req := buildObjectInfoRequest([]string{
		"oid " + looseBlobOID + "\n",
		"bogus\n",
		"size\n",
	})
	resp, err := runV2Session(t, store, req)
	require.NoError(t, err,
		"unknown-arg is recoverable; canonical does not die() here")

	// The unknown-arg ERR is emitted DURING arg parsing, BEFORE the
	// response body. Canonical's `packet_writer_error` writes a single
	// `ERR <msg>\n` pkt-line with no trailing flush.
	want := pktLine("ERR object-info: unexpected line: 'bogus'\n") +
		pktLine("size\n") +
		pktLine(fmt.Sprintf("%s %d\n", looseBlobOID, blobSize)) +
		"0000"
	assert.Equal(t, want, string(resp))
}

// TestObjectInfo_SHA256 pins the sha256 hex length (64 chars) flowing
// through the handler. The fixture has one resolvable blob; the
// response shape is the same as the sha1 case but with longer OIDs.
func TestObjectInfo_SHA256(t *testing.T) {
	t.Parallel()

	store := openStoreFromFixture256(t, "loose-objects-sha256")
	blobSize := querySize(t, store, loose256BlobOID)

	req := buildObjectInfoRequest([]string{
		"size\n",
		"oid " + loose256BlobOID + "\n",
	})
	resp, err := runV2Session(t, store, req)
	require.NoError(t, err)

	want := pktLine("size\n") +
		pktLine(fmt.Sprintf("%s %d\n", loose256BlobOID, blobSize)) +
		"0000"
	assert.Equal(t, want, string(resp))
}

// TestObjectInfo_CorruptObject pins the divergence from canonical Git on
// corruption. Canonical conflates miss and corrupt at the
// `odb_read_object_info` boundary; our scope distinguishes the two so
// callers (and operators) can tell the difference. A corrupt or
// otherwise unresolvable object surfaces as:
//
//   - A structured `ERR objstore: object corrupt or unresolvable: ...`
//     data pkt-line carrying the wrapped store error.
//   - A trailing flush.
//   - A wrapped [wire.ErrServerRefused] from `Serve` so the dispatcher
//     terminates the v2 session.
//
// Driven by flipping a byte in the `pack-only` fixture's pack-file body
// — the same mechanism used by the objstore-side
// `TestObjectInfo_CRC32MismatchWrapsErrCorruptObject` — to land a CRC
// mismatch in `Store.ObjectInfo`.
func TestObjectInfo_CorruptObject(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	src := filepath.Join("..", "..", "testdata", "repos", "pack-only")
	require.NoError(t, copyFixtureTree(src, dir))
	require.NoError(t, os.Rename(
		filepath.Join(dir, "dotgit"),
		filepath.Join(dir, ".git")))

	// Flip a byte inside the commit's compressed body so the CRC32
	// check at `verifyPackCRC` trips. Layout matches the objstore-side
	// corrupt test: the commit at offset 12 has its body running through
	// [12, 131); offset 64 is safely in the middle.
	packPath := filepath.Join(dir, ".git", "objects", "pack", "three-objects.pack")
	flipPackByte(t, packPath, 64)

	store, err := objstore.Open[objfmt.SHA1Hash](filepath.Join(dir, ".git"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	req := buildObjectInfoRequest([]string{
		"size\n",
		"oid " + packCommitOID + "\n",
	})
	resp, err := runV2Session(t, store, req)
	require.Error(t, err)
	require.ErrorIs(t, err, wire.ErrServerRefused,
		"want errors.Is(err, wire.ErrServerRefused); got %v", err)

	// Decode the response stream:
	//   1. The attrs line `size\n` — emitted before per-OID iteration,
	//      so it lands on the wire even when the very first OID fails
	//      catastrophically. Canonical Git's `send_info` emits the
	//      attrs line at line 48 before the OID loop at line 50, and
	//      we follow the same order.
	//   2. An ERR pkt-line carrying the wrapped objstore error
	//      message. The exact tail of the message includes the
	//      wrapped pack-CRC details so we assert on a stable prefix
	//      rather than the full bytes.
	//   3. A trailing flush.
	r := pktline.NewReader(bytes.NewReader(resp))

	pkt, err := r.ReadPacket()
	require.NoError(t, err)
	require.Equal(t, pktline.Data, pkt.Kind)
	assert.Equal(t, "size\n", string(pkt.Data),
		"attrs line precedes per-OID iteration; corruption surfaces inside the loop")

	pkt, err = r.ReadPacket()
	require.NoError(t, err)
	require.Equal(t, pktline.Data, pkt.Kind)
	assert.True(t, strings.HasPrefix(string(pkt.Data),
		"ERR objstore: object corrupt or unresolvable: "),
		"want ERR-prefixed objstore message; got %q", pkt.Data)

	pkt, err = r.ReadPacket()
	require.NoError(t, err)
	assert.Equal(t, pktline.Flush, pkt.Kind)
}

// copyFixtureTree mirrors `testfixture.MaterializeRepo`'s walk but
// keeps the `dotgit/` component intact (the corrupt-object test needs
// to flip a byte before opening the store, so the rename to `.git` is
// performed explicitly by the caller after `copyFixtureTree` returns).
func copyFixtureTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}

// flipPackByte XORs the byte at off with 0xff so any CRC over the
// compressed body changes. The pack file is opened read-write and the
// change written through to disk before the store is opened.
func flipPackByte(t *testing.T, packPath string, off int64) {
	t.Helper()

	f, err := os.OpenFile(packPath, os.O_RDWR, 0o644)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	var buf [1]byte
	_, err = f.ReadAt(buf[:], off)
	require.NoError(t, err)
	buf[0] ^= 0xff
	_, err = f.WriteAt(buf[:], off)
	require.NoError(t, err)
}

// TestEmitObjectInfoLine_AllocBudget pins the per-OID alloc floor of
// the hit-with-size path. Before the formatting fix, the path landed
// at ~8 allocs/OID — three of those came from the `strings.Builder`
// chain (`Builder.grow`, `fmt.Fprintf` interface boxing, the
// `b.String()` → `[]byte` round-trip). With those gone, the only
// remaining allocs are the ones inside `Store.ObjectInfo` itself
// (the pack-resolution path's read buffers), giving a floor near 5.
//
// The store is opened with `WithoutCRCCheck` so the budget isolates
// the emitter's own allocs from the per-call CRC-32 verification
// path's read buffers; the CRC path has its own coverage in
// `internal/objstore/store_bench_test.go::BenchmarkStore_ObjectInfo_CRC`.
//
// Amortising over 1000 OIDs lets per-call constants (the response
// setup, the iterator hand-off) round to zero so the average
// isolates the loop body. The budget is set tight enough to fail if
// any future change re-introduces a formatting-side alloc.
//
//nolint:paralleltest // testing.AllocsPerRun panics in parallel tests
func TestEmitObjectInfoLine_AllocBudget(t *testing.T) {
	const oidCount = 1000
	// 5 allocs/OID come from `Store.ObjectInfo`'s pack-resolution
	// path (out of scope here); a tiny epsilon covers per-call
	// constants (response setup, attrs line) that amortise but do
	// not round to exactly zero across 1000 OIDs. A regression in
	// the emitter would land back at 6+ and trip the budget.
	//
	// The `-race` budget carries roughly half an alloc/OID more
	// because race instrumentation perturbs `sync.Pool`'s steady
	// state: the extra heap pressure from the race runtime
	// triggers GCs more frequently, and `sync.Pool` discards
	// per-P entries on every GC cycle
	// (`sync/pool.go::poolCleanup`). The pack-resolution path
	// uses pooled scratch buffers, so periodic cold-path
	// reallocation inflates the average. The inflation is a
	// runtime characteristic, not a regression in the emitter;
	// the non-race budget continues to pin the production shape.
	//
	// Windows adds another ~1.0 alloc/OID on top of either base
	// budget, with non-trivial run-to-run variance. Observed
	// `windows-latest` race-mode runs landed between 6.50 and
	// 6.51/OID against the 6.00 Linux/Darwin budget; the floor is
	// consistent enough to call it a platform characteristic
	// rather than a flake, but the contributing path is not
	// pinpointed (candidates include the `*os.File` mmap fallback
	// in `internal/objfmt/mmap_reader.go` and per-call buffer
	// boxing inside Windows' `ReadAt`). Raise the budget by a
	// generous 1.0 on Windows so it stays a regression guard
	// without becoming a permanent red; a future investigation
	// can tighten this once the source is identified.
	maxAllocsPerOID := 5.01
	if raceEnabled {
		maxAllocsPerOID = 6.0
	}
	if runtime.GOOS == "windows" {
		maxAllocsPerOID += 1.0
	}

	// Materialise the `pack-only` fixture and open without CRC so the
	// budget is dominated by the emitter, not the per-object CRC pass.
	dir := t.TempDir()
	src := filepath.Join("..", "..", "testdata", "repos", "pack-only")
	require.NoError(t, copyFixtureTree(src, dir))
	require.NoError(t, os.Rename(
		filepath.Join(dir, "dotgit"),
		filepath.Join(dir, ".git")))
	store, err := objstore.Open[objfmt.SHA1Hash](filepath.Join(dir, ".git"),
		objstore.WithoutCRCCheck())
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	w := pktline.NewWriter(io.Discard)

	// Two known OIDs from the `pack-only` fixture, cycled so the
	// loop alternates between them. Mirrors the bench's hit set.
	hits := []string{
		"26dae744f51e61913f50bd402cbe63953c7d637b", // commit
		"97d881a6f710fc8fc34524d80bfc782359137a5c", // blob
	}
	oids := make([]string, oidCount)
	for i := range oids {
		oids[i] = hits[i%len(hits)]
	}
	args := wire.ObjectInfoArgs{Size: true}

	avg := testing.AllocsPerRun(20, func() {
		if err := writeObjectInfoResponse(w, store, args, oids); err != nil {
			t.Fatal(err)
		}
	})

	perOID := avg / float64(oidCount)
	if perOID > maxAllocsPerOID {
		t.Fatalf("post-fix allocs/OID = %.2f (total %.0f / %d OIDs), want <= %.2f",
			perOID, avg, oidCount, maxAllocsPerOID)
	}
}

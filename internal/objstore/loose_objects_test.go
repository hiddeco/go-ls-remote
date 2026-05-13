package objstore

import (
	"bytes"
	"compress/zlib"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
)

// Stable OIDs for the `loose-objects` fixture. The fixture script
// pins author/committer identity, dates, and gpg-signing off so these
// values do not drift across regenerations.
const (
	looseFixtureBlobOID   = "393a7c05257a543bc1369537c7fdb2851dc04b11"
	looseFixtureTreeOID   = "4cb61db1e9094ba0e955298fcbd038ec69bc7a38"
	looseFixtureCommitOID = "9a1288dcf7ead9936f178d8dd8a1f14c81eafbf9"
	looseFixtureTagOID    = "855c1386ff144601eb847df1b4e59057ca415883"

	looseFixtureBlobBody = "hello loose object world\n"
	looseFixtureBlobSize = int64(len(looseFixtureBlobBody))
)

// SHA-256 sibling fixture: same blob content, different fanout. Only
// the blob OID is referenced from tests today; the others are kept on
// the fixture itself (see `testdata/_gen/repos.sh`) for the other
// type-variants once a SHA-256 type sweep is added.
const loose256FixtureBlobOID = "c60061d62336c6b760e2c4ec860873a193c61662e4f2a6aa5cb3cbaf9339cd10"

// openLooseObjectsFromFixture materializes the named fixture and
// returns the SHA-1 `looseObjects` backend rooted at its `.git/`.
// SHA-256 tests use [openLooseObjectsFromFixture256].
func openLooseObjectsFromFixture(t *testing.T, name string, algo objfmt.Algo) *looseObjects[objfmt.SHA1Hash] {
	t.Helper()
	require.Equal(t, objfmt.SHA1, algo,
		"openLooseObjectsFromFixture only supports SHA-1; use openLooseObjectsFromFixture256")
	root := materializeFixture(t, name)
	gitDir := filepath.Join(root, ".git")
	l, err := openLoose[objfmt.SHA1Hash](gitDir, algo)
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })
	return l
}

// openLooseObjectsFromFixture256 is the SHA-256 sibling. Kept split
// rather than parameterised so each callsite reads at a glance.
func openLooseObjectsFromFixture256(t *testing.T, name string) *looseObjects[objfmt.SHA256Hash] {
	t.Helper()
	root := materializeFixture(t, name)
	gitDir := filepath.Join(root, ".git")
	l, err := openLoose[objfmt.SHA256Hash](gitDir, objfmt.SHA256)
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })
	return l
}

func TestLooseObjects_FindHitBlob(t *testing.T) {
	t.Parallel()
	// Hit path: the blob exists under its `aa/rest` fanout. Verify the
	// header fields and that draining body returns the canonical payload.
	l := openLooseObjectsFromFixture(t, "loose-objects", objfmt.SHA1)

	typ, size, body, ok, err := l.Find(hashFromHex(t, looseFixtureBlobOID, objfmt.SHA1))
	require.NoError(t, err)
	require.True(t, ok)
	require.NotNil(t, body)
	t.Cleanup(func() { _ = body.Close() })

	assert.Equal(t, objfmt.TypeBlob, typ)
	assert.Equal(t, looseFixtureBlobSize, size)

	got, err := io.ReadAll(body)
	require.NoError(t, err)
	assert.Equal(t, looseFixtureBlobBody, string(got))
}

func TestLooseObjects_FindMissReturnsNilError(t *testing.T) {
	t.Parallel()
	// Miss path: a hash whose fanout file does not exist must surface as
	// (false, nil) — never `os.ErrNotExist`. Backends compose; the
	// caller decides whether a miss here is fatal after checking packs
	// and alternates.
	l := openLooseObjectsFromFixture(t, "loose-objects", objfmt.SHA1)

	missing := hashFromHex(t,
		"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", objfmt.SHA1)
	typ, size, body, ok, err := l.Find(missing)
	require.NoError(t, err, "miss must be a nil error, not os.ErrNotExist")
	assert.False(t, ok)
	assert.Nil(t, body)
	assert.Zero(t, typ)
	assert.Zero(t, size)
}

func TestLooseObjects_FindMissingFanoutDirectory(t *testing.T) {
	t.Parallel()
	// Same observable shape as the miss case above, but exercises the
	// branch where the `aa/` subdirectory itself never existed — the
	// canonical state right after `git init` for any fanout bucket the
	// repo has not yet populated. ENOENT applies to the directory walk,
	// not just the file, and the backend must collapse both into a
	// clean miss.
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "objects"), 0o755))
	l, err := openLoose[objfmt.SHA1Hash](dir, objfmt.SHA1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })

	h := hashFromHex(t,
		"abababababababababababababababababababab", objfmt.SHA1)
	_, _, body, ok, err := l.Find(h)
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Nil(t, body)
}

func TestLooseObjects_FindPermissionError(t *testing.T) {
	t.Parallel()
	// A permission-denied open is a real backend failure, not a miss.
	// On systems where the test process is root (CI containers without
	// USER set, occasionally) chmod-0000 reads still succeed; skip
	// rather than emit a flaky assertion.
	if os.Geteuid() == 0 {
		t.Skip("running as root; chmod-0000 cannot block reads")
	}

	dir := t.TempDir()
	objsDir := filepath.Join(dir, "objects", "ab")
	require.NoError(t, os.MkdirAll(objsDir, 0o755))

	// Synthetic loose object: the body is valid zlib, but the test
	// never gets that far — chmod 0o000 trips the open.
	target := filepath.Join(objsDir,
		"abababababababababababababababababababab"[2:])
	require.NoError(t, os.WriteFile(target, validLooseBytes(t, "blob", "x"), 0o644))
	require.NoError(t, os.Chmod(target, 0o000))
	t.Cleanup(func() { _ = os.Chmod(target, 0o644) })

	l, err := openLoose[objfmt.SHA1Hash](dir, objfmt.SHA1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })

	_, _, body, ok, err := l.Find(hashFromHex(t,
		"abababababababababababababababababababab", objfmt.SHA1))
	require.Error(t, err, "permission denied must surface, not collapse to a miss")
	assert.False(t, ok)
	assert.Nil(t, body)
	// Different platforms wrap the syscall error differently
	// (`syscall.EACCES` on Linux / Darwin); `errors.Is` against
	// `fs.ErrPermission` is the portable check.
	assert.ErrorIs(t, err, fs.ErrPermission,
		"expected fs.ErrPermission in chain, got %v", err)
}

func TestLooseObjects_FindCorruptObjectWrapsErrCorruptObject(t *testing.T) {
	t.Parallel()
	// A loose-object file whose zlib stream is malformed must surface
	// as a real error chained through [ErrCorruptObject] — never a
	// silent miss, never a panic. Synthetic bytes live in a tempdir so
	// the committed fixture set never carries broken artefacts.
	dir := t.TempDir()
	objsDir := filepath.Join(dir, "objects", "ab")
	require.NoError(t, os.MkdirAll(objsDir, 0o755))

	target := filepath.Join(objsDir,
		"abababababababababababababababababababab"[2:])
	require.NoError(t, os.WriteFile(target, []byte("this is not a zlib stream"), 0o644))

	l, err := openLoose[objfmt.SHA1Hash](dir, objfmt.SHA1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })

	_, _, body, ok, err := l.Find(hashFromHex(t,
		"abababababababababababababababababababab", objfmt.SHA1))
	require.Error(t, err)
	assert.False(t, ok)
	assert.Nil(t, body)
	assert.ErrorIs(t, err, ErrCorruptObject,
		"expected ErrCorruptObject in chain, got %v", err)
}

func TestLooseObjects_FindSHA256Lookup(t *testing.T) {
	t.Parallel()
	// SHA-256 fanout: the first two of 64 hex chars select the
	// directory; the remaining 62 form the file name. The blob payload
	// matches the SHA-1 fixture so the assertion has a stable baseline.
	l := openLooseObjectsFromFixture256(t, "loose-objects-sha256")

	typ, size, body, ok, err := l.Find(
		hashFromHex256(t, loose256FixtureBlobOID))
	require.NoError(t, err)
	require.True(t, ok)
	t.Cleanup(func() { _ = body.Close() })

	assert.Equal(t, objfmt.TypeBlob, typ)
	assert.Equal(t, looseFixtureBlobSize, size)

	got, err := io.ReadAll(body)
	require.NoError(t, err)
	assert.Equal(t, looseFixtureBlobBody, string(got))
}

func TestLooseObjects_FindAllTypeVariants(t *testing.T) {
	t.Parallel()
	// Each of the four [objfmt.ObjectType] non-delta variants resolves
	// through the same code path; the test fixes the type-name parser
	// against silent regressions in either direction.
	l := openLooseObjectsFromFixture(t, "loose-objects", objfmt.SHA1)

	cases := []struct {
		name string
		oid  string
		want objfmt.ObjectType
	}{
		{"blob", looseFixtureBlobOID, objfmt.TypeBlob},
		{"tree", looseFixtureTreeOID, objfmt.TypeTree},
		{"commit", looseFixtureCommitOID, objfmt.TypeCommit},
		{"tag", looseFixtureTagOID, objfmt.TypeTag},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			typ, _, body, ok, err := l.Find(hashFromHex(t, tc.oid, objfmt.SHA1))
			require.NoError(t, err)
			require.True(t, ok)
			t.Cleanup(func() { _ = body.Close() })
			assert.Equal(t, tc.want, typ)
		})
	}
}

func TestLooseObjects_BodyCloseReleasesHandles(t *testing.T) {
	t.Parallel()
	// One body.Close() must release both the zlib decoder and the
	// underlying file. A follow-up read after the body is fully drained
	// must report an error (the file handle is closed, the zlib decoder
	// has nothing more to yield) — anything else would mask a handle
	// leak. The body is drained first so the assertion does not race
	// against bufio's internal buffer inside the zlib reader.
	l := openLooseObjectsFromFixture(t, "loose-objects", objfmt.SHA1)

	_, _, body, ok, err := l.Find(hashFromHex(t, looseFixtureBlobOID, objfmt.SHA1))
	require.NoError(t, err)
	require.True(t, ok)

	got, err := io.ReadAll(body)
	require.NoError(t, err)
	require.Equal(t, looseFixtureBlobBody, string(got))

	require.NoError(t, body.Close(), "first Close must succeed cleanly")

	buf := make([]byte, 8)
	_, readErr := body.Read(buf)
	assert.Error(t, readErr,
		"reading after drain+Close must error; otherwise the file handle is still alive")
}

func TestLooseObjects_FindThroughOpen(t *testing.T) {
	t.Parallel()
	// End-to-end: `Open` must thread `cfg.algo` into `openLoose` so
	// `s.loose.Find` resolves OIDs through the resulting Store[objfmt.SHA1Hash]. The
	// assertion mirrors the hit case on a fixture that is wired through
	// the full opener path, not just the backend constructor.
	root := materializeFixture(t, "loose-objects")
	s, err := Open[objfmt.SHA1Hash](root)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	typ, _, body, ok, err := s.loose.Find(hashFromHex(t, looseFixtureBlobOID, objfmt.SHA1))
	require.NoError(t, err)
	require.True(t, ok)
	t.Cleanup(func() { _ = body.Close() })
	assert.Equal(t, objfmt.TypeBlob, typ)
}

func TestLooseObjects_FindThroughOpenSHA256(t *testing.T) {
	t.Parallel()
	// SHA-256 sibling of the end-to-end check. Confirms `cfg.algo`
	// propagates from `extensions.objectFormat = sha256` all the way
	// into the loose-object path computation.
	root := materializeFixture(t, "loose-objects-sha256")
	s, err := Open[objfmt.SHA256Hash](root)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	require.Equal(t, objfmt.SHA256, s.Algo())

	typ, _, body, ok, err := s.loose.Find(hashFromHex256(t, loose256FixtureBlobOID))
	require.NoError(t, err)
	require.True(t, ok)
	t.Cleanup(func() { _ = body.Close() })
	assert.Equal(t, objfmt.TypeBlob, typ)
}

// validLooseBytes encodes the canonical loose-object framing for a
// trivial body. Used by the permission-error test where the test
// process never reads the bytes — the open trips first — but the file
// content stays plausibly real so a future expansion of the test
// (e.g. removing the chmod) does not silently corrupt the assertion.
func validLooseBytes(t *testing.T, typeName, body string) []byte {
	t.Helper()
	header := typeName + " " + strconv.Itoa(len(body)) + "\x00"

	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	_, err := zw.Write([]byte(header + body))
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

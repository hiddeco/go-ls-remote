package objstore

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// reftableContentFixtureMain is the OID committed by the
// `with-reftable-content` fixture's single commit. The reftable
// generator pins author/committer dates so the OID is stable across
// regenerations; updating the fixture means updating this constant.
const reftableContentFixtureMain = "dbe62b7be27170912462463476422dff1d92c24e"

// reftableDetachedFixtureHead is the OID HEAD is bound to in the
// `with-reftable-detached` fixture (also the value of refs/heads/main
// in the same fixture, since `update-ref --no-deref HEAD <oid>` does
// not touch main). Same stability guarantee as
// [reftableContentFixtureMain].
const reftableDetachedFixtureHead = "735d9012eb4e10ac1ab1d19e680281a6edc54ec2"

// openReftableFromFixture materializes the named reftable-backed
// fixture and opens the reftable backend on it. Centralises the
// gitDir/commonDir plumbing every reftable-backend test repeats.
func openReftableFromFixture(t *testing.T, name string) *reftableBackend[objfmt.SHA1Hash] {
	t.Helper()
	root := materializeFixture(t, name)
	gitDir := filepath.Join(root, ".git")
	b, err := openReftableBackend[objfmt.SHA1Hash](gitDir, gitDir, "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })
	return b
}

// collectReftableRefs drains the iterator into a slice. The reftable
// stack already yields refs in lexical order, so the slice is compared
// directly: a reordering regression surfaces as a diff.
func collectReftableRefs(t *testing.T, b *reftableBackend[objfmt.SHA1Hash]) []RefEntry[objfmt.SHA1Hash] {
	t.Helper()
	var out []RefEntry[objfmt.SHA1Hash]
	for entry, err := range b.IterRefs() {
		require.NoError(t, err)
		out = append(out, entry)
	}
	return out
}

func TestReftableBackend_IterRefs_YieldsContentExcludingHEAD(t *testing.T) {
	// The `with-reftable-content` fixture carries HEAD (a symref) plus
	// refs/heads/main as the only value record. IterRefs surfaces only
	// the latter — HEAD belongs to Head(), and symrefs other than HEAD
	// are intentionally hidden (same precedent as looseRefs[objfmt.SHA1Hash]).
	b := openReftableFromFixture(t, "with-reftable-content")
	got := collectReftableRefs(t, b)

	want := []RefEntry[objfmt.SHA1Hash]{
		{
			Name:      "refs/heads/main",
			OID:       hashFromHex(t, reftableContentFixtureMain, objfmt.SHA1),
			PeelKnown: true,
		},
	}
	assert.Equal(t, want, got)

	names := make([]string, 0, len(got))
	for _, e := range got {
		names = append(names, e.Name)
	}
	assert.True(t, slices.IsSorted(names), "IterRefs output must be sorted")
}

func TestReftableBackend_Head_SymrefToExistingTarget(t *testing.T) {
	// The fixture's HEAD is a symref pointing at refs/heads/main, which
	// the same stack carries as a value record. Head() must surface both
	// the target name and the resolved OID, with Unborn = false.
	b := openReftableFromFixture(t, "with-reftable-content")

	head, err := b.Head()
	require.NoError(t, err)
	assert.Equal(t, "refs/heads/main", head.Symref)
	assert.Equal(t, hashFromHex(t, reftableContentFixtureMain, objfmt.SHA1), head.OID)
	assert.False(t, head.Unborn)
}

func TestReftableBackend_Head_SymrefToMissingTargetIsUnborn(t *testing.T) {
	// `git init --ref-format=reftable` writes a HEAD record bound to
	// refs/heads/main even when no commit has landed yet. Head() must
	// report Symref set, OID zero, Unborn true — the reftable analogue
	// of looseRefs[objfmt.SHA1Hash]' unborn-branch shape.
	b := openReftableFromFixture(t, "with-reftable-unborn")

	head, err := b.Head()
	require.NoError(t, err)
	assert.Equal(t, "refs/heads/main", head.Symref)
	assert.Equal(t, objfmt.SHA1Hash{}, head.OID)
	assert.True(t, head.Unborn)
}

func TestReftableBackend_Head_Detached(t *testing.T) {
	// `git update-ref --no-deref HEAD <oid>` rewrites HEAD to a value
	// record (no Target). The backend reports it as a detached HEAD:
	// Symref empty, OID populated, Unborn false.
	b := openReftableFromFixture(t, "with-reftable-detached")

	head, err := b.Head()
	require.NoError(t, err)
	assert.Equal(t, "", head.Symref)
	assert.Equal(t, hashFromHex(t, reftableDetachedFixtureHead, objfmt.SHA1), head.OID)
	assert.False(t, head.Unborn)
}

func TestReftableBackend_Head_MissingRecordIsCorrupt(t *testing.T) {
	// Synthesize a stack without any HEAD record by writing an
	// empty `tables.list`. Canonical Git always writes a HEAD record at
	// `git init`, so a missing one is corruption — the backend must
	// wrap [ErrCorruptObject] rather than silently report unborn.
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "reftable"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "reftable", "tables.list"), nil, 0o644))

	b, err := openReftableBackend[objfmt.SHA1Hash](dir, dir, "")
	require.Error(t, err)
	assert.Nil(t, b)
	assert.True(t, errors.Is(err, ErrCorruptObject),
		"expected ErrCorruptObject, got %v", err)
}

func TestReftableBackend_CustomLocation_RelativeToGitDir(t *testing.T) {
	// `extensions.refStorage = reftable://<location>` resolves a
	// relative payload against gitDir per canonical Git's
	// `Documentation/config/extensions.adoc` § extensions.refStorage.
	// The fixture's reftable lives under the canonical `reftable/`
	// directory; copy it into a sibling `alt-reftable/` and confirm the
	// opener picks it up via the relative payload.
	root := materializeFixture(t, "with-reftable-content")
	gitDir := filepath.Join(root, ".git")
	src := filepath.Join(gitDir, "reftable")
	dst := filepath.Join(gitDir, "alt-reftable")
	require.NoError(t, copyDirContents(src, dst))

	b, err := openReftableBackend[objfmt.SHA1Hash](gitDir, gitDir, "./alt-reftable")
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })

	got := collectReftableRefs(t, b)
	want := []RefEntry[objfmt.SHA1Hash]{
		{
			Name:      "refs/heads/main",
			OID:       hashFromHex(t, reftableContentFixtureMain, objfmt.SHA1),
			PeelKnown: true,
		},
	}
	assert.Equal(t, want, got)
}

func TestReftableBackend_CustomLocation_AbsoluteIsVerbatim(t *testing.T) {
	// An absolute payload (`reftable:///abs/path`) is consumed verbatim;
	// relative resolution against gitDir does not kick in.
	root := materializeFixture(t, "with-reftable-content")
	gitDir := filepath.Join(root, ".git")

	abs := t.TempDir()
	require.NoError(t, copyDirContents(filepath.Join(gitDir, "reftable"), abs))

	b, err := openReftableBackend[objfmt.SHA1Hash](gitDir, gitDir, abs)
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })

	got := collectReftableRefs(t, b)
	want := []RefEntry[objfmt.SHA1Hash]{
		{
			Name:      "refs/heads/main",
			OID:       hashFromHex(t, reftableContentFixtureMain, objfmt.SHA1),
			PeelKnown: true,
		},
	}
	assert.Equal(t, want, got)
}

func TestReftableBackend_CommonDirVsGitDir_DefaultLocationUsesCommonDir(t *testing.T) {
	// With an empty location, the canonical layout puts the reftable
	// stack under `<commonDir>/reftable/`. Synthesise a worktree-shaped
	// case: gitDir is a sibling of commonDir, the reftable bytes live
	// under commonDir, and the opener must reach across to find them.
	srcRoot := materializeFixture(t, "with-reftable-content")
	srcGit := filepath.Join(srcRoot, ".git")

	scratch := t.TempDir()
	commonDir := filepath.Join(scratch, "common")
	gitDir := filepath.Join(scratch, "gitdir")
	require.NoError(t, os.MkdirAll(commonDir, 0o755))
	require.NoError(t, os.MkdirAll(gitDir, 0o755))
	require.NoError(t, copyDirContents(filepath.Join(srcGit, "reftable"), filepath.Join(commonDir, "reftable")))

	b, err := openReftableBackend[objfmt.SHA1Hash](gitDir, commonDir, "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })

	got := collectReftableRefs(t, b)
	want := []RefEntry[objfmt.SHA1Hash]{
		{
			Name:      "refs/heads/main",
			OID:       hashFromHex(t, reftableContentFixtureMain, objfmt.SHA1),
			PeelKnown: true,
		},
	}
	assert.Equal(t, want, got)
}

func TestReftableBackend_OpenMissingDirReturnsError(t *testing.T) {
	// A non-existent reftable directory must surface as an error
	// (canonical Git refuses to operate on a missing stack); the
	// constructor must wrap rather than silently succeed.
	b, err := openReftableBackend[objfmt.SHA1Hash](t.TempDir(), t.TempDir(), "")
	require.Error(t, err)
	assert.Nil(t, b)
}

func TestReftableBackend_OpenViaStore_YieldsPopulatedRefs(t *testing.T) {
	// End-to-end: `Open` on the populated fixture selects the reftable
	// backend, and `Store[objfmt.SHA1Hash].IterRefs` surfaces the same refs as the direct
	// backend test. Locks in the wiring through `openRefBackend`.
	root := materializeFixture(t, "with-reftable-content")

	s, err := Open[objfmt.SHA1Hash](root)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	_, ok := s.refs.(*reftableBackend[objfmt.SHA1Hash])
	require.True(t, ok, "want reftable backend, got %T", s.refs)

	var got []RefEntry[objfmt.SHA1Hash]
	for entry, err := range s.IterRefs() {
		require.NoError(t, err)
		got = append(got, entry)
	}
	want := []RefEntry[objfmt.SHA1Hash]{
		{
			Name:      "refs/heads/main",
			OID:       hashFromHex(t, reftableContentFixtureMain, objfmt.SHA1),
			PeelKnown: true,
		},
	}
	assert.Equal(t, want, got)

	head, err := s.Head()
	require.NoError(t, err)
	assert.Equal(t, "refs/heads/main", head.Symref)
	assert.Equal(t, hashFromHex(t, reftableContentFixtureMain, objfmt.SHA1), head.OID)
	assert.False(t, head.Unborn)
}

func TestReftableBackend_IterRefs_PeelKnownAlwaysTrue(t *testing.T) {
	// Reftable records always carry the peel slot (zero or set), so the
	// merged-view lift must surface PeelKnown=true for every entry it
	// yields. The fixture's only non-HEAD ref is a commit, hence Peeled
	// is the zero hash.
	b := openReftableFromFixture(t, "with-reftable-content")
	got := collectReftableRefs(t, b)

	require.Len(t, got, 1)
	assert.True(t, got[0].PeelKnown,
		"reftable entry must surface PeelKnown=true")
	assert.Equal(t, objfmt.SHA1Hash{}, got[0].Peeled)
}

func TestReftableBackend_Lookup_KnownRef(t *testing.T) {
	// The fixture's `refs/heads/main` value record carries no peel slot
	// (it is a commit). Lookup returns PeelKnown=true, Peeled=zero.
	b := openReftableFromFixture(t, "with-reftable-content")

	entry, found, err := b.Lookup("refs/heads/main")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "refs/heads/main", entry.Name)
	assert.Equal(t,
		hashFromHex(t, reftableContentFixtureMain, objfmt.SHA1), entry.OID)
	assert.True(t, entry.PeelKnown)
	assert.Equal(t, objfmt.SHA1Hash{}, entry.Peeled)
}

func TestReftableBackend_Lookup_MissingRef(t *testing.T) {
	b := openReftableFromFixture(t, "with-reftable-content")

	entry, found, err := b.Lookup("refs/heads/does-not-exist")
	require.NoError(t, err)
	assert.False(t, found)
	assert.Equal(t, RefEntry[objfmt.SHA1Hash]{}, entry)
}

func TestReftableBackend_Lookup_HEADHidden(t *testing.T) {
	// HEAD is exposed through Head(), not Lookup. A direct Lookup("HEAD")
	// must miss so callers cannot accidentally hand HEAD's symref payload
	// to peel logic that expects a value record.
	b := openReftableFromFixture(t, "with-reftable-content")

	_, found, err := b.Lookup("HEAD")
	require.NoError(t, err)
	assert.False(t, found, "HEAD must not surface through Lookup")
}

// copyDirContents copies every regular file under src into dst,
// creating dst if necessary. Used by the custom-location tests to
// stage reftable bytes outside the canonical layout. Subdirectories
// are not expected (a reftable stack is a flat directory of `.ref`
// files plus `tables.list`); the helper fails the test if it sees one.
func copyDirContents(src, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			return errors.New("copyDirContents: nested directories unexpected in a reftable stack")
		}
		raw, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), raw, 0o644); err != nil {
			return err
		}
	}
	return nil
}

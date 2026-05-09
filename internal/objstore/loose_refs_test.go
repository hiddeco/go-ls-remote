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

// hashFromHex parses s under algo or fails the test. Used in tests
// where the OID is a fixture-known synthetic value rather than something
// derived from a real object.
func hashFromHex(t *testing.T, s string, algo objfmt.Algo) objfmt.Hash {
	t.Helper()
	h, err := objfmt.ParseHex(s, algo)
	require.NoError(t, err)
	return h
}

// collectRefs drains the iterator into a deterministic slice. The
// backend yields refs in lexical order; tests compare the slice
// directly rather than re-sorting, so a regression in ordering is
// surfaced as a diff rather than masked.
func collectRefs(t *testing.T, r *looseRefs) []RefEntry {
	t.Helper()
	var out []RefEntry
	for entry, err := range r.IterRefs() {
		require.NoError(t, err)
		out = append(out, entry)
	}
	return out
}

// openLooseFromFixture materializes the named fixture and opens the
// loose-refs backend on it. The helper centralises the gitdir + algo
// plumbing every loose-refs test needs.
func openLooseFromFixture(t *testing.T, name string, algo objfmt.Algo) *looseRefs {
	t.Helper()
	root := materializeFixture(t, name)
	gitDir := filepath.Join(root, ".git")
	r, err := openLooseRefs(gitDir, gitDir, algo)
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func TestLooseRefs_LooseOnly_IterRefsSorted(t *testing.T) {
	// All three loose refs surface in lexical order with the synthetic
	// OIDs the fixture committed. Subdirectory entries
	// (`refs/heads/feature/x`) walk correctly.
	r := openLooseFromFixture(t, "loose-only", objfmt.SHA1)
	got := collectRefs(t, r)

	want := []RefEntry{
		{Name: "refs/heads/feature/x", OID: hashFromHex(t, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", objfmt.SHA1)},
		{Name: "refs/heads/main", OID: hashFromHex(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", objfmt.SHA1)},
		{Name: "refs/tags/v1", OID: hashFromHex(t, "cccccccccccccccccccccccccccccccccccccccc", objfmt.SHA1)},
	}
	assert.Equal(t, want, got)
}

func TestLooseRefs_PackedOnly_IterRefs(t *testing.T) {
	// Only `packed-refs` carries entries. The parser must surface them
	// all, the trait header sets `peeled` and `fully-peeled`, and the
	// tag's `^peel` line populates packedEntry.peeled.
	r := openLooseFromFixture(t, "packed-only", objfmt.SHA1)
	got := collectRefs(t, r)

	want := []RefEntry{
		{Name: "refs/heads/main", OID: hashFromHex(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", objfmt.SHA1)},
		{Name: "refs/tags/v1", OID: hashFromHex(t, "cccccccccccccccccccccccccccccccccccccccc", objfmt.SHA1)},
	}
	assert.Equal(t, want, got)
}

func TestLooseRefs_Mixed_LooseShadowsPacked(t *testing.T) {
	// `refs/heads/main` exists in both: loose carries OID-C, packed
	// carries OID-A. The loose entry must win, and the packed-only
	// `refs/heads/old` must still surface.
	r := openLooseFromFixture(t, "mixed", objfmt.SHA1)
	got := collectRefs(t, r)

	wantLoose := hashFromHex(t, "3333333333333333333333333333333333333333", objfmt.SHA1)
	wantOld := hashFromHex(t, "2222222222222222222222222222222222222222", objfmt.SHA1)
	want := []RefEntry{
		{Name: "refs/heads/main", OID: wantLoose},
		{Name: "refs/heads/old", OID: wantOld},
	}
	assert.Equal(t, want, got)

	// Loose overrides drop packed peel information: even though the
	// packed-refs file in this fixture advertises peel traits, the
	// loose-shadowed main carries no peel hint.
	_, peelKnown, ok := r.peelHint("refs/heads/main")
	require.True(t, ok)
	assert.False(t, peelKnown,
		"loose override must drop packed peel hint")
}

func TestLooseRefs_Head_SymrefToExistingRef(t *testing.T) {
	// HEAD = `ref: refs/heads/main`, and `refs/heads/main` exists as a
	// loose ref carrying OID `aaaa...`. Symref + OID populated, Unborn
	// false.
	r := openLooseFromFixture(t, "loose-only", objfmt.SHA1)

	head, err := r.Head()
	require.NoError(t, err)
	assert.Equal(t, "refs/heads/main", head.Symref)
	assert.Equal(t, hashFromHex(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", objfmt.SHA1), head.OID)
	assert.False(t, head.Unborn)
}

func TestLooseRefs_Head_SymrefToMissingRefIsUnborn(t *testing.T) {
	// HEAD points at refs/heads/main, but no such ref exists either
	// loose or packed: the canonical "unborn branch" state.
	r := openLooseFromFixture(t, "unborn-head", objfmt.SHA1)

	head, err := r.Head()
	require.NoError(t, err)
	assert.Equal(t, "refs/heads/main", head.Symref)
	assert.Equal(t, objfmt.Hash{}, head.OID)
	assert.True(t, head.Unborn)
}

func TestLooseRefs_Head_DetachedSHA1(t *testing.T) {
	// HEAD content is a raw 40-char SHA-1 hex. Symref empty, OID set,
	// Unborn false.
	r := openLooseFromFixture(t, "detached-head", objfmt.SHA1)

	head, err := r.Head()
	require.NoError(t, err)
	assert.Equal(t, "", head.Symref)
	assert.Equal(t, hashFromHex(t, "4444444444444444444444444444444444444444", objfmt.SHA1), head.OID)
	assert.False(t, head.Unborn)
}

func TestLooseRefs_Head_DetachedSHA256(t *testing.T) {
	// SHA-256 detached HEAD: a 64-char hex must parse cleanly when the
	// algo bound to the backend matches. Built in a tempdir so the
	// fixture set stays focused on the SHA-1 cases.
	dir := t.TempDir()
	hex64 := "5555555555555555555555555555555555555555555555555555555555555555"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "HEAD"), []byte(hex64+"\n"), 0o644))

	r, err := openLooseRefs(dir, dir, objfmt.SHA256)
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	head, err := r.Head()
	require.NoError(t, err)
	assert.Equal(t, "", head.Symref)
	assert.Equal(t, hashFromHex(t, hex64, objfmt.SHA256), head.OID)
	assert.False(t, head.Unborn)
}

func TestLooseRefs_Traits_PeeledAndFullyPeeled(t *testing.T) {
	// The fixture header is `# pack-refs with: peeled fully-peeled`;
	// both flags must be true and `sorted` must remain false.
	r := openLooseFromFixture(t, "packed-refs-fully-peeled", objfmt.SHA1)

	traits := r.refTraits()
	assert.True(t, traits.peeled)
	assert.True(t, traits.fullyPeeled)
	assert.False(t, traits.sorted)
}

func TestLooseRefs_Traits_SortedOnly(t *testing.T) {
	// Header `# pack-refs with: sorted`: only `sorted` flips.
	r := openLooseFromFixture(t, "packed-refs-sorted", objfmt.SHA1)

	traits := r.refTraits()
	assert.False(t, traits.peeled)
	assert.False(t, traits.fullyPeeled)
	assert.True(t, traits.sorted)
}

func TestLooseRefs_Traits_NoHeader(t *testing.T) {
	// `packed-refs` body without a `# pack-refs with:` header: every
	// trait flag remains false.
	r := openLooseFromFixture(t, "packed-refs-no-traits", objfmt.SHA1)

	traits := r.refTraits()
	assert.False(t, traits.peeled)
	assert.False(t, traits.fullyPeeled)
	assert.False(t, traits.sorted)
}

func TestLooseRefs_Traits_UnknownTokensTolerated(t *testing.T) {
	// A header carrying a future trait token must not blow up the file;
	// known tokens still surface, unknown tokens are silently ignored.
	traits := parsePackedRefTraits("# pack-refs with: peeled fully-peeled some-future-trait")
	assert.True(t, traits.peeled)
	assert.True(t, traits.fullyPeeled)
	assert.False(t, traits.sorted)
}

func TestLooseRefs_PeelKnown_Tag(t *testing.T) {
	// The annotated tag in `packed-only` carries a `^peel` line, so
	// peelKnown is true and peeled equals the parsed hex.
	r := openLooseFromFixture(t, "packed-only", objfmt.SHA1)

	peeled, peelKnown, ok := r.peelHint("refs/tags/v1")
	require.True(t, ok)
	assert.True(t, peelKnown)
	assert.Equal(t, hashFromHex(t, "dddddddddddddddddddddddddddddddddddddddd", objfmt.SHA1), peeled)
}

func TestLooseRefs_PeelKnown_NonTag(t *testing.T) {
	// The branch entry in `packed-only` has no `^peel` line; peelKnown
	// must be false and peeled must be the zero hash.
	r := openLooseFromFixture(t, "packed-only", objfmt.SHA1)

	peeled, peelKnown, ok := r.peelHint("refs/heads/main")
	require.True(t, ok)
	assert.False(t, peelKnown)
	assert.Equal(t, objfmt.Hash{}, peeled)
}

func TestLooseRefs_MalformedPackedRefsLine(t *testing.T) {
	// A `packed-refs` body whose first ref line is missing the
	// separator must surface ErrCorruptObject with line + text in the
	// message so diagnostics can pinpoint the offender.
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644))
	body := "# pack-refs with: peeled\nnotahexnotaref\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "packed-refs"), []byte(body), 0o644))

	_, err := openLooseRefs(dir, dir, objfmt.SHA1)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrCorruptObject),
		"expected ErrCorruptObject, got %v", err)
	assert.Contains(t, err.Error(), "line 2")
	assert.Contains(t, err.Error(), "notahexnotaref")
}

func TestLooseRefs_MalformedHEADContent(t *testing.T) {
	// HEAD content that is neither `ref: ...` nor a valid hex OID is
	// corruption; the error must wrap ErrCorruptObject.
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "HEAD"), []byte("garbage-not-a-ref-or-oid\n"), 0o644))

	_, err := openLooseRefs(dir, dir, objfmt.SHA1)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrCorruptObject),
		"expected ErrCorruptObject, got %v", err)
}

// TestLooseRefs_SortedSliceMatchesIterRefs locks in the invariant that
// the backing `sorted` slice is consistent with the IterRefs output —
// a regression in either path is surfaced as a diff.
func TestLooseRefs_SortedSliceMatchesIterRefs(t *testing.T) {
	r := openLooseFromFixture(t, "mixed", objfmt.SHA1)
	got := collectRefs(t, r)

	names := make([]string, 0, len(got))
	for _, e := range got {
		names = append(names, e.Name)
	}
	assert.True(t, slices.IsSorted(names), "IterRefs output must be sorted")
	assert.Equal(t, r.sorted, names, "sorted slice must mirror IterRefs")
}

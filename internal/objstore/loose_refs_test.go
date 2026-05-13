package objstore

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
)

// hashFromHex parses s as a SHA-1 hex string or fails the test. The
// algo argument is kept for source-compatibility with callsites that
// explicitly pass `objfmt.SHA1` for documentation; the helper rejects
// any other algo so SHA-256 fixtures route through `hashFromHex256`.
func hashFromHex(t *testing.T, s string, algo objfmt.Algo) objfmt.SHA1Hash {
	t.Helper()
	require.Equal(t, objfmt.SHA1, algo,
		"hashFromHex only supports SHA-1; use hashFromHex256 for SHA-256")
	h, err := objfmt.ParseSHA1Hex(s)
	require.NoError(t, err)
	return h
}

// hashFromHex256 parses s as a SHA-256 hex string or fails the test.
func hashFromHex256(t *testing.T, s string) objfmt.SHA256Hash {
	t.Helper()
	h, err := objfmt.ParseSHA256Hex(s)
	require.NoError(t, err)
	return h
}

// collectRefs drains the iterator into a deterministic slice. The
// backend yields refs in lexical order; tests compare the slice
// directly rather than re-sorting, so a regression in ordering is
// surfaced as a diff rather than masked.
func collectRefs(t *testing.T, r *looseRefs[objfmt.SHA1Hash]) []RefEntry[objfmt.SHA1Hash] {
	t.Helper()
	var out []RefEntry[objfmt.SHA1Hash]
	for entry, err := range r.IterRefs() {
		require.NoError(t, err)
		out = append(out, entry)
	}
	return out
}

// openLooseFromFixture materializes the named fixture and opens the
// loose-refs backend on it. The helper centralises the gitdir + algo
// plumbing every loose-refs test needs.
func openLooseFromFixture(t *testing.T, name string, algo objfmt.Algo) *looseRefs[objfmt.SHA1Hash] {
	t.Helper()
	root := materializeFixture(t, name)
	gitDir := filepath.Join(root, ".git")
	r, err := openLooseRefs[objfmt.SHA1Hash](gitDir, gitDir, algo)
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func TestLooseRefs_LooseOnly_IterRefsSorted(t *testing.T) {
	t.Parallel()
	// All three loose refs surface in lexical order with the synthetic
	// OIDs the fixture committed. Subdirectory entries
	// (`refs/heads/feature/x`) walk correctly.
	r := openLooseFromFixture(t, "loose-only", objfmt.SHA1)
	got := collectRefs(t, r)

	want := []RefEntry[objfmt.SHA1Hash]{
		{Name: "refs/heads/feature/x", OID: hashFromHex(t, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", objfmt.SHA1)},
		{Name: "refs/heads/main", OID: hashFromHex(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", objfmt.SHA1)},
		{Name: "refs/tags/v1", OID: hashFromHex(t, "cccccccccccccccccccccccccccccccccccccccc", objfmt.SHA1)},
	}
	assert.Equal(t, want, got)
}

func TestLooseRefs_PackedOnly_IterRefs(t *testing.T) {
	t.Parallel()
	// Only `packed-refs` carries entries. The parser must surface them
	// all, the trait header sets `peeled` and `fully-peeled` (so every
	// entry's PeelKnown is true), and the tag's `^peel` line populates
	// packedEntry[objfmt.SHA1Hash].peeled.
	r := openLooseFromFixture(t, "packed-only", objfmt.SHA1)
	got := collectRefs(t, r)

	want := []RefEntry[objfmt.SHA1Hash]{
		{
			Name:      "refs/heads/main",
			OID:       hashFromHex(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", objfmt.SHA1),
			PeelKnown: true,
		},
		{
			Name:      "refs/tags/v1",
			OID:       hashFromHex(t, "cccccccccccccccccccccccccccccccccccccccc", objfmt.SHA1),
			Peeled:    hashFromHex(t, "dddddddddddddddddddddddddddddddddddddddd", objfmt.SHA1),
			PeelKnown: true,
		},
	}
	assert.Equal(t, want, got)
}

func TestLooseRefs_Mixed_LooseShadowsPacked(t *testing.T) {
	t.Parallel()
	// `refs/heads/main` exists in both: loose carries OID-C, packed
	// carries OID-A. The loose entry must win, and the packed-only
	// `refs/heads/old` must still surface.
	r := openLooseFromFixture(t, "mixed", objfmt.SHA1)
	got := collectRefs(t, r)

	wantLoose := hashFromHex(t, "3333333333333333333333333333333333333333", objfmt.SHA1)
	wantOld := hashFromHex(t, "2222222222222222222222222222222222222222", objfmt.SHA1)
	// `mixed`'s packed-refs header advertises `fully-peeled`, so the
	// packed-only `refs/heads/old` entry inherits PeelKnown=true (its
	// missing `^<oid>` line is authoritative under the trait). The
	// loose-shadowed `refs/heads/main` keeps PeelKnown=false because
	// the trait does not apply to a loose-override OID.
	want := []RefEntry[objfmt.SHA1Hash]{
		{Name: "refs/heads/main", OID: wantLoose},
		{Name: "refs/heads/old", OID: wantOld, PeelKnown: true},
	}
	assert.Equal(t, want, got)

	// Loose overrides drop packed peel information AND the fixture's
	// packed-refs header carries no `fully-peeled` trait, so the
	// loose-shadowed main must surface PeelKnown=false.
	entry, found, err := r.Lookup("refs/heads/main")
	require.NoError(t, err)
	require.True(t, found)
	assert.False(t, entry.PeelKnown,
		"loose override must drop packed peel hint")
	assert.Equal(t, objfmt.SHA1Hash{}, entry.Peeled)
}

func TestLooseRefs_Head_SymrefToExistingRef(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	// HEAD points at refs/heads/main, but no such ref exists either
	// loose or packed: the canonical "unborn branch" state.
	r := openLooseFromFixture(t, "unborn-head", objfmt.SHA1)

	head, err := r.Head()
	require.NoError(t, err)
	assert.Equal(t, "refs/heads/main", head.Symref)
	assert.Equal(t, objfmt.SHA1Hash{}, head.OID)
	assert.True(t, head.Unborn)
}

func TestLooseRefs_Head_DetachedSHA1(t *testing.T) {
	t.Parallel()
	// HEAD content is a raw 40-char SHA-1 hex. Symref empty, OID set,
	// Unborn false.
	r := openLooseFromFixture(t, "detached-head", objfmt.SHA1)

	head, err := r.Head()
	require.NoError(t, err)
	assert.Empty(t, head.Symref)
	assert.Equal(t, hashFromHex(t, "4444444444444444444444444444444444444444", objfmt.SHA1), head.OID)
	assert.False(t, head.Unborn)
}

func TestLooseRefs_Head_DetachedSHA256(t *testing.T) {
	t.Parallel()
	// SHA-256 detached HEAD: a 64-char hex must parse cleanly when the
	// algo bound to the backend matches. Built in a tempdir so the
	// fixture set stays focused on the SHA-1 cases.
	dir := t.TempDir()
	hex64 := "5555555555555555555555555555555555555555555555555555555555555555"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "HEAD"), []byte(hex64+"\n"), 0o644))

	r, err := openLooseRefs[objfmt.SHA256Hash](dir, dir, objfmt.SHA256)
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	head, err := r.Head()
	require.NoError(t, err)
	assert.Empty(t, head.Symref)
	assert.Equal(t, hashFromHex256(t, hex64), head.OID)
	assert.False(t, head.Unborn)
}

// writeRefFile materialises a loose ref file at `<dir>/<name>` and
// creates the intermediate directories. Used by chain-resolution tests
// that build their own gitdir layouts on the fly rather than pulling a
// committed fixture.
func writeRefFile(t *testing.T, dir, name, content string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(name))
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
	require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
}

func TestLooseRefs_Head_TwoLevelSymrefChainResolves(t *testing.T) {
	t.Parallel()
	// HEAD -> refs/heads/x -> refs/heads/y, where `y` carries an OID.
	// The resolver follows both hops; `Head.Symref` is the terminal
	// name (the symref that pointed at the OID), matching canonical
	// [refs.c::resolve_ref_unsafe]'s `refname` return.
	//
	// [refs.c::resolve_ref_unsafe]: https://github.com/git/git/blob/v2.54.0/refs.c#L2075
	dir := t.TempDir()
	oid := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	writeRefFile(t, dir, "HEAD", "ref: refs/heads/x\n")
	writeRefFile(t, dir, "refs/heads/x", "ref: refs/heads/y\n")
	writeRefFile(t, dir, "refs/heads/y", oid+"\n")

	r, err := openLooseRefs[objfmt.SHA1Hash](dir, dir, objfmt.SHA1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	head, err := r.Head()
	require.NoError(t, err)
	assert.Equal(t, "refs/heads/y", head.Symref,
		"Symref must be the terminal symref, matching canonical resolve_ref_unsafe")
	assert.Equal(t, hashFromHex(t, oid, objfmt.SHA1), head.OID)
	assert.False(t, head.Unborn)
}

func TestLooseRefs_Head_ChainExceedingMaxDepthFails(t *testing.T) {
	t.Parallel()
	// Six hops: HEAD -> a -> b -> c -> d -> e -> f. Canonical's cap is
	// `SYMREF_MAXDEPTH = 5` ([refs/refs-internal.h:246]); v0 mirrors it
	// and the seventh lookup must surface ErrCorruptObject.
	//
	// [refs/refs-internal.h:246]: https://github.com/git/git/blob/v2.54.0/refs/refs-internal.h#L246
	dir := t.TempDir()
	writeRefFile(t, dir, "HEAD", "ref: refs/heads/a\n")
	writeRefFile(t, dir, "refs/heads/a", "ref: refs/heads/b\n")
	writeRefFile(t, dir, "refs/heads/b", "ref: refs/heads/c\n")
	writeRefFile(t, dir, "refs/heads/c", "ref: refs/heads/d\n")
	writeRefFile(t, dir, "refs/heads/d", "ref: refs/heads/e\n")
	writeRefFile(t, dir, "refs/heads/e", "ref: refs/heads/f\n")
	writeRefFile(t, dir, "refs/heads/f",
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n")

	_, err := openLooseRefs[objfmt.SHA1Hash](dir, dir, objfmt.SHA1)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrCorruptObject,
		"deep chain must wrap ErrCorruptObject, got %v", err)
}

func TestLooseRefs_Head_SymrefCycleDetected(t *testing.T) {
	t.Parallel()
	// A two-step cycle: HEAD -> refs/heads/a -> refs/heads/b ->
	// refs/heads/a. The walker must spot the revisit and surface
	// ErrCorruptObject rather than spinning until depth-cap.
	dir := t.TempDir()
	writeRefFile(t, dir, "HEAD", "ref: refs/heads/a\n")
	writeRefFile(t, dir, "refs/heads/a", "ref: refs/heads/b\n")
	writeRefFile(t, dir, "refs/heads/b", "ref: refs/heads/a\n")

	_, err := openLooseRefs[objfmt.SHA1Hash](dir, dir, objfmt.SHA1)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrCorruptObject,
		"cycle must wrap ErrCorruptObject, got %v", err)
}

func TestLooseRefs_Head_ChainTerminatingInUnbornRefIsUnborn(t *testing.T) {
	t.Parallel()
	// HEAD -> refs/heads/x -> refs/heads/y, but `y` does not exist.
	// `Head.Symref` is the LAST symref name in the chain (the
	// unresolvable target), `OID` is zero, and `Unborn` is true.
	dir := t.TempDir()
	writeRefFile(t, dir, "HEAD", "ref: refs/heads/x\n")
	writeRefFile(t, dir, "refs/heads/x", "ref: refs/heads/y\n")

	r, err := openLooseRefs[objfmt.SHA1Hash](dir, dir, objfmt.SHA1)
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	head, err := r.Head()
	require.NoError(t, err)
	assert.Equal(t, "refs/heads/y", head.Symref)
	assert.Equal(t, objfmt.SHA1Hash{}, head.OID)
	assert.True(t, head.Unborn)
}

func TestLooseRefs_Traits_PeeledAndFullyPeeled(t *testing.T) {
	t.Parallel()
	// The fixture header is `# pack-refs with: peeled fully-peeled`;
	// both flags must be true and `sorted` must remain false.
	r := openLooseFromFixture(t, "packed-refs-fully-peeled", objfmt.SHA1)

	assert.True(t, r.traits.peeled)
	assert.True(t, r.traits.fullyPeeled)
	assert.False(t, r.traits.sorted)
}

func TestLooseRefs_Traits_SortedOnly(t *testing.T) {
	t.Parallel()
	// Header `# pack-refs with: sorted`: only `sorted` flips.
	r := openLooseFromFixture(t, "packed-refs-sorted", objfmt.SHA1)

	assert.False(t, r.traits.peeled)
	assert.False(t, r.traits.fullyPeeled)
	assert.True(t, r.traits.sorted)
}

func TestLooseRefs_Traits_NoHeader(t *testing.T) {
	t.Parallel()
	// `packed-refs` body without a `# pack-refs with:` header: every
	// trait flag remains false.
	r := openLooseFromFixture(t, "packed-refs-no-traits", objfmt.SHA1)

	assert.False(t, r.traits.peeled)
	assert.False(t, r.traits.fullyPeeled)
	assert.False(t, r.traits.sorted)
}

func TestLooseRefs_Traits_UnknownTokensTolerated(t *testing.T) {
	t.Parallel()
	// A header carrying a future trait token must not blow up the file;
	// known tokens still surface, unknown tokens are silently ignored.
	traits := parsePackedRefTraits("# pack-refs with: peeled fully-peeled some-future-trait")
	assert.True(t, traits.peeled)
	assert.True(t, traits.fullyPeeled)
	assert.False(t, traits.sorted)
}

func TestLooseRefs_PeelKnown_Tag(t *testing.T) {
	t.Parallel()
	// The annotated tag in `packed-only` carries a `^peel` line, so
	// PeelKnown is true and Peeled equals the parsed hex.
	r := openLooseFromFixture(t, "packed-only", objfmt.SHA1)

	entry, found, err := r.Lookup("refs/tags/v1")
	require.NoError(t, err)
	require.True(t, found)
	assert.True(t, entry.PeelKnown)
	assert.Equal(t, hashFromHex(t, "dddddddddddddddddddddddddddddddddddddddd", objfmt.SHA1), entry.Peeled)
}

func TestLooseRefs_PeelKnown_NonTag_FullyPeeledMakesItKnown(t *testing.T) {
	t.Parallel()
	// The branch entry in `packed-only` has no `^peel` line, but the
	// fixture's header advertises `peeled fully-peeled`, so the absence
	// is authoritative: PeelKnown=true with Peeled=zero.
	r := openLooseFromFixture(t, "packed-only", objfmt.SHA1)

	entry, found, err := r.Lookup("refs/heads/main")
	require.NoError(t, err)
	require.True(t, found)
	assert.True(t, entry.PeelKnown,
		"fully-peeled trait must make missing peel definitive")
	assert.Equal(t, objfmt.SHA1Hash{}, entry.Peeled)
}

func TestLooseRefs_MalformedPackedRefsLine(t *testing.T) {
	t.Parallel()
	// A `packed-refs` body whose first ref line is missing the
	// separator must surface ErrCorruptObject with line + text in the
	// message so diagnostics can pinpoint the offender.
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644))
	body := "# pack-refs with: peeled\nnotahexnotaref\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "packed-refs"), []byte(body), 0o644))

	_, err := openLooseRefs[objfmt.SHA1Hash](dir, dir, objfmt.SHA1)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrCorruptObject,
		"expected ErrCorruptObject, got %v", err)
	assert.Contains(t, err.Error(), "line 2")
	assert.Contains(t, err.Error(), "notahexnotaref")
}

func TestLooseRefs_MalformedHEADContent(t *testing.T) {
	t.Parallel()
	// HEAD content that is neither `ref: ...` nor a valid hex OID is
	// corruption; the error must wrap ErrCorruptObject.
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "HEAD"), []byte("garbage-not-a-ref-or-oid\n"), 0o644))

	_, err := openLooseRefs[objfmt.SHA1Hash](dir, dir, objfmt.SHA1)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrCorruptObject,
		"expected ErrCorruptObject, got %v", err)
}

// TestLooseRefs_SortedSliceMatchesIterRefs locks in the invariant that
// the backing `sorted` slice is consistent with the IterRefs output —
// a regression in either path is surfaced as a diff.
func TestLooseRefs_SortedSliceMatchesIterRefs(t *testing.T) {
	t.Parallel()
	r := openLooseFromFixture(t, "mixed", objfmt.SHA1)
	got := collectRefs(t, r)

	names := make([]string, 0, len(got))
	for _, e := range got {
		names = append(names, e.Name)
	}
	assert.True(t, slices.IsSorted(names), "IterRefs output must be sorted")
	assert.Equal(t, r.sorted, names, "sorted slice must mirror IterRefs")
}

func TestLooseRefs_IterRefs_PeelFieldsPopulated_FullyPeeled(t *testing.T) {
	t.Parallel()
	// `packed-refs-fully-peeled` advertises the `fully-peeled` trait, so
	// every yielded entry's PeelKnown must be true regardless of whether
	// the ref itself is peelable. The annotated tag carries its peel hex
	// in Peeled; the branch entry has Peeled at zero.
	r := openLooseFromFixture(t, "packed-refs-fully-peeled", objfmt.SHA1)
	got := collectRefs(t, r)

	wantPeel := hashFromHex(t,
		"dddddddddddddddddddddddddddddddddddddddd", objfmt.SHA1)
	want := []RefEntry[objfmt.SHA1Hash]{
		{
			Name:      "refs/heads/main",
			OID:       hashFromHex(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", objfmt.SHA1),
			Peeled:    objfmt.SHA1Hash{},
			PeelKnown: true,
		},
		{
			Name:      "refs/tags/v1",
			OID:       hashFromHex(t, "cccccccccccccccccccccccccccccccccccccccc", objfmt.SHA1),
			Peeled:    wantPeel,
			PeelKnown: true,
		},
	}
	assert.Equal(t, want, got)
}

func TestLooseRefs_IterRefs_PeelFieldsPopulated_NoTrait(t *testing.T) {
	t.Parallel()
	// `packed-only` advertises `peeled fully-peeled` too, so PeelKnown
	// must be true. Pair this with the no-trait check below to confirm
	// PeelKnown does follow the trait, not a hard-coded value.
	r := openLooseFromFixture(t, "packed-only", objfmt.SHA1)
	got := collectRefs(t, r)
	for _, e := range got {
		assert.True(t, e.PeelKnown,
			"%s: fully-peeled trait must surface as PeelKnown=true", e.Name)
	}

	// `packed-refs-no-traits` ships an entry with no trait header. The
	// branch ref has no `^peel` line, so PeelKnown must be false.
	r2 := openLooseFromFixture(t, "packed-refs-no-traits", objfmt.SHA1)
	for entry, err := range r2.IterRefs() {
		require.NoError(t, err)
		if entry.Name == "refs/heads/main" {
			assert.False(t, entry.PeelKnown,
				"branch ref without trait must have PeelKnown=false")
			assert.Equal(t, objfmt.SHA1Hash{}, entry.Peeled)
		}
	}
}

func TestLooseRefs_Lookup_FullyPeeledTrait_NoPeelMeansDefinitive(t *testing.T) {
	t.Parallel()
	// Branch entry in a `fully-peeled` fixture. The trait makes the
	// absence of `^<oid>` authoritative: PeelKnown=true, Peeled=zero.
	r := openLooseFromFixture(t, "packed-refs-fully-peeled", objfmt.SHA1)

	entry, found, err := r.Lookup("refs/heads/main")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "refs/heads/main", entry.Name)
	assert.Equal(t,
		hashFromHex(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", objfmt.SHA1),
		entry.OID)
	assert.True(t, entry.PeelKnown)
	assert.Equal(t, objfmt.SHA1Hash{}, entry.Peeled)
}

func TestLooseRefs_Lookup_PeelLineRecorded(t *testing.T) {
	t.Parallel()
	// The annotated tag has both the trait and an explicit `^peel`
	// line. PeelKnown=true and Peeled carries the recorded hex.
	r := openLooseFromFixture(t, "packed-refs-fully-peeled", objfmt.SHA1)

	entry, found, err := r.Lookup("refs/tags/v1")
	require.NoError(t, err)
	require.True(t, found)
	assert.True(t, entry.PeelKnown)
	assert.Equal(t,
		hashFromHex(t, "dddddddddddddddddddddddddddddddddddddddd", objfmt.SHA1),
		entry.Peeled)
}

func TestLooseRefs_Lookup_NoTrait_PeelLineStillKnown(t *testing.T) {
	t.Parallel()
	// Without `fully-peeled` but with an explicit `^peel`, the peel is
	// still definitive: the entry's own peelKnown bit suffices.
	r := openLooseFromFixture(t, "packed-only", objfmt.SHA1)

	entry, found, err := r.Lookup("refs/tags/v1")
	require.NoError(t, err)
	require.True(t, found)
	assert.True(t, entry.PeelKnown)
	assert.Equal(t,
		hashFromHex(t, "dddddddddddddddddddddddddddddddddddddddd", objfmt.SHA1),
		entry.Peeled)
}

func TestLooseRefs_Lookup_NoTrait_NoPeelLineUnknown(t *testing.T) {
	t.Parallel()
	// `packed-refs-no-traits` ships a branch entry with no trait, so the
	// absence of `^peel` is not authoritative: PeelKnown must be false.
	r := openLooseFromFixture(t, "packed-refs-no-traits", objfmt.SHA1)

	entry, found, err := r.Lookup("refs/heads/main")
	require.NoError(t, err)
	require.True(t, found)
	assert.False(t, entry.PeelKnown)
	assert.Equal(t, objfmt.SHA1Hash{}, entry.Peeled)
}

func TestLooseRefs_Lookup_MissingRef(t *testing.T) {
	t.Parallel()
	r := openLooseFromFixture(t, "packed-refs-fully-peeled", objfmt.SHA1)

	entry, found, err := r.Lookup("refs/heads/does-not-exist")
	require.NoError(t, err)
	assert.False(t, found)
	assert.Equal(t, RefEntry[objfmt.SHA1Hash]{}, entry)
}

// TestLooseRefs_PeeledTrait_TagsKnowPeel exercises the `peeled`-trait
// path in `toRefEntry`: under `# pack-refs with: peeled` (without
// `fully-peeled`), a tag's missing `^peel` line is authoritative —
// canonical Git's `next_record` ([refs/packed-backend.c:945]) sets
// `REF_KNOWS_PEELED` for any ref whose name has the `refs/tags/`
// prefix. Annotated tags with an explicit peel still surface the
// recorded OID; commit-target lightweight tags surface a zero peel
// with PeelKnown=true.
//
// [refs/packed-backend.c:945]: https://github.com/git/git/blob/v2.54.0/refs/packed-backend.c#L945
func TestLooseRefs_PeeledTrait_TagsKnowPeel(t *testing.T) {
	t.Parallel()
	r := openLooseFromFixture(t, "packed-refs-peeled-only", objfmt.SHA1)

	annotated, found, err := r.Lookup("refs/tags/v1")
	require.NoError(t, err)
	require.True(t, found)
	assert.True(t, annotated.PeelKnown,
		"annotated tag with `^peel` must surface PeelKnown=true")
	assert.Equal(t,
		hashFromHex(t, "dddddddddddddddddddddddddddddddddddddddd", objfmt.SHA1),
		annotated.Peeled)

	lightweight, found, err := r.Lookup("refs/tags/lightweight")
	require.NoError(t, err)
	require.True(t, found)
	assert.True(t, lightweight.PeelKnown,
		"`peeled` trait makes a tag's missing `^peel` line authoritative")
	assert.Equal(t, objfmt.SHA1Hash{}, lightweight.Peeled,
		"commit-target tag yields zero peel under the `peeled` trait")
}

// TestLooseRefs_PeeledTrait_NonTagDoesNotInferPeelKnown pins down the
// `refs/tags/` scope: the `peeled` trait says nothing about non-tag
// refs ([refs/packed-backend.c:945] gates the `REF_KNOWS_PEELED` set
// on `starts_with(rec->refname, "refs/tags/")`). A branch ref without
// `^peel` must surface PeelKnown=false even when the trait is set.
//
// [refs/packed-backend.c:945]: https://github.com/git/git/blob/v2.54.0/refs/packed-backend.c#L945
func TestLooseRefs_PeeledTrait_NonTagDoesNotInferPeelKnown(t *testing.T) {
	t.Parallel()
	r := openLooseFromFixture(t, "packed-refs-peeled-only", objfmt.SHA1)

	entry, found, err := r.Lookup("refs/heads/main")
	require.NoError(t, err)
	require.True(t, found)
	assert.False(t, entry.PeelKnown,
		"`peeled` trait does not apply to non-tag refs")
	assert.Equal(t, objfmt.SHA1Hash{}, entry.Peeled)
}

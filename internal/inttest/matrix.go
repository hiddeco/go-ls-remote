package inttest

import (
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	lsremote "github.com/hiddeco/go-ls-remote"
	"github.com/hiddeco/go-ls-remote/internal/testfixture"
)

// Entry is one row in the integration-test fixture matrix. Each entry
// names a single repository under `testdata/repos/` together with the
// answers a correct transport implementation must return for that
// repository: the symbolic-HEAD target, the advertised ref set, and a
// small map of sample `object-info` sizes.
//
// An Entry is data, not behaviour. Tests iterate [Entries] and call
// [Entry.Materialize] to obtain a fresh on-disk gitdir; the entry's
// declared expectations are the cross-transport baseline.
//
// # Why no static path
//
// An Entry intentionally exposes only [Entry.Name] as a stable
// identifier. The on-disk path is per-`t.TempDir()` and lives on the
// returned value from [Entry.Materialize]. A pre-computed `RepoPath`
// field would mislead callers into thinking it survives the test
// lifetime, and shadow the per-test isolation `testfixture` provides.
type Entry struct {
	// Name is the fixture directory under `testdata/repos/`, e.g.
	// `loose-only` or `loose-objects-sha256`.
	Name string

	// ObjectFormat is the cryptographic hash function the fixture
	// uses to name objects. Harnesses dispatch on this field to
	// open the right `objstore.Store[H]` instantiation.
	ObjectFormat lsremote.ObjectFormat

	// ExpectedDefaultBranch is the symbolic-HEAD target a correct
	// `ls-refs` response advertises as `HEAD`'s symref, e.g.
	// `refs/heads/main`. Empty when [Entry.Detached] is true.
	ExpectedDefaultBranch string

	// Unborn is true when HEAD is a symbolic ref pointing at a
	// branch with no commits yet. A v2 server omits `HEAD` from
	// the `ls-refs` response in that case; v0 emits the
	// `capabilities^{}` placeholder.
	Unborn bool

	// Detached is true when HEAD is a raw object id rather than a
	// `ref: refs/...` line. Canonical Git advertises a detached
	// HEAD inline on the wire without a symref pointer.
	Detached bool

	// ExpectedRefs is the set of refs a correct transport reports
	// for this fixture, excluding `HEAD`. Order is irrelevant —
	// consumers compare by name — but consistent ordering aids
	// review. The slice MUST hold every ref the on-disk store
	// exposes; partial declarations are treated as a drift error.
	ExpectedRefs []ExpectedRef

	// ExpectedObjectInfo maps object ids (lower-case hex) to the
	// payload size in bytes that a correct `object-info` response
	// returns. The map is populated only for fixtures whose
	// objects are real on-disk records; fixtures that ship
	// synthetic placeholder OIDs leave it empty because there is
	// no underlying object to size.
	ExpectedObjectInfo map[string]int64
}

// Materialize copies the fixture into a fresh `t.TempDir()` via
// `testfixture.MaterializeRepo` and ensures the conventional
// `objects/pack/` directory exists. Several fixtures ship only the
// minimum subtree their backend exercises; `objstore.Open` expects the
// pack directory to be present even when no pack ships, so this helper
// re-creates it the way every transport harness would.
//
// The returned path is the gitdir (the renamed `.git/` under the
// `t.TempDir()` root); pass it directly to `objstore.Open`.
func (e Entry) Materialize(t testing.TB) string {
	t.Helper()
	gitdir := testfixture.MaterializeRepo(t, e.Name)
	require.NoError(t, ensurePackDir(gitdir))
	return gitdir
}

// ExpectedRef is one ref a correct transport must advertise for a given
// fixture. The fields mirror the public [lsremote.Ref] shape so a
// cross-transport assertion is a per-field comparison.
type ExpectedRef struct {
	// Name is the fully-qualified ref name, e.g. `refs/heads/main`,
	// `refs/tags/v1`.
	Name string

	// Hash is the hex object id the ref points at. For fixtures
	// that use synthetic placeholder OIDs (e.g. `aaaa...`), this is
	// the placeholder verbatim; the value still pins the wire
	// shape even when no real object exists.
	Hash string

	// Peeled is the hex object id of the underlying commit when the
	// ref is an annotated tag and the fixture's backend records the
	// peel cheaply (a `^<oid>` line in `packed-refs`, or the
	// `fully-peeled` packed-refs trait). Empty otherwise.
	Peeled string
}

// Entries returns the curated fixture matrix used by the
// cross-transport integration suite. The slice is freshly allocated on
// every call so callers may sort or filter it without disturbing
// subsequent consumers.
//
// The selection covers the ref-backend shapes that every transport
// must agree on (loose-only, packed-only, the loose-overrides-packed
// `mixed` blend, detached and unborn HEADs, the all-empty repository)
// plus an object-store shape per hash algorithm (the SHA-1
// `loose-objects` fixture and the SHA-256 `loose-objects-sha256`
// fixture). Deliberately corrupt fixtures (`idx-corrupt`,
// `midx-missing-pack`, `with-alternates-cycle`, …) and shape-specific
// fixtures (alternates, reftable, worktree-as-file) are out of scope
// for this matrix; they have dedicated unit tests at the layer that
// owns the affected behaviour.
func Entries() []Entry {
	return slices.Clone(curated)
}

// FixturesRoot resolves to `<module>/testdata/repos`. It is exported so
// tests outside this package can confirm declared fixture names against
// the on-disk tree without round-tripping through `runtime.Caller` in
// each call site.
func FixturesRoot(t testing.TB) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller(0) failed; package layout changed?")
	// `file` is `<module>/internal/inttest/matrix.go`; walk up to the
	// module root and join `testdata/repos`.
	return filepath.Join(filepath.Dir(file), "..", "..", "testdata", "repos")
}

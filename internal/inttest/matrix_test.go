package inttest_test

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	lsremote "github.com/hiddeco/go-ls-remote"
	"github.com/hiddeco/go-ls-remote/internal/inttest"
	"github.com/hiddeco/go-ls-remote/internal/objfmt"
	"github.com/hiddeco/go-ls-remote/internal/objstore"
)

// TestEntries_namesResolveOnDisk asserts that every fixture named by the
// matrix exists under `testdata/repos/`. The matrix is the contract a
// later integration harness reads against on-disk state; a stale name
// here would silently break every consumer.
func TestEntries_namesResolveOnDisk(t *testing.T) {
	entries := inttest.Entries()
	require.NotEmpty(t, entries, "matrix must declare at least one fixture")

	root := inttest.FixturesRoot(t)
	seen := map[string]bool{}
	for _, e := range entries {
		require.NotEmpty(t, e.Name, "entry has empty Name")
		require.False(t, seen[e.Name], "duplicate entry %q", e.Name)
		seen[e.Name] = true

		dir := filepath.Join(root, e.Name)
		info, err := os.Stat(dir)
		require.NoError(t, err, "fixture %q missing on disk", e.Name)
		assert.True(t, info.IsDir(), "fixture %q is not a directory", e.Name)
	}
}

// TestEntries_expectedRefsMatchStore asserts that every entry's declared
// ref set agrees with what `objstore.Open` reports on the materialised
// fixture. Mismatches mean the declarations have drifted from the
// fixture bytes and a downstream cross-transport test would compare
// against a stale baseline.
func TestEntries_expectedRefsMatchStore(t *testing.T) {
	for _, e := range inttest.Entries() {
		t.Run(e.Name, func(t *testing.T) {
			got := openAndCollectRefs(t, e)

			want := make(map[string]inttest.ExpectedRef, len(e.ExpectedRefs))
			for _, r := range e.ExpectedRefs {
				want[r.Name] = r
			}

			assert.Equal(t, len(want), len(got),
				"ref-set size mismatch for %q: want %d got %d (%v)",
				e.Name, len(want), len(got), refNames(got))
			for name, w := range want {
				g, ok := got[name]
				if !assert.True(t, ok, "ref %q missing from store for %q", name, e.Name) {
					continue
				}
				assert.Equal(t, w.Hash, g.Hash, "hash mismatch on %q", name)
				assert.Equal(t, w.Peeled, g.Peeled, "peel mismatch on %q", name)
			}
		})
	}
}

// TestEntries_expectedDefaultBranchMatchesHEAD asserts that each entry's
// `ExpectedDefaultBranch` agrees with what `Store.Head` reports.
//
// Convention used by the matrix:
//   - Detached entries declare an empty `ExpectedDefaultBranch`.
//   - Symbolic-HEAD entries (including unborn) declare the symref
//     target verbatim (e.g. `refs/heads/main`).
func TestEntries_expectedDefaultBranchMatchesHEAD(t *testing.T) {
	for _, e := range inttest.Entries() {
		t.Run(e.Name, func(t *testing.T) {
			switch e.ObjectFormat {
			case lsremote.ObjectFormatSHA1:
				store := openSHA1Store(t, e)
				head, err := store.Head()
				require.NoError(t, err)
				if e.Detached {
					assert.Empty(t, head.Symref)
					assert.Empty(t, e.ExpectedDefaultBranch)
				} else {
					assert.Equal(t, e.ExpectedDefaultBranch, head.Symref)
					assert.Equal(t, e.Unborn, head.Unborn)
				}
			case lsremote.ObjectFormatSHA256:
				store := openSHA256Store(t, e)
				head, err := store.Head()
				require.NoError(t, err)
				if e.Detached {
					assert.Empty(t, head.Symref)
				} else {
					assert.Equal(t, e.ExpectedDefaultBranch, head.Symref)
					assert.Equal(t, e.Unborn, head.Unborn)
				}
			default:
				t.Fatalf("unsupported object format %q", e.ObjectFormat)
			}
		})
	}
}

// TestEntries_expectedObjectInfoMatchesStore asserts that every OID
// declared in `ExpectedObjectInfo` resolves through the local store
// and reports the declared size. The size column is what the
// `object-info` v2 command echoes on the wire; declarations that drift
// from disk would let a transport regression go unnoticed.
func TestEntries_expectedObjectInfoMatchesStore(t *testing.T) {
	for _, e := range inttest.Entries() {
		if len(e.ExpectedObjectInfo) == 0 {
			continue
		}
		t.Run(e.Name, func(t *testing.T) {
			switch e.ObjectFormat {
			case lsremote.ObjectFormatSHA1:
				store := openSHA1Store(t, e)
				for hexOID, wantSize := range e.ExpectedObjectInfo {
					var oid objfmt.SHA1Hash
					n, err := hex.Decode(oid[:], []byte(hexOID))
					require.NoError(t, err)
					require.Equal(t, len(oid), n)
					info, err := store.ObjectInfo(oid)
					require.NoError(t, err, "ObjectInfo(%s)", hexOID)
					assert.Equal(t, wantSize, info.Size, "size for %s", hexOID)
				}
			case lsremote.ObjectFormatSHA256:
				store := openSHA256Store(t, e)
				for hexOID, wantSize := range e.ExpectedObjectInfo {
					var oid objfmt.SHA256Hash
					n, err := hex.Decode(oid[:], []byte(hexOID))
					require.NoError(t, err)
					require.Equal(t, len(oid), n)
					info, err := store.ObjectInfo(oid)
					require.NoError(t, err, "ObjectInfo(%s)", hexOID)
					assert.Equal(t, wantSize, info.Size, "size for %s", hexOID)
				}
			}
		})
	}
}

// openAndCollectRefs materialises e and returns its refs as a map keyed
// by ref name. The type-switch on object format is the same one the
// cross-transport equivalence harness will eventually centralise.
func openAndCollectRefs(t *testing.T, e inttest.Entry) map[string]collectedRef {
	t.Helper()
	out := map[string]collectedRef{}
	switch e.ObjectFormat {
	case lsremote.ObjectFormatSHA1:
		store := openSHA1Store(t, e)
		for re, err := range store.IterRefs() {
			require.NoError(t, err)
			r := collectedRef{Name: re.Name, Hash: hex.EncodeToString(re.OID[:])}
			if re.PeelKnown {
				peeled := hex.EncodeToString(re.Peeled[:])
				if peeled != zeroSHA1 {
					r.Peeled = peeled
				}
			}
			out[re.Name] = r
		}
	case lsremote.ObjectFormatSHA256:
		store := openSHA256Store(t, e)
		for re, err := range store.IterRefs() {
			require.NoError(t, err)
			r := collectedRef{Name: re.Name, Hash: hex.EncodeToString(re.OID[:])}
			if re.PeelKnown {
				peeled := hex.EncodeToString(re.Peeled[:])
				if peeled != zeroSHA256 {
					r.Peeled = peeled
				}
			}
			out[re.Name] = r
		}
	}
	return out
}

func openSHA1Store(t *testing.T, e inttest.Entry) *objstore.Store[objfmt.SHA1Hash] {
	t.Helper()
	gitdir := e.Materialize(t)
	store, err := objstore.Open[objfmt.SHA1Hash](gitdir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func openSHA256Store(t *testing.T, e inttest.Entry) *objstore.Store[objfmt.SHA256Hash] {
	t.Helper()
	gitdir := e.Materialize(t)
	store, err := objstore.Open[objfmt.SHA256Hash](gitdir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

type collectedRef struct {
	Name   string
	Hash   string
	Peeled string
}

func refNames(m map[string]collectedRef) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

const (
	zeroSHA1   = "0000000000000000000000000000000000000000"
	zeroSHA256 = "0000000000000000000000000000000000000000000000000000000000000000"
)

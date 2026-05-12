package inttest

import (
	"testing"

	"github.com/stretchr/testify/assert"

	lsremote "github.com/hiddeco/go-ls-remote"
)

// CompareRefs asserts that got matches want as a set, keyed by ref
// name. The comparison is order-tolerant: canonical Git's `ls-refs`
// response is C-locale byte ordered (`for_each_namespaced_ref_1`) and
// the in-process emulator preserves that order, but a separate
// equivalence check on order belongs to a server-specific test, not to
// the cross-transport surface.
//
// HEAD is excluded from the comparison. [Entry.ExpectedRefs] declares
// only the non-HEAD refs a fixture exposes; HEAD's shape varies with
// the entry's [Entry.Unborn] and [Entry.Detached] flags and is checked
// separately by [CompareHEAD].
//
// The fixtureName argument labels every failure with the entry it
// originated from, so a t.Run loop over [Entries] surfaces the
// fixture's name in the failure message without the caller threading
// it through manually.
func CompareRefs(t *testing.T, got []lsremote.Ref, want []ExpectedRef, fixtureName string) {
	t.Helper()

	gotByName := make(map[string]lsremote.Ref, len(got))
	for _, r := range got {
		if r.Name == "HEAD" {
			continue
		}
		gotByName[r.Name] = r
	}
	wantByName := make(map[string]ExpectedRef, len(want))
	for _, r := range want {
		wantByName[r.Name] = r
	}

	assert.Equal(t, len(wantByName), len(gotByName),
		"%s: ref-set size mismatch; want %d got %d",
		fixtureName, len(wantByName), len(gotByName))

	for name, w := range wantByName {
		g, ok := gotByName[name]
		if !assert.True(t, ok, "%s: ref %q missing", fixtureName, name) {
			continue
		}
		assert.Equal(t, w.Hash, g.Hash,
			"%s: hash mismatch on %q", fixtureName, name)
		assert.Equal(t, w.Peeled, g.Peeled,
			"%s: peel mismatch on %q", fixtureName, name)
	}

	for name := range gotByName {
		_, ok := wantByName[name]
		assert.True(t, ok,
			"%s: unexpected ref %q in transport response", fixtureName, name)
	}
}

// CompareHEAD asserts that the HEAD entry in got matches the shape
// declared by e. Three shapes arise:
//
//   - Resolved symbolic HEAD (`!e.Unborn && !e.Detached`): exactly one
//     `HEAD` entry must appear, with a non-empty [lsremote.Ref.Hash] and
//     [lsremote.Ref.Symref] equal to [Entry.ExpectedDefaultBranch].
//   - Unborn HEAD (`e.Unborn`): exactly one `HEAD` entry must appear
//     with an empty [lsremote.Ref.Hash] and [lsremote.Ref.Symref] equal
//     to [Entry.ExpectedDefaultBranch]. The caller must have requested
//     `Unborn: true` and `Symrefs: true` on the [lsremote.RefsRequest];
//     [ls-refs.c:135-136] skips HEAD altogether otherwise.
//   - Detached HEAD (`e.Detached`): exactly one `HEAD` entry must
//     appear with a non-empty [lsremote.Ref.Hash] and no
//     [lsremote.Ref.Symref] (`send_possibly_unborn_head` does not
//     attach `symref-target:` to a detached HEAD).
//
// [ls-refs.c:135-136]: https://github.com/git/git/blob/v2.54.0/ls-refs.c#L135-L136
func CompareHEAD(t *testing.T, got []lsremote.Ref, e Entry) {
	t.Helper()

	var heads []lsremote.Ref
	for _, r := range got {
		if r.Name == "HEAD" {
			heads = append(heads, r)
		}
	}

	if !assert.Len(t, heads, 1, "%s: expected exactly one HEAD entry", e.Name) {
		return
	}
	h := heads[0]

	switch {
	case e.Detached:
		assert.NotEmpty(t, h.Hash, "%s: detached HEAD must carry an OID", e.Name)
		assert.Empty(t, h.Symref, "%s: detached HEAD must not carry a symref", e.Name)
	case e.Unborn:
		assert.Empty(t, h.Hash, "%s: unborn HEAD must carry an empty hash", e.Name)
		assert.Equal(t, e.ExpectedDefaultBranch, h.Symref,
			"%s: unborn HEAD symref mismatch", e.Name)
	default:
		assert.NotEmpty(t, h.Hash, "%s: resolved HEAD must carry an OID", e.Name)
		assert.Equal(t, e.ExpectedDefaultBranch, h.Symref,
			"%s: resolved HEAD symref mismatch", e.Name)
	}
}

// CompareObjectInfo asserts that every OID declared in want appears in
// got with the declared size. Extras in got are ignored: the wire
// command may return more entries than the test declares (e.g. when a
// future iteration extends the matrix), and the test should not couple
// the harness's response shape to the matrix's declared expectations
// any tighter than necessary.
//
// The fixtureName argument labels failures with the originating entry,
// matching [CompareRefs]'s convention.
func CompareObjectInfo(t *testing.T, got []lsremote.ObjectInfo, want map[string]int64, fixtureName string) {
	t.Helper()

	gotByHash := make(map[string]lsremote.ObjectInfo, len(got))
	for _, info := range got {
		gotByHash[info.Hash] = info
	}

	for hash, wantSize := range want {
		g, ok := gotByHash[hash]
		if !assert.True(t, ok, "%s: object-info missing for %s", fixtureName, hash) {
			continue
		}
		assert.Equal(t, wantSize, g.Size,
			"%s: size mismatch for %s", fixtureName, hash)
	}
}

package inttest

import (
	"os"
	"path/filepath"

	lsremote "github.com/hiddeco/go-ls-remote"
)

// curated is the source of truth the [Entries] helper clones. The set
// is ordered for review readability — ref-backend shapes first
// (loose-only, packed-only, mixed), edge HEADs next (detached, unborn,
// empty), object-store shapes last (loose-objects, pack-only, and the
// SHA-256 sibling). See [Entries] for the selection rationale.
//
// Synthetic OIDs (`aaaa...`, `bbbb...`, `cccc...`, `dddd...`) come from
// the on-disk fixtures verbatim. They are valid for ref-pointer
// assertions because the wire protocol echoes the bytes the backend
// holds; they are not valid for `object-info` because no object body
// matches. Fixtures whose objects are real on-disk records carry
// populated `ExpectedObjectInfo` maps; the rest leave it empty.
var curated = []Entry{
	{
		Name:                  "loose-only",
		ObjectFormat:          lsremote.ObjectFormatSHA1,
		ExpectedDefaultBranch: "refs/heads/main",
		ExpectedRefs: []ExpectedRef{
			{Name: "refs/heads/feature/x", Hash: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
			{Name: "refs/heads/main", Hash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
			{Name: "refs/tags/v1", Hash: "cccccccccccccccccccccccccccccccccccccccc"},
		},
	},
	{
		Name:                  "packed-only",
		ObjectFormat:          lsremote.ObjectFormatSHA1,
		ExpectedDefaultBranch: "refs/heads/main",
		ExpectedRefs: []ExpectedRef{
			{Name: "refs/heads/main", Hash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
			{
				Name:   "refs/tags/v1",
				Hash:   "cccccccccccccccccccccccccccccccccccccccc",
				Peeled: "dddddddddddddddddddddddddddddddddddddddd",
			},
		},
	},
	{
		// The loose `refs/heads/main` overrides the `packed-refs`
		// entry of the same name; `refs/heads/old` lives only in
		// `packed-refs` and therefore surfaces with its trait-derived
		// `Peeled` known to the backend.
		Name:                  "mixed",
		ObjectFormat:          lsremote.ObjectFormatSHA1,
		ExpectedDefaultBranch: "refs/heads/main",
		ExpectedRefs: []ExpectedRef{
			{Name: "refs/heads/main", Hash: "3333333333333333333333333333333333333333"},
			{Name: "refs/heads/old", Hash: "2222222222222222222222222222222222222222"},
		},
	},
	{
		// HEAD is a raw OID (no `ref: refs/...` prefix). The fixture
		// ships no refs under `refs/`, so the ref set is empty.
		Name:         "detached-head",
		ObjectFormat: lsremote.ObjectFormatSHA1,
		Detached:     true,
	},
	{
		// HEAD points at `refs/heads/main` but no such ref exists on
		// disk. A v2 server omits HEAD; v0 emits the
		// `capabilities^{}` placeholder. The error-matrix integration
		// suite consumes this entry.
		Name:                  "unborn-head",
		ObjectFormat:          lsremote.ObjectFormatSHA1,
		ExpectedDefaultBranch: "refs/heads/main",
		Unborn:                true,
	},
	{
		// Zero refs, zero objects. The error-matrix integration suite
		// consumes this entry for the empty-repository advertisement.
		Name:                  "empty",
		ObjectFormat:          lsremote.ObjectFormatSHA1,
		ExpectedDefaultBranch: "refs/heads/main",
		Unborn:                true,
	},
	{
		// Real loose objects with an unborn HEAD. The fixture has
		// no refs, so the ref set is empty, but the `object-info`
		// path is exercised end-to-end against real on-disk sizes.
		Name:                  "loose-objects",
		ObjectFormat:          lsremote.ObjectFormatSHA1,
		ExpectedDefaultBranch: "refs/heads/main",
		Unborn:                true,
		ExpectedObjectInfo: map[string]int64{
			"393a7c05257a543bc1369537c7fdb2851dc04b11": 25,  // blob
			"4cb61db1e9094ba0e955298fcbd038ec69bc7a38": 36,  // tree
			"855c1386ff144601eb847df1b4e59057ca415883": 150, // tag
			"9a1288dcf7ead9936f178d8dd8a1f14c81eafbf9": 193, // commit
		},
	},
	{
		// Real pack with no refs. The pack OIDs are not declared
		// here — the matrix's per-pack OID enumeration is not yet a
		// shared utility, and the cross-transport equivalence test
		// can confirm pack-resident object-info via fixtures with
		// loose objects. Listed in the matrix so harnesses also
		// exercise the pack-backend code path.
		Name:                  "pack-only",
		ObjectFormat:          lsremote.ObjectFormatSHA1,
		ExpectedDefaultBranch: "refs/heads/main",
		Unborn:                true,
	},
	{
		// SHA-256 sibling of `loose-objects`. Real on-disk objects
		// let the cross-transport equivalence test compare wire
		// `object-info` against local-store `ObjectInfo` for the
		// `sha256` algorithm path.
		Name:                  "loose-objects-sha256",
		ObjectFormat:          lsremote.ObjectFormatSHA256,
		ExpectedDefaultBranch: "refs/heads/main",
		Unborn:                true,
		ExpectedObjectInfo: map[string]int64{
			"92d2fbd767b5d4ce56ba1dcbc710860b5255f42259c9c7f3fe0c33895545a1d3": 181, // tag
			"e260f0e971c7745ca923fc46c3ea01378efc0a68b0e6f73dc30ecaf9e9ffa546": 48,  // tree
			"c60061d62336c6b760e2c4ec860873a193c61662e4f2a6aa5cb3cbaf9339cd10": 25,  // blob
			"fa1eca2ffe8355c2de5fafcb6da9f5e768e0bf14713a5bfa8b4f5e2ec215dc6c": 224, // commit
		},
	},
}

// ensurePackDir creates `<gitdir>/objects/pack` if it does not yet
// exist. `objstore.Open` requires the directory; several fixtures
// (loose-only, mixed, …) ship without it because they exercise only
// the ref backend.
func ensurePackDir(gitdir string) error {
	return os.MkdirAll(filepath.Join(gitdir, "objects", "pack"), 0o755)
}

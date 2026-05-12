package server

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/hiddeco/go-ls-remote/internal/wire"
	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// reftableContentMainSHA1 mirrors `reftableContentFixtureMain` in the
// objstore tests: the OID committed by the `with-reftable-content`
// fixture's single commit. Updating the fixture means updating both
// constants. The duplication is intentional — the constant in
// `objstore/reftable_backend_test.go` is unexported, and re-importing
// the test package would create a cycle.
const reftableContentMainSHA1 = "dbe62b7be27170912462463476422dff1d92c24e"

// buildLSRefsRequest builds a single v2 ls-refs command-request frame
// followed by the empty-request flush that terminates the session.
// argLines are written verbatim into the command-args section, each
// becoming one pkt-line; the caller supplies the trailing LF if the
// canonical wire form requires one.
func buildLSRefsRequest(argLines []string) []byte {
	var req bytes.Buffer
	req.Write(pktBytes("command=ls-refs\n"))
	req.Write(delimBytes)
	for _, line := range argLines {
		req.Write(pktBytes(line))
	}
	req.Write(flushBytes) // end of command-args
	req.Write(flushBytes) // empty-request: terminate session
	return req.Bytes()
}

// TestLSRefs_Empty_NoArgs pins the response for an empty repo with no
// args: the unborn HEAD is suppressed (canonical Git only emits the
// unborn line when the client has set both `unborn` and `symrefs`,
// [ls-refs.c:135-136]), and there are no other refs.
//
// [ls-refs.c:135-136]: https://github.com/git/git/blob/v2.54.0/ls-refs.c#L135-L136
func TestLSRefs_Empty_NoArgs(t *testing.T) {
	store := openEmptyStore(t)

	resp, err := runV2Session(t, store, buildLSRefsRequest(nil))
	require.NoError(t, err)
	assert.Equal(t, "0000", string(resp))
}

// TestLSRefs_Empty_UnbornOnly pins that `unborn` alone is not
// sufficient: canonical's gate at [ls-refs.c:136] requires
// `data->unborn && data->symrefs && (flag & REF_ISSYMREF)`.
//
// [ls-refs.c:136]: https://github.com/git/git/blob/v2.54.0/ls-refs.c#L136
func TestLSRefs_Empty_UnbornOnly(t *testing.T) {
	store := openEmptyStore(t)

	resp, err := runV2Session(t, store, buildLSRefsRequest([]string{"unborn\n"}))
	require.NoError(t, err)
	assert.Equal(t, "0000", string(resp))
}

// TestLSRefs_Empty_SymrefsOnly pins that `symrefs` alone is not
// sufficient: HEAD's resolved OID is zero, so the non-unborn branch
// of `send_possibly_unborn_head` would emit `unborn HEAD ...` but only
// if `unborn` is set; with just `symrefs`, the unborn fallback gate is
// closed ([ls-refs.c:135-136]).
//
// [ls-refs.c:135-136]: https://github.com/git/git/blob/v2.54.0/ls-refs.c#L135-L136
func TestLSRefs_Empty_SymrefsOnly(t *testing.T) {
	store := openEmptyStore(t)

	resp, err := runV2Session(t, store, buildLSRefsRequest([]string{"symrefs\n"}))
	require.NoError(t, err)
	assert.Equal(t, "0000", string(resp))
}

// TestLSRefs_Empty_UnbornSymrefs pins the unborn-HEAD wire shape
// ([ls-refs.c:91-94]): when `ref->oid == NULL`, canonical Git writes
// `unborn %s` in place of the OID. The literal `unborn` token replaces
// the hex OID, then the refname follows; with `symrefs` set, the
// `symref-target:` attribute lands.
//
// [ls-refs.c:91-94]: https://github.com/git/git/blob/v2.54.0/ls-refs.c#L91-L94
func TestLSRefs_Empty_UnbornSymrefs(t *testing.T) {
	store := openEmptyStore(t)

	req := buildLSRefsRequest([]string{"unborn\n", "symrefs\n"})
	resp, err := runV2Session(t, store, req)
	require.NoError(t, err)

	want := pktLine("unborn HEAD symref-target:refs/heads/main\n") + "0000"
	assert.Equal(t, want, string(resp))
}

// TestLSRefs_PackedRefsFullyPeeled_NoArgs pins the no-args response for
// a repo with HEAD pointing at refs/heads/main, plus an annotated tag
// with a peel line in `packed-refs`. No attrs requested: just OID +
// refname per ref. HEAD's OID equals the tip of refs/heads/main.
func TestLSRefs_PackedRefsFullyPeeled_NoArgs(t *testing.T) {
	store := openStoreFromFixture(t, "packed-refs-fully-peeled")

	resp, err := runV2Session(t, store, buildLSRefsRequest(nil))
	require.NoError(t, err)

	mainOID := strings.Repeat("a", 40)
	tagOID := strings.Repeat("c", 40)

	want := pktLine(mainOID+" HEAD\n") +
		pktLine(mainOID+" refs/heads/main\n") +
		pktLine(tagOID+" refs/tags/v1\n") +
		"0000"
	assert.Equal(t, want, string(resp))
}

// TestLSRefs_PackedRefsFullyPeeled_Peel pins the `peel` arg: the
// annotated tag gets a ` peeled:<oid>` suffix; HEAD and the branch do
// not (HEAD is a commit, no peel). Canonical reference:
// [ls-refs.c:111-115].
//
// [ls-refs.c:111-115]: https://github.com/git/git/blob/v2.54.0/ls-refs.c#L111-L115
func TestLSRefs_PackedRefsFullyPeeled_Peel(t *testing.T) {
	store := openStoreFromFixture(t, "packed-refs-fully-peeled")

	resp, err := runV2Session(t, store, buildLSRefsRequest([]string{"peel\n"}))
	require.NoError(t, err)

	mainOID := strings.Repeat("a", 40)
	tagOID := strings.Repeat("c", 40)
	peeledOID := strings.Repeat("d", 40)

	want := pktLine(mainOID+" HEAD\n") +
		pktLine(mainOID+" refs/heads/main\n") +
		pktLine(tagOID+" refs/tags/v1 peeled:"+peeledOID+"\n") +
		"0000"
	assert.Equal(t, want, string(resp))
}

// TestLSRefs_PackedRefsFullyPeeled_Symrefs pins the `symrefs` arg: HEAD
// gets a ` symref-target:refs/heads/main` suffix. Non-HEAD refs are
// resolved by `IterRefs` so they never carry `symref-target:` in this
// emulator (documented divergence from canonical Git: see the doc
// comment on `writeLSRefsResponse`).
func TestLSRefs_PackedRefsFullyPeeled_Symrefs(t *testing.T) {
	store := openStoreFromFixture(t, "packed-refs-fully-peeled")

	resp, err := runV2Session(t, store, buildLSRefsRequest([]string{"symrefs\n"}))
	require.NoError(t, err)

	mainOID := strings.Repeat("a", 40)
	tagOID := strings.Repeat("c", 40)

	want := pktLine(mainOID+" HEAD symref-target:refs/heads/main\n") +
		pktLine(mainOID+" refs/heads/main\n") +
		pktLine(tagOID+" refs/tags/v1\n") +
		"0000"
	assert.Equal(t, want, string(resp))
}

// TestLSRefs_PackedRefsFullyPeeled_PeelSymrefs combines both attrs.
// Order on the line is `symref-target:` then `peeled:` per
// [ls-refs.c:95-115] — symrefs first, peel second.
//
// [ls-refs.c:95-115]: https://github.com/git/git/blob/v2.54.0/ls-refs.c#L95-L115
func TestLSRefs_PackedRefsFullyPeeled_PeelSymrefs(t *testing.T) {
	store := openStoreFromFixture(t, "packed-refs-fully-peeled")

	req := buildLSRefsRequest([]string{"peel\n", "symrefs\n"})
	resp, err := runV2Session(t, store, req)
	require.NoError(t, err)

	mainOID := strings.Repeat("a", 40)
	tagOID := strings.Repeat("c", 40)
	peeledOID := strings.Repeat("d", 40)

	want := pktLine(mainOID+" HEAD symref-target:refs/heads/main\n") +
		pktLine(mainOID+" refs/heads/main\n") +
		pktLine(tagOID+" refs/tags/v1 peeled:"+peeledOID+"\n") +
		"0000"
	assert.Equal(t, want, string(resp))
}

// TestLSRefs_PackedRefsFullyPeeled_SingleTagPrefix pins prefix
// filtering: with `ref-prefix refs/tags/`, only the tag passes
// `ref_match` ([ls-refs.c:54-67]). HEAD is filtered out because none of
// the prefixes is a prefix of `HEAD`.
//
// [ls-refs.c:54-67]: https://github.com/git/git/blob/v2.54.0/ls-refs.c#L54-L67
func TestLSRefs_PackedRefsFullyPeeled_SingleTagPrefix(t *testing.T) {
	store := openStoreFromFixture(t, "packed-refs-fully-peeled")

	req := buildLSRefsRequest([]string{"ref-prefix refs/tags/\n"})
	resp, err := runV2Session(t, store, req)
	require.NoError(t, err)

	tagOID := strings.Repeat("c", 40)

	want := pktLine(tagOID+" refs/tags/v1\n") + "0000"
	assert.Equal(t, want, string(resp))
}

// TestLSRefs_PackedRefsFullyPeeled_TwoPrefixes pins multi-prefix
// filtering: `ref-prefix refs/heads/` and `ref-prefix refs/tags/`
// together let both the branch and the tag through, but HEAD is still
// filtered out. C-locale sort puts `refs/heads/main` before
// `refs/tags/v1`.
func TestLSRefs_PackedRefsFullyPeeled_TwoPrefixes(t *testing.T) {
	store := openStoreFromFixture(t, "packed-refs-fully-peeled")

	req := buildLSRefsRequest([]string{
		"ref-prefix refs/heads/\n",
		"ref-prefix refs/tags/\n",
	})
	resp, err := runV2Session(t, store, req)
	require.NoError(t, err)

	mainOID := strings.Repeat("a", 40)
	tagOID := strings.Repeat("c", 40)

	want := pktLine(mainOID+" refs/heads/main\n") +
		pktLine(tagOID+" refs/tags/v1\n") +
		"0000"
	assert.Equal(t, want, string(resp))
}

// TestLSRefs_DetachedHead_NoArgs pins the detached-HEAD shape with no
// args: HEAD's OID is non-zero, so `send_possibly_unborn_head` emits
// it as a normal ref. There are no other refs in this fixture.
func TestLSRefs_DetachedHead_NoArgs(t *testing.T) {
	store := openStoreFromFixture(t, "detached-head")

	resp, err := runV2Session(t, store, buildLSRefsRequest(nil))
	require.NoError(t, err)

	headOID := strings.Repeat("4", 40)
	want := pktLine(headOID+" HEAD\n") + "0000"
	assert.Equal(t, want, string(resp))
}

// TestLSRefs_DetachedHead_Symrefs pins that a detached HEAD never
// carries a `symref-target:` attribute even when the client requests
// `symrefs`: canonical's [ls-refs.c:95] gates emission on
// `ref->flags & REF_ISSYMREF`, which is false for a detached HEAD.
//
// [ls-refs.c:95]: https://github.com/git/git/blob/v2.54.0/ls-refs.c#L95
func TestLSRefs_DetachedHead_Symrefs(t *testing.T) {
	store := openStoreFromFixture(t, "detached-head")

	resp, err := runV2Session(t, store, buildLSRefsRequest([]string{"symrefs\n"}))
	require.NoError(t, err)

	headOID := strings.Repeat("4", 40)
	want := pktLine(headOID+" HEAD\n") + "0000"
	assert.Equal(t, want, string(resp))
}

// TestLSRefs_Mixed_LooseOverridesPacked pins the merged-view behaviour
// of `IterRefs`: the loose `refs/heads/main` (3333...) wins over the
// packed entry (1111...). The packed-only ref `refs/heads/old`
// (2222...) is also emitted. C-locale sort puts `main` before `old`.
// HEAD is symbolic, so it lands first with the same OID as the loose
// `refs/heads/main`.
func TestLSRefs_Mixed_LooseOverridesPacked(t *testing.T) {
	store := openStoreFromFixture(t, "mixed")

	resp, err := runV2Session(t, store, buildLSRefsRequest(nil))
	require.NoError(t, err)

	mainOID := strings.Repeat("3", 40)
	oldOID := strings.Repeat("2", 40)
	want := pktLine(mainOID+" HEAD\n") +
		pktLine(mainOID+" refs/heads/main\n") +
		pktLine(oldOID+" refs/heads/old\n") +
		"0000"
	assert.Equal(t, want, string(resp))
}

// TestLSRefs_SHA256_Empty pins that the unborn-HEAD line uses the
// repository's hash algorithm — vacuously here, since the unborn line
// does not carry an OID, but the test still exercises a sha256 store
// flowing through the handler. The fixture has no refs, just a
// symbolic HEAD pointing at refs/heads/main.
func TestLSRefs_SHA256_Empty(t *testing.T) {
	store := openStoreFromFixture256(t, "sha256")

	req := buildLSRefsRequest([]string{"unborn\n", "symrefs\n"})
	resp, err := runV2Session(t, store, req)
	require.NoError(t, err)

	want := pktLine("unborn HEAD symref-target:refs/heads/main\n") + "0000"
	assert.Equal(t, want, string(resp))
}

// TestLSRefs_Reftable_Symrefs pins the response against a
// reftable-backed repo: HEAD is a symref to refs/heads/main, and
// refs/heads/main is the only value record. With `symrefs`, HEAD gets
// a `symref-target:` attribute. The test exercises both the reftable
// backend and the sha1 OID hex length.
func TestLSRefs_Reftable_Symrefs(t *testing.T) {
	store := openStoreFromFixture(t, "with-reftable-content")

	resp, err := runV2Session(t, store, buildLSRefsRequest([]string{"symrefs\n"}))
	require.NoError(t, err)

	want := pktLine(reftableContentMainSHA1+" HEAD symref-target:refs/heads/main\n") +
		pktLine(reftableContentMainSHA1+" refs/heads/main\n") +
		"0000"
	assert.Equal(t, want, string(resp))
}

// TestLSRefs_UnknownArg pins the structured-error path for an
// unrecognised argument: canonical's [ls-refs.c:188] calls `die()`. We
// surface the same condition as a wrapped [wire.ErrServerRefused] after
// emitting an `ERR ls-refs: unknown argument "<line>"` pkt-line + flush.
//
// [ls-refs.c:188]: https://github.com/git/git/blob/v2.54.0/ls-refs.c#L188
func TestLSRefs_UnknownArg(t *testing.T) {
	store := openEmptyStore(t)

	resp, err := runV2Session(t, store, buildLSRefsRequest([]string{"blah\n"}))
	require.Error(t, err)
	assert.True(t, errors.Is(err, wire.ErrServerRefused),
		"want errors.Is(err, wire.ErrServerRefused); got %v", err)

	want := pktLine(`ERR ls-refs: unknown argument "blah"`+"\n") + "0000"
	assert.Equal(t, want, string(resp))
}

// TestWriteLSRefsResponse_AllocsPerRef pins the per-ref allocation
// budget for `writeLSRefsResponse`'s ref-emission loop. After the
// scratch-buffer reuse and typed-`AppendHex` migration the loop
// body has no per-ref hex-string allocation and writes OID hex
// directly into the reused `[]byte` scratch. The budget is set loose
// enough not to flake on an off-by-one ref-count rounding (a small
// per-call constant amortised across 1000 refs) but tight enough to
// fail against either the pre-scratch five-alloc shape or the
// post-scratch / pre-AppendHex two-alloc shape.
//
// The fixture carries 1000 packed refs to amortise per-call constant
// overhead (the HEAD line, the iterator setup) so the per-ref
// average isolates the loop body's work.
func TestWriteLSRefsResponse_AllocsPerRef(t *testing.T) {
	const refCount = 1000
	const maxAllocsPerRef = 1.0

	for _, tc := range []struct {
		name string
		args wire.RefsArgs
	}{
		{name: "plain", args: wire.RefsArgs{}},
		{name: "peel", args: wire.RefsArgs{Peel: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := buildBenchPackedRefsRepo(t, refCount, true)
			w := pktline.NewWriter(io.Discard)

			avg := testing.AllocsPerRun(20, func() {
				if err := writeLSRefsResponse(w, store, tc.args); err != nil {
					t.Fatal(err)
				}
			})

			perRef := avg / float64(refCount)
			if perRef > maxAllocsPerRef {
				t.Fatalf("post-fix allocs/ref = %.2f (total %.0f / %d refs), want <= %.1f",
					perRef, avg, refCount, maxAllocsPerRef)
			}
		})
	}
}

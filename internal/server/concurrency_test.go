package server

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
	"github.com/hiddeco/go-ls-remote/internal/objstore"
	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/transport"
)

// TestServe_ConcurrentSessionsRaceClean is the cross-session `-race`
// probe for the in-process [Serve] emulator. It establishes the
// concurrency contract callers like the `transport/file` connection
// rely on: many independent sessions can run against a single shared
// [objstore.Store[objfmt.SHA1Hash]] without interfering, both within a protocol family
// (many v2 sessions racing over `IterRefs` / `Head` / `Peel` /
// `ObjectInfo`) and across families (a v0 advertisement loop racing a
// v2 command-dispatch loop over the same backend).
//
// The store's read methods are documented as "safe for concurrent use
// by multiple goroutines once the backend has been constructed"
// (`internal/objstore/ref_backend.go:14-17`); the per-method
// concurrency probes in `internal/objstore/concurrency_test.go` already
// fence each backend method against itself. This test fences the
// composition: every public path [Serve] takes through the store —
// the v2 advertisement's `Algo` query, the v2 ls-refs handler's
// `Head` + `IterRefs` + `Peel` triple, the v2 object-info handler's
// per-OID `ObjectInfo`, and the v0 advertisement's full-ref-list
// emission — runs simultaneously from a single shared store.
//
// The mix is deterministic per worker index: workers `w % 3 == 0`
// drive a v0 advertisement, `w % 3 == 1` drive a v2 ls-refs request
// (with `peel` and `symrefs` set so the response touches the peel
// cache and the symref decoration), and `w % 3 == 2` drive a v2
// object-info request for the packed commit OID. With 32 workers and
// 32 iterations apiece that is ~10 each per session shape; the race
// detector inspects the shared backend across every interleave the
// scheduler produces.
//
// Each goroutine asserts byte-equality against a per-shape expected
// stream. Byte-pinning here (rather than structural assertions) keeps
// the test honest: if a future shared-state change in `Serve` or any
// handler corrupts a per-session response, the diff surfaces in the
// assertion message rather than vanishing into a length-only check.
// The fixed expected bytes are computed once on the orchestrator from
// the same store every worker uses, so the test does not duplicate the
// byte-pinned single-session expectations elsewhere in the package.
func TestServe_ConcurrentSessionsRaceClean(t *testing.T) {
	t.Parallel()

	store := openConcurrentSessionsFixture(t)

	commitOID := packCommitOID
	commitHash, err := objfmt.ParseSHA1Hex(commitOID)
	require.NoError(t, err)
	commitInfo, err := store.ObjectInfo(commitHash)
	require.NoError(t, err)

	const agent = "test-agent/0.0"

	// Pre-compute the expected bytes for each session shape from the
	// orchestrator. The advertisement length is identical across all
	// v2 sessions and across all v0 sessions, so capturing it once and
	// reusing it inside every worker keeps the loop a tight assertion
	// sweep.
	wantV0 := pktLine(commitOID+" HEAD\x00symref=HEAD:refs/heads/main object-format=sha1 agent="+agent+"\n") +
		pktLine(commitOID+" refs/heads/main\n") +
		"0000"

	wantV2Ad := pktLine("version 2\n") +
		pktLine("agent="+agent+"\n") +
		pktLine("ls-refs=unborn\n") +
		pktLine("object-format=sha1\n") +
		pktLine("object-info\n") +
		"0000"

	wantLSRefsBody := pktLine(commitOID+" HEAD symref-target:refs/heads/main\n") +
		pktLine(commitOID+" refs/heads/main\n") +
		"0000"

	wantObjectInfoBody := pktLine("size\n") +
		pktLine(fmt.Sprintf("%s %d\n", commitOID, commitInfo.Size)) +
		"0000"

	workers := max(runtime.GOMAXPROCS(0)*4, 32)
	const iterations = 32

	var (
		wg    sync.WaitGroup
		start = make(chan struct{})
	)
	wg.Add(workers)
	for w := range workers {
		go func() {
			defer wg.Done()
			<-start
			for i := range iterations {
				switch w % 3 {
				case 0:
					// v0 advertisement: a single Serve call that emits
					// HEAD with the cap-laden first line, then the lone
					// non-HEAD ref, then a flush. Drives `Head`,
					// `IterRefs`, and the `object-format` advertisement
					// path through the shared store.
					got := runConcurrentAdvertise(t, store, transport.ProtocolV0, agent)
					if !assert.Equalf(t, wantV0, string(got),
						"worker %d iter %d: v0 advertisement drift", w, i) {
						return
					}

				case 1:
					// v2 advertisement followed by a single ls-refs
					// request with `peel` + `symrefs`. Drives the v2
					// capability advertisement, then the handler's
					// `Head` + symref decoration, `IterRefs` drain, and
					// `Peel` lookup. The shared `peelCache` install
					// races every other ls-refs worker on every iter.
					got := runConcurrentSession(t, store, agent, buildLSRefsRequest([]string{
						"peel\n", "symrefs\n",
					}))
					if !assert.Equalf(t, wantV2Ad+wantLSRefsBody, string(got),
						"worker %d iter %d: v2 ls-refs response drift", w, i) {
						return
					}

				case 2:
					// v2 advertisement followed by a single object-info
					// request for the packed commit. Drives the pack
					// lookup and CRC verification path of `ObjectInfo`
					// from many goroutines concurrently — the same
					// backend codepath the per-method probe in
					// `internal/objstore/concurrency_test.go` exercises,
					// here reached through `Serve`'s dispatcher.
					got := runConcurrentSession(t, store, agent, buildObjectInfoRequest([]string{
						"size\n", "oid " + commitOID + "\n",
					}))
					if !assert.Equalf(t, wantV2Ad+wantObjectInfoBody, string(got),
						"worker %d iter %d: v2 object-info response drift", w, i) {
						return
					}
				}
			}
		}()
	}
	close(start)
	wg.Wait()
}

// runConcurrentSession runs one [Serve] invocation in v2 mode against
// the shared store and returns the full byte stream the server emitted
// (advertisement + handler response). It is the worker-side sibling of
// [runV2Session] and [runAdvertise]: those helpers split the stream so
// the per-handler tests can compare against advertisement-stripped
// bodies, while this helper keeps the full stream so the byte-pinned
// concurrency assertions can cover the advertisement and response in a
// single comparison without re-running [writeV2Advertisement] inside
// the worker loop.
func runConcurrentSession(t *testing.T, store *objstore.Store[objfmt.SHA1Hash], agent string, request []byte) []byte {
	t.Helper()

	var sink bytes.Buffer
	r := pktline.NewReader(bytes.NewReader(request))
	w := pktline.NewWriter(&sink)

	// `assert` (not `require`) because the helper runs on worker
	// goroutines — `require.FailNow` is unsafe off the test goroutine.
	//nolint:testifylint // go-require / require-error tension; assert wins inside the worker goroutine
	assert.NoError(t, Serve(t.Context(), r, w, store, Options{
		Agent:             agent,
		PreferredProtocol: transport.ProtocolV2,
	}))
	return sink.Bytes()
}

// runConcurrentAdvertise runs one [Serve] invocation in advertisement-
// only mode (v0, or a v2 path that exits after the advertisement) and
// returns the bytes it emitted. It mirrors [runAdvertise] but accepts
// the protocol version so the same helper covers both v0 and v2
// advertisement-only sessions inside the worker loop.
func runConcurrentAdvertise(t *testing.T, store *objstore.Store[objfmt.SHA1Hash], proto transport.ProtocolVersion, agent string) []byte {
	t.Helper()

	// A single flush in the inbound stream is the v2 empty-request
	// terminator ([serve.c::process_request lines 314-321]); the v0
	// path returns before reading any inbound byte, so the flush is
	// harmless there.
	//
	// [serve.c::process_request lines 314-321]: https://github.com/git/git/blob/v2.54.0/serve.c#L314-L321
	src := bytes.NewReader([]byte("0000"))
	var sink bytes.Buffer
	r := pktline.NewReader(src)
	w := pktline.NewWriter(&sink)

	// `assert` (not `require`) because the helper runs on worker
	// goroutines — `require.FailNow` is unsafe off the test goroutine.
	//nolint:testifylint // go-require / require-error tension; assert wins inside the worker goroutine
	assert.NoError(t, Serve(t.Context(), r, w, store, Options{
		Agent:             agent,
		PreferredProtocol: proto,
	}))
	return sink.Bytes()
}

// openConcurrentSessionsFixture builds a self-contained store rooted at
// a fresh `t.TempDir()`. The shape combines real refs and real objects
// so every probe the worker loop fires lands on production code paths:
//
//   - HEAD is a symbolic ref to `refs/heads/main`, so `Head` returns a
//     non-zero OID and the v2 ls-refs handler's symref decoration path
//     fires.
//   - `packed-refs` resolves `refs/heads/main` to the canonical
//     `three-objects` pack's commit (the `packCommitOID` constant from
//     `object_info_test.go`), so the OID the worker queries via
//     `object-info` is the same OID exposed by `IterRefs` and `Head`.
//   - The `three-objects.{pack,idx}` pair from `testdata/objfmt/` lives
//     under `objects/pack/`, giving `ObjectInfo` a real CRC-verifiable
//     target on the shared pack backend.
//
// Built byte-by-byte rather than reusing [openStoreFromFixture] because
// no committed fixture combines a resolvable HEAD with the
// `three-objects` pack in one tree, and promoting the combination into
// `testdata/repos/` would couple two unrelated generators
// (`testdata/_gen/repos.sh` and `testdata/_gen/objfmt.sh`) for one
// test's benefit.
func openConcurrentSessionsFixture(t *testing.T) *objstore.Store[objfmt.SHA1Hash] {
	t.Helper()

	root := t.TempDir()
	gitDir := filepath.Join(root, ".git")
	require.NoError(t, os.MkdirAll(filepath.Join(gitDir, "objects", "pack"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(gitDir, "refs"), 0o755))

	require.NoError(t, os.WriteFile(filepath.Join(gitDir, "HEAD"),
		[]byte("ref: refs/heads/main\n"), 0o644))

	// `packed-refs` pins refs/heads/main at the real commit OID inside
	// the `three-objects` pack so HEAD, IterRefs, and ObjectInfo all
	// converge on the same OID.
	packedRefs := "" +
		"# pack-refs with: peeled fully-peeled sorted\n" +
		packCommitOID + " refs/heads/main\n"
	require.NoError(t, os.WriteFile(
		filepath.Join(gitDir, "packed-refs"),
		[]byte(packedRefs), 0o644))

	wd, err := os.Getwd()
	require.NoError(t, err)
	objfmtSrc := filepath.Join(wd, "..", "..", "testdata", "objfmt")
	for _, name := range []string{
		"three-objects.pack", "three-objects.idx",
	} {
		copyFileForTest(t,
			filepath.Join(objfmtSrc, name),
			filepath.Join(gitDir, "objects", "pack", name))
	}

	s, err := objstore.Open[objfmt.SHA1Hash](gitDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// copyFileForTest copies the file at src to dst with mode 0o644. Named
// to avoid colliding with the unexported `copyFile` in
// `internal/objstore/concurrency_test.go`; both helpers do the same job
// for their respective fixture builders.
func copyFileForTest(t *testing.T, src, dst string) {
	t.Helper()

	in, err := os.Open(src)
	require.NoError(t, err)
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	require.NoError(t, err)
	defer func() { _ = out.Close() }()
	_, err = io.Copy(out, in)
	require.NoError(t, err)
}

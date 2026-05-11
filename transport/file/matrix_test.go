package filet

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hiddeco/go-ls-remote/internal/testfixture"
	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/transport"
)

// TestFixtureMatrix is the consolidated end-to-end matrix for the
// local-filesystem transport. Each scenario opens a `file://` URL
// against a fresh copy of a fixture from `testdata/repos/`, drains the
// v2 advertisement, runs at least one v2 command, and asserts the
// response shape.
//
// The file transport's failure surface is narrower than HTTP's: there
// is no auth, no redirect, no dumb-vs-smart selection, and no
// status-code mapping. The matrix covers the success axes that DO
// vary across fixtures — ref backend (loose vs. packed vs. reftable),
// pack backend (idx-only vs. midx), hash algorithm (sha1 vs. sha256),
// alternates-chain transitive object resolution, and the unborn-HEAD
// gate. Failure cases (`ErrNotFound` for non-repo paths, malformed
// percent-decode) are covered in `open_test.go`; duplicating them here
// would not add coverage.
//
// Scenarios:
//
//   - empty: minimal `files+sha1` repo, no refs. Advertisement is just
//     `version 2\n` + caps + flush; ls-refs response is a flush-only
//     stream (no ref data lines).
//   - loose-only: refs spanning `refs/heads/*` and `refs/tags/*` in
//     loose form. ls-refs surfaces HEAD plus refs/heads/main.
//   - packed-only: refs only in `packed-refs`. Same surface shape as
//     loose-only at the ls-refs layer.
//   - mixed: loose `refs/heads/main` shadows the packed entry; the
//     packed-only `refs/heads/old` still surfaces.
//   - unborn-head: HEAD points at a missing ref; with `unborn` +
//     `symrefs` args the response carries an `unborn HEAD
//     symref-target:refs/heads/main` line.
//   - sha256: the advertisement's `object-format=sha256` capability
//     reflects the fixture's hash algorithm.
//   - midx-with-siblings: multi-pack-index pack backend. ls-refs runs
//     against an empty refs set, then object-info resolves a known OID
//     from the `three-objects.pack` payload.
//   - with-alternates-chain: A → B → C alternates chain. The dial path
//     opens A; transitive object resolution is the load-bearing
//     property the open exercises (ls-refs sees no refs because A's
//     own refs/ is empty).
//   - with-reftable-content: reftable ref backend with a real commit.
//     ls-refs surfaces refs/heads/main from the reftable stack.
func TestFixtureMatrix(t *testing.T) {
	t.Run("empty", testMatrixEmpty)
	t.Run("loose-only", testMatrixLooseOnly)
	t.Run("packed-only", testMatrixPackedOnly)
	t.Run("mixed", testMatrixMixed)
	t.Run("unborn-head", testMatrixUnbornHead)
	t.Run("sha256", testMatrixSHA256)
	t.Run("midx-with-siblings", testMatrixMidxWithSiblings)
	t.Run("with-alternates-chain", testMatrixWithAlternatesChain)
	t.Run("with-reftable-content", testMatrixWithReftableContent)
}

// openMatrixConn opens a `file://` Conn against gitdir, drains its v2
// advertisement, and returns both the [transport.Conn] and the
// capability bytes observed during the drain. Tests that need to
// assert on the advertised capabilities (e.g. `object-format=<algo>`)
// read them off the second return value.
//
// The returned value is the public [transport.Conn] interface rather
// than the algo-specialised `*Conn[H]` so the helper works for both
// SHA-1 and SHA-256 fixtures; tests that need command access call
// the interface methods.
func openMatrixConn(t *testing.T, gitdir string) (transport.Conn, []string) {
	t.Helper()
	u, err := transport.ParseURL("file://" + gitdir)
	require.NoError(t, err)

	tr := New()
	conn, err := tr.Open(context.Background(), u, transport.OpenOptions{
		UserAgent: "matrix-test/0.0",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	// Drain the advertisement, capturing the data lines so callers can
	// assert on advertised capabilities. The v2 advertisement begins
	// with `version 2\n`, then one cap per line, then a flush
	// (`serve.c::protocol_v2_advertise_capabilities`).
	rdr := conn.Advertisement()
	var caps []string
	for {
		p, err := rdr.ReadPacket()
		require.NoError(t, err)
		if p.Kind == pktline.Flush {
			break
		}
		if p.Kind == pktline.Data {
			caps = append(caps, strings.TrimRight(string(p.Data), "\n"))
		}
	}
	return conn, caps
}

func testMatrixEmpty(t *testing.T) {
	gitdir := materializeServeableFixture(t, "empty")
	c, caps := openMatrixConn(t, gitdir)

	assert.Contains(t, caps, "version 2",
		"v2 advertisement must lead with the version line")
	assertHasCap(t, caps, "object-format=sha1")

	// `empty` carries no refs, only an unborn HEAD. Without the
	// `unborn` arg the server must skip HEAD entirely
	// (`ls-refs.c::send_possibly_unborn_head` returns early), so the
	// response is a flush-only stream with no data packets.
	rdr, err := c.Command(context.Background(), "ls-refs",
		cmdBody("ls-refs", nil, []string{"object-format=sha1"}))
	require.NoError(t, err)

	pkts := readAllPackets(t, rdr)
	require.NotEmpty(t, pkts, "even an empty ls-refs response must carry the trailing flush")
	for _, p := range pkts {
		assert.NotEqual(t, pktline.Data, p.Kind,
			"empty fixture's ls-refs must produce no data packets, got %q", p.Data)
	}
	require.Equal(t, pktline.Flush, pkts[len(pkts)-1].Kind)
}

func testMatrixLooseOnly(t *testing.T) {
	gitdir := materializeServeableFixture(t, "loose-only")
	c, caps := openMatrixConn(t, gitdir)
	assertHasCap(t, caps, "object-format=sha1")

	rdr, err := c.Command(context.Background(), "ls-refs",
		cmdBody("ls-refs",
			[]string{"peel", "symrefs"}, []string{"object-format=sha1"}))
	require.NoError(t, err)

	lines := dataLines(t, rdr)
	// HEAD points at refs/heads/main (oid `aaaa...`); the loose backend
	// resolves the symref to its OID, and the symrefs arg surfaces the
	// `symref-target:` attribute on the HEAD line.
	requireRefLine(t, lines, " HEAD")
	requireRefLine(t, lines, " refs/heads/main\n")
	requireRefLine(t, lines, " refs/heads/feature/x\n")
	requireRefLine(t, lines, " refs/tags/v1\n")
}

func testMatrixPackedOnly(t *testing.T) {
	gitdir := materializeServeableFixture(t, "packed-only")
	c, _ := openMatrixConn(t, gitdir)

	rdr, err := c.Command(context.Background(), "ls-refs",
		cmdBody("ls-refs", []string{"peel"}, []string{"object-format=sha1"}))
	require.NoError(t, err)

	lines := dataLines(t, rdr)
	// `packed-only` packs main and v1 (annotated) in `packed-refs`.
	// HEAD resolves to the packed oid for main; v1 carries a `peeled:`
	// attribute because the `packed-refs` header advertised
	// `peeled fully-peeled` and the `^peel` line is present.
	requireRefLine(t, lines, " HEAD")
	requireRefLine(t, lines, " refs/heads/main\n")
	requireRefLine(t, lines, " refs/tags/v1 peeled:")
}

func testMatrixMixed(t *testing.T) {
	gitdir := materializeServeableFixture(t, "mixed")
	c, _ := openMatrixConn(t, gitdir)

	rdr, err := c.Command(context.Background(), "ls-refs",
		cmdBody("ls-refs", nil, []string{"object-format=sha1"}))
	require.NoError(t, err)

	lines := dataLines(t, rdr)
	// `mixed` has a loose `refs/heads/main` (`3333...`) shadowing the
	// packed `1111...`, plus a packed-only `refs/heads/old` (`2222...`).
	// The loose-overrides-packed precedence is what we pin.
	mainLine := requireRefLine(t, lines, " refs/heads/main\n")
	assert.True(t, strings.HasPrefix(mainLine, strings.Repeat("3", 40)),
		"loose main must shadow the packed entry; got %q", mainLine)
	requireRefLine(t, lines, " refs/heads/old\n")
}

func testMatrixUnbornHead(t *testing.T) {
	gitdir := materializeServeableFixture(t, "unborn-head")
	c, caps := openMatrixConn(t, gitdir)
	// The server advertises `ls-refs=unborn` so a v2 client knows the
	// `unborn` argument is recognised (`ls-refs.c:153`'s capability
	// echo). Pinning the cap shape protects against accidental
	// regressions in the advertise path.
	assertHasCap(t, caps, "ls-refs=unborn")

	rdr, err := c.Command(context.Background(), "ls-refs",
		cmdBody("ls-refs",
			[]string{"symrefs", "unborn"}, []string{"object-format=sha1"}))
	require.NoError(t, err)

	lines := dataLines(t, rdr)
	// Unborn HEAD path: `head.OID == 0` and `head.Symref ==
	// "refs/heads/main"`, so the handler emits
	// `unborn HEAD symref-target:refs/heads/main\n`
	// (`ls-refs.c:91-94`). Without the `unborn` arg the line would be
	// suppressed entirely.
	line := requireRefLine(t, lines, " HEAD symref-target:refs/heads/main\n")
	assert.True(t, strings.HasPrefix(line, "unborn "),
		"an unborn HEAD must take `unborn` in the OID slot, not a zero hash; got %q", line)
}

func testMatrixSHA256(t *testing.T) {
	gitdir := materializeServeableFixture(t, "sha256")
	_, caps := openMatrixConn(t, gitdir)
	// The fixture flips `extensions.objectFormat` to `sha256`; the
	// advertise path must surface that through the
	// `object-format=sha256` capability so callers (and a peer's
	// `parse_advertised_object_format`) can pick the right hash.
	assertHasCap(t, caps, "object-format=sha256")
	assert.NotContains(t, caps, "object-format=sha1",
		"a sha256 fixture must not also advertise object-format=sha1")
}

func testMatrixMidxWithSiblings(t *testing.T) {
	gitdir := materializeServeableFixture(t, "midx-with-siblings")
	c, _ := openMatrixConn(t, gitdir)

	// `midx-with-siblings` has no refs (HEAD is unborn, refs/ empty),
	// so a default ls-refs is a flush-only stream. We exercise the
	// command path so the encoder + dispatcher are fully drained
	// before the next command on the same Conn.
	rdr, err := c.Command(context.Background(), "ls-refs",
		cmdBody("ls-refs", nil, []string{"object-format=sha1"}))
	require.NoError(t, err)
	_ = readAllPackets(t, rdr)

	// `26dae744...` is the canonical `three-objects.pack` commit OID
	// (mirrors `internal/objstore/idx_catalog_test.go`'s
	// `threeCommitOID` constant). The midx backend serves it via the
	// `three-objects` sibling pack; this proves the file transport's
	// command loop reaches the pack backend through the in-process
	// goroutine.
	const threeCommitOID = "26dae744f51e61913f50bd402cbe63953c7d637b"
	rdr, err = c.Command(context.Background(), "object-info",
		cmdBody("object-info",
			[]string{"size", "oid " + threeCommitOID},
			[]string{"object-format=sha1"}))
	require.NoError(t, err)

	lines := dataLines(t, rdr)
	require.NotEmpty(t, lines, "object-info must produce data lines for a hit")
	requireDataLine(t, lines, "size")
	// The per-OID line is `<oid> <size>\n` per `protocol-caps.c::send_info`.
	requireRefLine(t, lines, threeCommitOID+" ")
}

func testMatrixWithAlternatesChain(t *testing.T) {
	// The chain fixture ships three sibling repos (a/, b/, c/) under a
	// single fixture root with no top-level dotgit/. `MaterializeRepo`
	// would fail the test on that layout; `MaterializeRepoTree`
	// returns the destination root so the test can target `a/.git`
	// directly — that is the entry point the alternates-chain test in
	// `internal/objstore` uses too.
	root := testfixture.MaterializeRepoTree(t, "with-alternates-chain")
	gitdir := filepath.Join(root, "a", ".git")

	c, caps := openMatrixConn(t, gitdir)
	assertHasCap(t, caps, "object-format=sha1")

	// `a` carries no refs of its own (refs/ is empty, HEAD is unborn),
	// but the alternates chain must open without error — that is the
	// load-bearing property: `objstore.Open` resolves the relative
	// `../../../b/.git/objects` entry against `a/.git/objects/` and
	// transitively through B to C without surfacing
	// `ErrCorruptObject`. A clean ls-refs flush proves the server
	// goroutine reached its command loop on the chained store.
	rdr, err := c.Command(context.Background(), "ls-refs",
		cmdBody("ls-refs", nil, []string{"object-format=sha1"}))
	require.NoError(t, err)
	pkts := readAllPackets(t, rdr)
	require.NotEmpty(t, pkts)
	require.Equal(t, pktline.Flush, pkts[len(pkts)-1].Kind)
}

func testMatrixWithReftableContent(t *testing.T) {
	gitdir := materializeServeableFixture(t, "with-reftable-content")
	c, caps := openMatrixConn(t, gitdir)
	assertHasCap(t, caps, "object-format=sha1")

	rdr, err := c.Command(context.Background(), "ls-refs",
		cmdBody("ls-refs", []string{"symrefs"}, []string{"object-format=sha1"}))
	require.NoError(t, err)

	lines := dataLines(t, rdr)
	// `with-reftable-content` carries one commit on
	// `refs/heads/main`; the reftable backend yields HEAD (a symref to
	// main) plus refs/heads/main itself. The reftable backend must
	// resolve through the same `Store.IterRefs` surface the loose and
	// packed backends use.
	requireRefLine(t, lines, " HEAD")
	requireRefLine(t, lines, " refs/heads/main\n")
}

// dataLines drains rdr through its trailing flush and returns the data
// packets' string payloads in stream order. The helper is the matrix
// test's lingua franca: each scenario asserts on the presence (or
// absence) of well-formed ref lines, never on the precise byte stream.
func dataLines(t *testing.T, rdr *pktline.Reader) []string {
	t.Helper()
	pkts := readAllPackets(t, rdr)
	var lines []string
	for _, p := range pkts {
		if p.Kind == pktline.Data {
			lines = append(lines, string(p.Data))
		}
	}
	return lines
}

// requireRefLine asserts that lines contains at least one entry with
// substr; on success it returns the matched line so further per-line
// assertions can chain off it without re-scanning.
func requireRefLine(t *testing.T, lines []string, substr string) string {
	t.Helper()
	for _, l := range lines {
		if strings.Contains(l, substr) {
			return l
		}
	}
	require.FailNowf(t, "missing ref line",
		"expected a line containing %q in %q", substr, lines)
	return ""
}

// requireDataLine asserts that lines contains an entry whose trimmed
// payload exactly equals want. It is the strict-equality complement to
// [requireRefLine] for the `size` attrs line and similar fixed
// payloads.
func requireDataLine(t *testing.T, lines []string, want string) {
	t.Helper()
	for _, l := range lines {
		if strings.TrimRight(l, "\n") == want {
			return
		}
	}
	require.FailNowf(t, "missing data line",
		"expected an exact line %q in %q", want, lines)
}

// assertHasCap asserts that caps (the v2 advertisement's data lines)
// contains an exact match for want. The advertised cap list ordering
// is canonical (`agent`, `ls-refs[...]`, `fetch...`, `server-option`,
// `object-format=<algo>`, `object-info`); pinning order would couple
// the matrix to that internal contract more tightly than necessary, so
// the assertion only checks membership.
func assertHasCap(t *testing.T, caps []string, want string) {
	t.Helper()
	if slices.Contains(caps, want) {
		return
	}
	assert.Failf(t, "missing capability",
		"expected %q in advertised caps %q", want, caps)
}

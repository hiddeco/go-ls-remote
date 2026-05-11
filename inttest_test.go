package lsremote_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	lsremote "github.com/hiddeco/go-ls-remote"
	"github.com/hiddeco/go-ls-remote/internal/inttest"
	"github.com/hiddeco/go-ls-remote/internal/objfmt"
	"github.com/hiddeco/go-ls-remote/internal/objstore"
	"github.com/hiddeco/go-ls-remote/transport"
	filet "github.com/hiddeco/go-ls-remote/transport/file"
	gitt "github.com/hiddeco/go-ls-remote/transport/git"
	httpt "github.com/hiddeco/go-ls-remote/transport/http"
	ssht "github.com/hiddeco/go-ls-remote/transport/ssh"
)

// TestRefs_AcrossAllTransports drives every fixture in [inttest.Entries]
// through every supported transport (HTTP, HTTPS, SSH, git daemon,
// `file://`) and asserts that the public discovery surface — `Refs`,
// `ListRefs`, `DefaultBranch`, `ObjectInfo` — returns identical results
// regardless of which wire the bytes flowed over.
//
// The cross-transport invariant is the suite's reason to exist: a
// regression in the wire codec, the in-process server emulator, or the
// `transport.URL` → root conversion would show up here as a divergence
// between, say, the file transport's `ListRefs` and the HTTP transport's
// `ListRefs` on the same fixture. The matrix is data-driven; individual
// `t.Run` cases name the (fixture, transport) pair so a failure pinpoints
// both axes.
func TestRefs_AcrossAllTransports(t *testing.T) {
	for _, entry := range inttest.Entries() {
		t.Run(entry.Name, func(t *testing.T) {
			for _, tp := range transports() {
				t.Run(tp.name, func(t *testing.T) {
					runEquivalence(t, entry, tp)
				})
			}
		})
	}
}

// runEquivalence materialises entry, stands up tp's server, and drives
// the public API through it, asserting every result against the entry's
// declared expectations.
func runEquivalence(t *testing.T, entry inttest.Entry, tp transportSetup) {
	t.Helper()

	gitdir := entry.Materialize(t)
	ep := tp.start(t, entry, gitdir)

	ctx := context.Background()
	opts := []lsremote.Option{
		lsremote.WithTransports(ep.registry),
		lsremote.WithProtocol(lsremote.ProtocolV2),
	}

	// Session-level checks: open one session and exercise every command
	// against it. This shape catches a Session that fails to reuse the
	// underlying transport for follow-up commands (the bug the v2 split
	// between advertisement and command loop guards against).
	session, err := lsremote.Dial(ctx, ep.url, opts...)
	require.NoError(t, err, "Dial(%s)", ep.url)
	t.Cleanup(func() { _ = session.Close() })

	caps := session.Capabilities()
	assert.Equal(t, lsremote.ProtocolV2, caps.Version,
		"every harness is wired for v2; advertisement must negotiate v2")
	assert.Equal(t, entry.ObjectFormat, caps.ObjectFormat,
		"capability `object-format` must match the fixture's hash algorithm")

	// `Refs` (iterator) and `ListRefs` (slice) share the same v2
	// `ls-refs` exchange; collect each into a slice and assert they
	// agree as a sanity check on the two APIs.
	refsArgs := lsremote.RefsRequest{
		Symrefs: true,
		Peel:    true,
		Unborn:  entry.Unborn,
	}
	iterRefs := collectRefs(t, session, refsArgs)
	listRefs, err := session.ListRefs(ctx, refsArgs)
	require.NoError(t, err)
	require.Equal(t, len(iterRefs), len(listRefs),
		"Refs iterator and ListRefs must agree on length")
	for i := range iterRefs {
		assert.Equal(t, iterRefs[i], listRefs[i],
			"Refs[%d] vs ListRefs[%d] mismatch", i, i)
	}

	inttest.CompareRefs(t, listRefs, entry.ExpectedRefs, entry.Name)
	inttest.CompareHEAD(t, listRefs, entry)

	// `DefaultBranch` is a thin wrapper around `ls-refs` with
	// `ref-prefix HEAD` and `symrefs`; it must surface the same symref
	// target the wire emits. Detached fixtures advertise no symref, so
	// the helper returns `ErrNoDefaultBranch`.
	checkDefaultBranch(t, ctx, ep, entry, opts)

	// Top-level `Tags` and `Heads` helpers re-dial; pass the same
	// registry so they hit the harness rather than the package default.
	checkPrefixHelpers(t, ctx, ep, entry, opts)

	// `ObjectInfo` is exercised only for entries whose declared OIDs
	// resolve to real on-disk objects; synthetic-OID fixtures leave the
	// map empty.
	if len(entry.ExpectedObjectInfo) > 0 {
		oids := make([]string, 0, len(entry.ExpectedObjectInfo))
		for hash := range entry.ExpectedObjectInfo {
			oids = append(oids, hash)
		}
		gotInfos, err := session.ObjectInfo(ctx, oids,
			lsremote.ObjectInfoRequest{Size: true})
		require.NoError(t, err, "%s/%s: ObjectInfo", entry.Name, tp.name)
		inttest.CompareObjectInfo(t, gotInfos, entry.ExpectedObjectInfo, entry.Name)
	}
}

// collectRefs drains [Session.Refs] into a slice, failing the test on
// any iterator-yielded error.
func collectRefs(t *testing.T, s *lsremote.Session, args lsremote.RefsRequest) []lsremote.Ref {
	t.Helper()
	seq, err := s.Refs(context.Background(), args)
	require.NoError(t, err)
	var refs []lsremote.Ref
	for ref, err := range seq {
		require.NoError(t, err)
		refs = append(refs, ref)
	}
	return refs
}

// checkDefaultBranch exercises the top-level [DefaultBranch] helper.
//
// Three shapes arise across the matrix:
//
//   - Resolved HEAD: `DefaultBranch` returns [Entry.ExpectedDefaultBranch].
//   - Unborn HEAD: the v2 `ls-refs` exchange the helper drives sets
//     the `unborn` argument (mirroring `connect.c:591-592`), so the
//     server emits an unborn HEAD entry whose `symref-target:` carries
//     the branch name. `DefaultBranch` surfaces that target verbatim,
//     so an unborn HEAD is indistinguishable from a resolved HEAD at
//     this layer — both produce [Entry.ExpectedDefaultBranch].
//   - Detached HEAD: HEAD is a raw OID with no symref attribute, so
//     the helper returns a [*ProtocolError] whose chain matches
//     [ErrNoDefaultBranch] regardless of the wire it travelled.
func checkDefaultBranch(t *testing.T, ctx context.Context,
	ep endpoint, entry inttest.Entry, opts []lsremote.Option) {
	t.Helper()
	got, err := lsremote.DefaultBranch(ctx, ep.url, opts...)
	if entry.Detached {
		require.Error(t, err, "%s: detached HEAD must error", entry.Name)
		assert.True(t, errors.Is(err, lsremote.ErrNoDefaultBranch),
			"%s: must surface ErrNoDefaultBranch; got %v", entry.Name, err)
		return
	}
	require.NoError(t, err, "%s: DefaultBranch", entry.Name)
	assert.Equal(t, entry.ExpectedDefaultBranch, got,
		"%s: DefaultBranch", entry.Name)
}

// checkPrefixHelpers exercises [Tags] and [Heads]. Each is a thin
// wrapper around [Refs] with a fixed prefix; the cross-transport
// guarantee here is that the prefix filter survives every transport's
// command path. The shared expectations are derived from the entry's
// declared ref set.
func checkPrefixHelpers(t *testing.T, ctx context.Context,
	ep endpoint, entry inttest.Entry, opts []lsremote.Option) {
	t.Helper()

	var wantTags, wantHeads []inttest.ExpectedRef
	for _, r := range entry.ExpectedRefs {
		switch {
		case strings.HasPrefix(r.Name, "refs/tags/"):
			wantTags = append(wantTags, r)
		case strings.HasPrefix(r.Name, "refs/heads/"):
			wantHeads = append(wantHeads, r)
		}
	}

	tagSeq, err := lsremote.Tags(ctx, ep.url, opts...)
	require.NoError(t, err, "%s: Tags", entry.Name)
	gotTags := drain(t, tagSeq)
	inttest.CompareRefs(t, gotTags, wantTags, entry.Name+"/tags")

	headSeq, err := lsremote.Heads(ctx, ep.url, opts...)
	require.NoError(t, err, "%s: Heads", entry.Name)
	gotHeads := drain(t, headSeq)
	inttest.CompareRefs(t, gotHeads, wantHeads, entry.Name+"/heads")
}

// drain materialises a [Refs] iterator into a slice, surfacing any
// iterator-yielded error as a test failure.
func drain(t *testing.T, seq func(yield func(lsremote.Ref, error) bool)) []lsremote.Ref {
	t.Helper()
	var refs []lsremote.Ref
	for ref, err := range seq {
		require.NoError(t, err)
		refs = append(refs, ref)
	}
	return refs
}

// endpoint is what a [transportSetup.start] returns: the URL to dial
// and the [transport.Registry] preconfigured for it. Bundling them
// keeps the per-transport setup closure return type uniform.
type endpoint struct {
	url      string
	registry *transport.Registry
}

// transportSetup names one transport and provides the constructor that
// stands up a server for a given fixture. The harnesses register their
// cleanups on the supplied [*testing.T], so the closure has no return
// channel for teardown.
type transportSetup struct {
	name  string
	start func(t *testing.T, entry inttest.Entry, gitdir string) endpoint
}

// transports returns the set of (name, constructor) pairs the
// cross-transport suite drives. Order is presentation-only; each row
// is independent.
func transports() []transportSetup {
	return []transportSetup{
		{name: "http", start: startHTTP},
		{name: "https", start: startHTTPS},
		{name: "ssh", start: startSSH},
		{name: "git", start: startGit},
		{name: "file", start: startFile},
	}
}

// startHTTP stands up a plain `http://` smart-HTTP harness and returns
// a registry wired with the default [httpt.Transport].
func startHTTP(t *testing.T, entry inttest.Entry, gitdir string) endpoint {
	t.Helper()
	base := openServer(t, entry, gitdir, func(s any) string {
		switch store := s.(type) {
		case *objstore.Store[objfmt.SHA1Hash]:
			return inttest.NewHTTPServer(t, store)
		case *objstore.Store[objfmt.SHA256Hash]:
			return inttest.NewHTTPServer(t, store)
		default:
			t.Fatalf("unexpected store type %T", s)
			return ""
		}
	})
	return endpoint{
		url:      base + "/repo.git",
		registry: transport.NewRegistry(httpt.New()),
	}
}

// startHTTPS stands up a TLS harness and returns a registry whose
// [httpt.Transport] trusts the harness's self-signed certificate.
func startHTTPS(t *testing.T, entry inttest.Entry, gitdir string) endpoint {
	t.Helper()
	var (
		baseURL string
		client  *http.Client
	)
	openServer(t, entry, gitdir, func(s any) string {
		switch store := s.(type) {
		case *objstore.Store[objfmt.SHA1Hash]:
			baseURL, client = inttest.NewHTTPSServer(t, store)
		case *objstore.Store[objfmt.SHA256Hash]:
			baseURL, client = inttest.NewHTTPSServer(t, store)
		default:
			t.Fatalf("unexpected store type %T", s)
		}
		return baseURL
	})
	return endpoint{
		url:      baseURL + "/repo.git",
		registry: transport.NewRegistry(httpt.New(httpt.WithClient(client))),
	}
}

// startSSH stands up an SSH harness and returns a registry whose
// [ssht.Transport] pins the harness's host key and offers a fresh
// in-memory ed25519 signer (the harness accepts any pubkey).
func startSSH(t *testing.T, entry inttest.Entry, gitdir string) endpoint {
	t.Helper()
	var srv *inttest.SSHServer
	openServer(t, entry, gitdir, func(s any) string {
		switch store := s.(type) {
		case *objstore.Store[objfmt.SHA1Hash]:
			srv = inttest.NewSSHServer(t, store)
		case *objstore.Store[objfmt.SHA256Hash]:
			srv = inttest.NewSSHServer(t, store)
		default:
			t.Fatalf("unexpected store type %T", s)
		}
		return srv.URL()
	})

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer, err := ssh.NewSignerFromKey(priv)
	require.NoError(t, err)

	tr := ssht.New(
		ssht.WithAuth(ssht.Signer(signer)),
		ssht.WithKnownHosts(srv.HostKeyCallback()),
	)
	return endpoint{
		url:      srv.URL(),
		registry: transport.NewRegistry(tr),
	}
}

// startGit stands up a git-daemon harness and returns a registry wired
// with the default [gitt.Transport].
func startGit(t *testing.T, entry inttest.Entry, gitdir string) endpoint {
	t.Helper()
	var url string
	openServer(t, entry, gitdir, func(s any) string {
		switch store := s.(type) {
		case *objstore.Store[objfmt.SHA1Hash]:
			url = inttest.NewGitServer(t, store)
		case *objstore.Store[objfmt.SHA256Hash]:
			url = inttest.NewGitServer(t, store)
		default:
			t.Fatalf("unexpected store type %T", s)
		}
		return url
	})
	return endpoint{
		url:      url,
		registry: transport.NewRegistry(gitt.New()),
	}
}

// startFile bypasses every network server and points the file
// transport directly at the materialised gitdir. The file transport
// owns its own in-process upload-pack loop, so the harness adds
// nothing beyond URL construction.
func startFile(t *testing.T, _ inttest.Entry, gitdir string) endpoint {
	t.Helper()
	return endpoint{
		url:      "file://" + gitdir,
		registry: transport.NewRegistry(filet.New()),
	}
}

// openServer opens the entry's store, hands it to fn (which constructs
// the transport-specific harness against the typed value), and
// registers a cleanup that closes the store at test end. The any-typed
// boundary keeps the per-transport startXxx helpers free of a generic
// dispatch each.
//
// The returned string is fn's return value, propagated to the caller.
// startXxx helpers that need a typed handle past this point (the SSH
// path needs `*SSHServer`, the HTTPS path needs `*http.Client`)
// capture into outer-scope variables from inside fn rather than going
// through this return value.
func openServer(t *testing.T, entry inttest.Entry, gitdir string,
	fn func(store any) string) string {
	t.Helper()
	switch entry.ObjectFormat {
	case lsremote.ObjectFormatSHA1:
		store, err := objstore.Open[objfmt.SHA1Hash](gitdir)
		require.NoError(t, err)
		t.Cleanup(func() { _ = store.Close() })
		return fn(store)
	case lsremote.ObjectFormatSHA256:
		store, err := objstore.Open[objfmt.SHA256Hash](gitdir)
		require.NoError(t, err)
		t.Cleanup(func() { _ = store.Close() })
		return fn(store)
	default:
		t.Fatalf("unsupported object format %q", entry.ObjectFormat)
		return ""
	}
}

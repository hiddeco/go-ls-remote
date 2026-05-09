package server

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/hiddeco/go-ls-remote/internal/objstore"
	"github.com/hiddeco/go-ls-remote/internal/wire"
	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/transport"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// materializeRepoFixture copies the named fixture from
// `testdata/repos/<name>/` into a fresh `t.TempDir()`, renaming the
// committed `dotgit` component to `.git`. It generalises
// [materializeEmptyRepo]: canonical Git refuses to track a path
// containing a literal `.git` component (see `path.c::is_dotgit_path`),
// so the on-disk fixtures store the gitdir under a `dotgit/` directory
// and tests rename it on materialization.
func materializeRepoFixture(t *testing.T, name string) string {
	t.Helper()

	wd, err := os.Getwd()
	require.NoError(t, err)
	src := filepath.Join(wd, "..", "..", "testdata", "repos", name)
	info, err := os.Stat(src)
	require.NoError(t, err, "fixture %q missing; regenerate with testdata/_gen/repos.sh", name)
	require.True(t, info.IsDir())

	dst := t.TempDir()
	require.NoError(t, filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		parts := splitPath(rel)
		for i, part := range parts {
			if part == "dotgit" {
				parts[i] = ".git"
			}
		}
		target := filepath.Join(append([]string{dst}, parts...)...)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	}))
	return filepath.Join(dst, ".git")
}

// splitPath splits a slash- or separator-delimited path into its
// non-empty components in order. The empty/`.` input yields nil. It
// underpins the per-segment `dotgit`→`.git` rename in
// [materializeRepoFixture].
func splitPath(p string) []string {
	if p == "." {
		return nil
	}
	var parts []string
	for {
		dir, file := filepath.Split(p)
		if file != "" {
			parts = append([]string{file}, parts...)
		}
		if dir == "" {
			break
		}
		p = filepath.Clean(dir)
		if p == "." || p == string(filepath.Separator) {
			break
		}
	}
	return parts
}

// pktLine encodes payload as a single pkt-line, prefixing it with a
// 4-hex-digit length field that includes the prefix itself. The helper
// keeps the byte-pinned expectations in this file readable.
func pktLine(payload string) string {
	return fmt.Sprintf("%04x%s", 4+len(payload), payload)
}

// runAdvertise runs [Serve] synchronously against the given store and
// options, returning the bytes it emitted. The client-to-server side
// is a [bytes.Reader] preloaded with a single flush packet — the v2
// empty-request terminator from `serve.c::process_request` lines
// 314-321 — so the v2 command loop exits cleanly. The v0 path returns
// before reading any client byte, so the preloaded flush is harmless
// there too.
func runAdvertise(t *testing.T, store *objstore.Store, opts Options) []byte {
	t.Helper()

	src := bytes.NewReader([]byte("0000"))
	var sink bytes.Buffer

	r := pktline.NewReader(src)
	w := pktline.NewWriter(&sink)

	require.NoError(t, Serve(context.Background(), r, w, store, opts))
	return sink.Bytes()
}

// TestServe_V2AdvertisementBytes pins the full v2 capability
// advertisement against the `empty` fixture (sha1) with an explicit
// agent string. The byte-pinned literal exercises the canonical
// emission order (`agent`, `ls-refs`, `object-format`, `object-info`)
// from `serve.c::protocol_v2_advertise_capabilities` and the pkt-line
// framing from `pkt-line.c::packet_write`.
func TestServe_V2AdvertisementBytes(t *testing.T) {
	store := openEmptyStore(t)

	got := runAdvertise(t, store, Options{
		Agent:             "test-agent/0.0",
		PreferredProtocol: transport.ProtocolV2,
	})

	want := pktLine("version 2\n") +
		pktLine("agent=test-agent/0.0\n") +
		pktLine("ls-refs=unborn\n") +
		pktLine("object-format=sha1\n") +
		pktLine("object-info\n") +
		"0000"
	assert.Equal(t, want, string(got))
}

// TestServe_V2AdvertisementDefaultsAgent pins the fallback when
// [Options.Agent] is empty: the server advertises
// [wire.DefaultUserAgent], matching `serve.c::agent_advertise`
// lines 25-31 (canonical Git falls back to `git_user_agent_sanitized`).
func TestServe_V2AdvertisementDefaultsAgent(t *testing.T) {
	store := openEmptyStore(t)

	got := runAdvertise(t, store, Options{
		PreferredProtocol: transport.ProtocolV2,
	})

	wantAgent := pktLine("agent=" + wire.DefaultUserAgent + "\n")
	assert.Contains(t, string(got), wantAgent,
		"want agent line %q in advertisement", wantAgent)
}

// TestServe_V2AdvertisementSHA256 pins the `object-format` line for a
// sha256 repository, matching `serve.c::object_format_advertise`
// lines 53-58 (canonical Git emits the repository's
// `the_hash_algo->name`).
func TestServe_V2AdvertisementSHA256(t *testing.T) {
	gitdir := materializeRepoFixture(t, "sha256")
	store, err := objstore.Open(gitdir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	got := runAdvertise(t, store, Options{
		Agent:             "test-agent/0.0",
		PreferredProtocol: transport.ProtocolV2,
	})

	want := pktLine("version 2\n") +
		pktLine("agent=test-agent/0.0\n") +
		pktLine("ls-refs=unborn\n") +
		pktLine("object-format=sha256\n") +
		pktLine("object-info\n") +
		"0000"
	assert.Equal(t, want, string(got))
}

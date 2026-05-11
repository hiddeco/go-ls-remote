package server

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
	"github.com/hiddeco/go-ls-remote/internal/objstore"
	"github.com/hiddeco/go-ls-remote/internal/testfixture"
	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/stretchr/testify/require"
)

// canonicalDir returns the absolute path to the canonical-corpus
// directory for fixture. The path is derived from this source file's
// own location via [runtime.Caller] — same shape as
// [testfixture.MaterializeRepo] — so the resolution is independent
// of the test's working directory.
func canonicalDir(t testing.TB, fixture string) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller(0) failed; package layout changed?")
	// `file` is `<module>/internal/server/canonical_test.go`; walk
	// up to the module root and join `testdata/canonical/<fixture>`.
	return filepath.Join(filepath.Dir(file), "..", "..", "testdata", "canonical", fixture)
}

// readCanonical reads the canonical-corpus artifact at
// `testdata/canonical/<fixture>/<name>` and fails the test on
// missing file. The bytes returned are the committed baseline that
// `internal/server`'s output is compared against by every test in
// this file.
func readCanonical(t testing.TB, fixture, name string) []byte {
	t.Helper()
	path := filepath.Join(canonicalDir(t, fixture), name)
	data, err := os.ReadFile(path)
	require.NoError(t, err, "read canonical artifact %s/%s", fixture, name)
	return data
}

// openCanonicalStore materialises fixture into a fresh `t.TempDir()`
// and opens an [objstore.Store] rooted at its `.git` directory. Use
// this in the byte-equivalence harness so the fixture handed to
// `Serve` is the exact tree canonical Git captured against.
func openCanonicalStore(t *testing.T, fixture string) *objstore.Store[objfmt.SHA1Hash] {
	t.Helper()
	gitdir := testfixture.MaterializeRepo(t, fixture)
	require.NoError(t, os.MkdirAll(filepath.Join(gitdir, "objects", "pack"), 0o755))
	store, err := objstore.Open[objfmt.SHA1Hash](gitdir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// TestCanonical_AdvertisementV2_empty pins the v2 capability
// advertisement against canonical Git's bytes for the `empty`
// fixture. The mask normalises the agent line and drops the
// capabilities the two sides legitimately diverge on (canonical
// advertises `fetch=` and `server-option`; this emulator
// advertises `object-info`) so the test asserts byte-equivalence
// on the substantive common subset: version line, ordering, framing,
// and the `agent`, `ls-refs`, `object-format` cap lines.
func TestCanonical_AdvertisementV2_empty(t *testing.T) {
	want := readCanonical(t, "empty", "advertisement-v2.bin")
	store := openCanonicalStore(t, "empty")

	var buf bytes.Buffer
	w := pktline.NewWriter(&buf)
	require.NoError(t, writeV2Advertisement(w, store, Options{}))

	got := maskV2Advertisement(buf.Bytes())
	wantMasked := maskV2Advertisement(want)

	if !bytes.Equal(got, wantMasked) {
		t.Fatalf("v2 advertisement mismatch for `empty`:\n got: %q\nwant: %q",
			got, wantMasked)
	}
}

// TestCanonical_AdvertisementV2_matrix extends the advertisement
// assertion to the rest of the curated fixture matrix. The
// advertisement is the same shape regardless of fixture contents,
// so the bodies are identical; the cases exercise the harness
// against fixtures the empty case does not (loose, packed,
// reftable-backed).
func TestCanonical_AdvertisementV2_matrix(t *testing.T) {
	for _, fixture := range []string{"loose-only", "packed-only", "with-reftable-content"} {
		t.Run(fixture, func(t *testing.T) {
			want := readCanonical(t, fixture, "advertisement-v2.bin")
			store := openCanonicalStore(t, fixture)

			var buf bytes.Buffer
			w := pktline.NewWriter(&buf)
			require.NoError(t, writeV2Advertisement(w, store, Options{}))

			got := maskV2Advertisement(buf.Bytes())
			wantMasked := maskV2Advertisement(want)

			if !bytes.Equal(got, wantMasked) {
				t.Fatalf("v2 advertisement mismatch for %s:\n got: %q\nwant: %q",
					fixture, got, wantMasked)
			}
		})
	}
}

// TestCanonical_LSRefs_matrix pins the v2 ls-refs response bytes
// against canonical Git for each non-trivial fixture. The request
// bytes are replayed from `testdata/canonical/<fixture>/ls-refs.req`
// — the exact bytes canonical was fed when the response was
// captured — so the test exercises the same code path on both
// sides.
//
// The ls-refs response body has no agent involvement, but the
// stream still flows through maskAgent for defensive idempotence:
// any future ERR-pkt or session-id-like cap on the response side
// is normalised by the same machinery as the advertisement test.
func TestCanonical_LSRefs_matrix(t *testing.T) {
	for _, fixture := range []string{"empty", "loose-only", "packed-only", "with-reftable-content"} {
		t.Run(fixture, func(t *testing.T) {
			req := readCanonical(t, fixture, "ls-refs.req")
			want := readCanonical(t, fixture, "ls-refs.bin")

			store := openCanonicalStore(t, fixture)

			var out bytes.Buffer
			r := pktline.NewReader(bytes.NewReader(req))
			w := pktline.NewWriter(&out)
			err := runV2CommandLoop(context.Background(), r, w, store, Options{})
			require.NoError(t, err)

			got := maskAgent(out.Bytes())
			wantMasked := maskAgent(want)

			if !bytes.Equal(got, wantMasked) {
				t.Fatalf("ls-refs response mismatch for %s:\n got: %q\nwant: %q",
					fixture, got, wantMasked)
			}
		})
	}
}

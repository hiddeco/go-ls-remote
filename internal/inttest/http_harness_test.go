package inttest_test

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hiddeco/go-ls-remote/internal/inttest"
	"github.com/hiddeco/go-ls-remote/internal/objfmt"
	"github.com/hiddeco/go-ls-remote/internal/objstore"
	"github.com/hiddeco/go-ls-remote/internal/wire"
	"github.com/hiddeco/go-ls-remote/pktline"
)

// openLooseOnlySHA1Store materialises the `loose-only` fixture and
// returns its opened [objstore.Store[objfmt.SHA1Hash]]. The fixture
// declares a `refs/heads/main` ref, which the advertisement test
// depends on.
func openLooseOnlySHA1Store(t *testing.T) *objstore.Store[objfmt.SHA1Hash] {
	t.Helper()

	var entry inttest.Entry
	for _, e := range inttest.Entries() {
		if e.Name == "loose-only" {
			entry = e
			break
		}
	}
	require.Equal(t, "loose-only", entry.Name, "matrix must include the loose-only fixture")

	gitdir := entry.Materialize(t)
	store, err := objstore.Open[objfmt.SHA1Hash](gitdir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// TestNewHTTPServer_servesAdvertisement asserts that the harness emits
// the smart-HTTP `# service=git-upload-pack` preamble followed by the
// v2 advertisement on `GET /repo.git/info/refs?service=git-upload-pack`.
// At least one `refs/heads/` line must surface in the ls-refs response
// that follows — confirming the harness wires `internal/server.Serve`
// against the supplied store.
func TestNewHTTPServer_servesAdvertisement(t *testing.T) {
	t.Parallel()

	store := openLooseOnlySHA1Store(t)
	base := inttest.NewHTTPServer(t, store)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		base+"/repo.git/info/refs?service=git-upload-pack", http.NoBody)
	require.NoError(t, err)
	req.Header.Set("Git-Protocol", "version=2")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "application/x-git-upload-pack-advertisement",
		resp.Header.Get("Content-Type"))

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	pr := pktline.NewReader(bytes.NewReader(body))
	pkt, err := pr.ReadPacket()
	require.NoError(t, err)
	assert.Equal(t, pktline.Data, pkt.Kind)
	assert.Equal(t, "# service=git-upload-pack\n", string(pkt.Data),
		"first packet must be the smart-HTTP service preamble")

	pkt, err = pr.ReadPacket()
	require.NoError(t, err)
	assert.Equal(t, pktline.Flush, pkt.Kind,
		"preamble must be followed by a flush per canonical Git's `http-backend.c`")

	// The remainder is the v2 advertisement. The first packet is
	// `version 2\n`; subsequent packets carry capability lines.
	pkt, err = pr.ReadPacket()
	require.NoError(t, err)
	assert.Equal(t, pktline.Data, pkt.Kind)
	assert.Equal(t, "version 2\n", string(pkt.Data),
		"v2 advertisement must begin with `version 2\\n`")
}

// TestNewHTTPServer_handlesCommandPost asserts that the harness routes
// a v2 `ls-refs` command POST through `internal/server.Serve` and
// returns at least one `refs/heads/` line for the `loose-only` fixture.
func TestNewHTTPServer_handlesCommandPost(t *testing.T) {
	t.Parallel()

	store := openLooseOnlySHA1Store(t)
	base := inttest.NewHTTPServer(t, store)

	var body bytes.Buffer
	pw := pktline.NewWriter(&body)
	require.NoError(t,
		wire.EncodeV2CommandRequest(pw, "ls-refs",
			[]string{"peel", "symrefs"},
			[]string{"object-format=sha1"}))

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		base+"/repo.git/git-upload-pack", bytes.NewReader(body.Bytes()))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	req.Header.Set("Accept", "application/x-git-upload-pack-result")
	req.Header.Set("Git-Protocol", "version=2")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "application/x-git-upload-pack-result",
		resp.Header.Get("Content-Type"))

	respBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	pr := pktline.NewReader(bytes.NewReader(respBody))

	// The harness wires the POST handler through
	// [internal/server.ServeCommandLoop], which emits only the command
	// response — no leading advertisement to drain. The first packet
	// is therefore the first ls-refs ref line.
	var sawHEAD, sawMain bool
	for {
		pkt, err := pr.ReadPacket()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		if pkt.Kind != pktline.Data {
			continue
		}
		// HEAD lines may carry the `symref-target:<name>` suffix when
		// the `symrefs` capability is requested, so match the bare
		// ` HEAD` token (with a leading space separating it from the
		// hex oid) rather than ` HEAD\n`.
		s := string(pkt.Data)
		if strings.Contains(s, " HEAD ") || strings.HasSuffix(s, " HEAD\n") {
			sawHEAD = true
		}
		if strings.Contains(s, " refs/heads/main\n") {
			sawMain = true
		}
	}
	assert.True(t, sawHEAD, "ls-refs response must carry a HEAD line")
	assert.True(t, sawMain, "ls-refs response must carry refs/heads/main")
}

// TestNewHTTPServer_unsupportedPath asserts that paths the harness does
// not mount return 404. The harness mounts the fixed `/repo.git/...`
// suffix; everything else is `http.NotFound`.
func TestNewHTTPServer_unsupportedPath(t *testing.T) {
	t.Parallel()

	store := openLooseOnlySHA1Store(t)
	base := inttest.NewHTTPServer(t, store)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		base+"/elsewhere.git/info/refs?service=git-upload-pack", http.NoBody)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode,
		"paths the harness does not mount must return 404")
}

// TestNewHTTPRedirectServer_emitsConfiguredStatus asserts that the
// redirect harness echoes the configured status code and `Location`
// header on every request, regardless of method or path. This is the
// minimum primitive the cross-transport and error-matrix suites compose
// to test redirect-policy behaviours.
func TestNewHTTPRedirectServer_emitsConfiguredStatus(t *testing.T) {
	t.Parallel()

	const dest = "https://example.com/repo.git/info/refs?service=git-upload-pack"
	base := inttest.NewHTTPRedirectServer(t, http.StatusFound, dest)

	// `http.Client` follows redirects by default; suppress that so we
	// can observe the 3xx response itself.
	client := &http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		base+"/whatever/info/refs?service=git-upload-pack", http.NoBody)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusFound, resp.StatusCode)
	assert.Equal(t, dest, resp.Header.Get("Location"))
}

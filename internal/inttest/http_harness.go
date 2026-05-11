package inttest

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
	"github.com/hiddeco/go-ls-remote/internal/objstore"
	"github.com/hiddeco/go-ls-remote/internal/server"
	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/transport"
)

// Smart-HTTP content types as named by canonical Git's
// `http-backend.c::get_info_refs` and `service_rpc`. The harness emits
// them on the responses it serves; clients hitting the harness with
// `transport/http` use the same constants.
const (
	httpSmartAdvContentType  = "application/x-git-upload-pack-advertisement"
	httpSmartRespContentType = "application/x-git-upload-pack-result"
)

// httpRepoMount is the fixed URL prefix [NewHTTPServer] mounts. A single
// hard-coded value keeps the harness's API tight; tests that need a
// different mount point can compose [NewHTTPRedirectServer] in front of
// the harness or build their own `httptest.Server` from
// [internal/server.Serve] directly.
const httpRepoMount = "/repo.git"

// NewHTTPServer returns the base URL of an in-process smart-HTTP
// server that mounts store at `/repo.git`. The server handles the
// canonical upload-pack endpoints — `GET /repo.git/info/refs?service=git-upload-pack`
// for the smart advertisement and `POST /repo.git/git-upload-pack` for
// the v2 command loop — by delegating to [internal/server.Serve].
//
// The harness diverges from a `(url, cleanup)` return shape because t
// is already a [testing.TB]: the [httptest.Server] is closed via
// [testing.TB.Cleanup], so callers neither have to remember a `defer`
// nor risk a double-close. The single string return is also harder to
// misuse than a tuple with a partially-consumed cleanup.
//
// The generic parameter H is inferred from store; cross-fixture
// callers that iterate [Entries] type-switch on [Entry.ObjectFormat]
// once and call this function with the matching concrete store.
//
// # Wire shape
//
// The advertisement handler writes the `# service=git-upload-pack\n`
// preamble pkt-line and a flush before invoking
// [internal/server.Serve] with a feed of `0000` — the empty client
// request that drives Serve through its advertisement-only code path.
// Canonical Git's `http-backend.c::get_info_refs` does the same:
// preamble, flush, then the upload-pack advertisement bytes verbatim.
//
// The command handler reads the request body and hands it to
// [internal/server.ServeCommandLoop], which runs the v2 command-request
// loop without re-emitting the leading advertisement. The split
// mirrors canonical Git's `http-backend.c::service_rpc`: the POST
// response carries only the command response, while the GET probe
// owns the advertisement.
func NewHTTPServer[H objfmt.Hash](t testing.TB, store *objstore.Store[H]) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(httpHandler(t, store)))
	t.Cleanup(srv.Close)
	return srv.URL
}

// httpHandler routes the two endpoints the harness exposes. Anything
// else returns 404 so tests can pin negative-path behaviour against a
// well-known status.
func httpHandler[H objfmt.Hash](t testing.TB, store *objstore.Store[H]) http.HandlerFunc {
	t.Helper()
	infoRefs := httpRepoMount + "/info/refs"
	uploadPack := httpRepoMount + "/git-upload-pack"
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == infoRefs:
			w.Header().Set("Content-Type", httpSmartAdvContentType)
			pw := pktline.NewWriter(w)
			require.NoError(t, pw.WritePacket([]byte("# service=git-upload-pack\n")))
			require.NoError(t, pw.WriteFlush())
			require.NoError(t, server.Serve(r.Context(),
				pktline.NewReader(bytes.NewReader([]byte("0000"))),
				pw, store, server.Options{
					PreferredProtocol: transport.ProtocolV2,
				}))
		case r.Method == http.MethodPost && r.URL.Path == uploadPack:
			body, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			w.Header().Set("Content-Type", httpSmartRespContentType)
			require.NoError(t, server.ServeCommandLoop(r.Context(),
				pktline.NewReader(bytes.NewReader(body)),
				pktline.NewWriter(w), store, server.Options{
					PreferredProtocol: transport.ProtocolV2,
				}))
		default:
			http.NotFound(w, r)
		}
	}
}

// NewHTTPRedirectServer returns the base URL of an in-process server
// that responds with status and a `Location: location` header for
// every request, regardless of method or path. It is the minimal
// primitive for redirect-policy tests: callers chain it in front of a
// [NewHTTPServer] instance to exercise follow-on hops, cross-origin
// strip behaviour, or rejection paths without committing to any one
// scenario in this layer.
//
// status should be a 3xx code; the harness writes the header
// regardless so callers can also exercise the "non-3xx with Location"
// shape if they need to. The server is registered for cleanup via
// [testing.TB.Cleanup].
func NewHTTPRedirectServer(t testing.TB, status int, location string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", location)
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

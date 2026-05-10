package lsremote

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
	"github.com/hiddeco/go-ls-remote/internal/objstore"
	"github.com/hiddeco/go-ls-remote/internal/server"
	"github.com/hiddeco/go-ls-remote/internal/testfixture"
	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/trace"
	"github.com/hiddeco/go-ls-remote/transport"
)

// smartAdvHeader is the content type a real smart-HTTP server replies
// with on the discovery probe — `application/x-git-upload-pack-advertisement`.
// The HTTP transport keys its smart/dumb dispatch off this value.
const smartAdvHeader = "application/x-git-upload-pack-advertisement"

// commandResultHeader is the content type a real smart-HTTP server
// replies with on the v2 command POST — `application/x-git-upload-pack-result`.
const commandResultHeader = "application/x-git-upload-pack-result"

// openFixtureStore materialises the named fixture from `testdata/repos/`
// and returns an opened `[objstore.Store]` over it. The
// `objects/pack/` directory is created on demand because some ref-only
// fixtures ship without one — mirroring `transport/http/command_test.go`.
func openFixtureStore(t *testing.T, name string) *objstore.Store[objfmt.SHA1Hash] {
	t.Helper()
	gitdir := testfixture.MaterializeRepo(t, name)
	require.NoError(t, os.MkdirAll(filepath.Join(gitdir, "objects", "pack"), 0o755))
	store, err := objstore.Open[objfmt.SHA1Hash](gitdir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// serveHandlerV2 returns an `http.Handler` that emulates a v2-speaking
// smart-HTTP server backed by the materialised fixture store. The shape
// mirrors `transport/http/command_test.go::serveHandler`: a GET on
// `<repoPath>/info/refs` returns the `# service=git-upload-pack`
// preamble plus the v2 advertisement, and a POST on
// `<repoPath>/git-upload-pack` runs the v2 command loop.
func serveHandlerV2(t *testing.T, store *objstore.Store[objfmt.SHA1Hash], repoPath string) http.Handler {
	t.Helper()
	infoRefsPath := repoPath + "/info/refs"
	uploadPackPath := repoPath + "/git-upload-pack"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == infoRefsPath:
			w.Header().Set("Content-Type", smartAdvHeader)
			pw := pktline.NewWriter(w)
			require.NoError(t, pw.WritePacket([]byte("# service=git-upload-pack\n")))
			require.NoError(t, pw.WriteFlush())
			err := server.Serve(r.Context(),
				pktline.NewReader(bytes.NewReader([]byte("0000"))),
				pw, store, server.Options{
					Agent:             "test-server/0.0",
					PreferredProtocol: transport.ProtocolV2,
				})
			require.NoError(t, err)
		case r.Method == http.MethodPost && r.URL.Path == uploadPackPath:
			body, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			w.Header().Set("Content-Type", commandResultHeader)
			// `server.Serve` always runs the advertise-then-loop flow,
			// but a real `upload-pack` POST emits the command response
			// alone. Drive `Serve` into a buffer, drop everything up to
			// (and including) the advertisement's trailing flush, then
			// stream the remainder to the HTTP body so the Session-layer
			// decoder sees the canonical wire shape.
			var sink bytes.Buffer
			require.NoError(t, server.Serve(r.Context(),
				pktline.NewReader(bytes.NewReader(body)),
				pktline.NewWriter(&sink), store,
				server.Options{
					Agent:             "test-server/0.0",
					PreferredProtocol: transport.ProtocolV2,
				}))
			_, err = w.Write(stripV2Advertisement(t, sink.Bytes()))
			require.NoError(t, err)
		default:
			http.NotFound(w, r)
		}
	})
}

// serveHandlerV0 returns an `http.Handler` that emulates a v0 server.
// Only the discovery probe is implemented; v0 has no command loop, so
// the POST endpoint is unreachable from the Dial flow.
func serveHandlerV0(t *testing.T, store *objstore.Store[objfmt.SHA1Hash], repoPath string) http.Handler {
	t.Helper()
	infoRefsPath := repoPath + "/info/refs"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == infoRefsPath {
			w.Header().Set("Content-Type", smartAdvHeader)
			pw := pktline.NewWriter(w)
			require.NoError(t, pw.WritePacket([]byte("# service=git-upload-pack\n")))
			require.NoError(t, pw.WriteFlush())
			require.NoError(t, server.Serve(r.Context(),
				pktline.NewReader(bytes.NewReader([]byte("0000"))),
				pw, store, server.Options{
					Agent:             "test-server/0.0",
					PreferredProtocol: transport.ProtocolV0,
				}))
			return
		}
		http.NotFound(w, r)
	})
}

// recordingTracerEvents is a `trace.Tracer` that records every event
// for later assertions; we only need pointer identity for the
// option-passthrough test, but a real Tracer makes the intent clear.
type recordingTracerEvents struct {
	events []trace.Event
}

func (r *recordingTracerEvents) OnEvent(e trace.Event) { r.events = append(r.events, e) }

// captureTransport is a stub `transport.Transport` that records the
// `transport.OpenOptions` it was called with so a test can verify the
// dial-config plumbing forwards `tracer`, `userAgent` and `protocol`
// verbatim. It returns `conn`/`err` on Open without doing any I/O.
type captureTransport struct {
	schemes []string
	gotOpts transport.OpenOptions
	gotURL  *transport.URL
	conn    transport.Conn
	err     error
}

func (c *captureTransport) Schemes() []string { return c.schemes }

func (c *captureTransport) Open(_ context.Context, u *transport.URL, opts transport.OpenOptions) (transport.Conn, error) {
	c.gotOpts = opts
	c.gotURL = u
	return c.conn, c.err
}

// stubConn satisfies `transport.Conn` with a caller-supplied
// `pktline.Reader` for its advertisement. `Command` and `Close` are
// inert no-ops so a test can keep a returned `*Session` alive across
// assertions without worrying about resource cleanup.
type stubConn struct {
	adv     *pktline.Reader
	closed  bool
	closeFn func() error
}

func (s *stubConn) Advertisement() *pktline.Reader { return s.adv }
func (s *stubConn) Command(context.Context, string, []string, []string) (*pktline.Reader, error) {
	return nil, errors.New("stubConn: Command not implemented")
}
func (s *stubConn) Close() error {
	s.closed = true
	if s.closeFn != nil {
		return s.closeFn()
	}
	return nil
}

// stripV2Advertisement drops everything up to and including the v2
// advertisement's terminating flush from raw, returning the suffix
// (the command response and its own trailing framing). The in-process
// `server.Serve` always emits the advertisement before running the
// command loop, but a real upload-pack POST emits only the command
// response — strip the advertisement so the Session-layer decoder
// sees the canonical wire shape.
//
// The implementation reads pkt-lines until it consumes the
// advertisement's flush and then returns whatever bytes the underlying
// reader has not yet consumed. The leading `version 2\n` data packet,
// the capability lines, and the flush all belong to the advertisement.
func stripV2Advertisement(t *testing.T, raw []byte) []byte {
	t.Helper()
	src := bytes.NewReader(raw)
	rdr := pktline.NewReader(src)
	for {
		pkt, err := rdr.ReadPacket()
		require.NoError(t, err, "advertisement read while stripping")
		if pkt.Kind == pktline.Flush {
			break
		}
	}
	// `src` advances byte-for-byte under `pktline.Reader.ReadPacket`,
	// so the remaining bytes are the post-advertisement portion of the
	// stream.
	rest, err := io.ReadAll(src)
	require.NoError(t, err)
	return rest
}

// buildV2Advertisement returns a byte slice carrying a minimal v2
// capability advertisement so a `stubConn` can hand a positioned
// `pktline.Reader` back to `ParseAdvertisement`. The shape mirrors the
// emitter in `internal/server/advertise.go`:
//
//	pkt:  "version 2\n"
//	pkt:  "agent=test/0\n"
//	pkt:  "object-format=sha1\n"
//	pkt:  "ls-refs=unborn\n"
//	pkt:  "fetch\n"
//	pkt:  "object-info\n"
//	flush
func buildV2Advertisement(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	pw := pktline.NewWriter(&b)
	for _, line := range []string{
		"version 2\n",
		"agent=test/0\n",
		"object-format=sha1\n",
		"ls-refs=unborn\n",
		"fetch\n",
		"object-info\n",
	} {
		require.NoError(t, pw.WritePacket([]byte(line)))
	}
	require.NoError(t, pw.WriteFlush())
	return b.Bytes()
}

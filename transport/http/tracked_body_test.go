package httpt

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hiddeco/go-ls-remote/pktline"
)

// TestConn_inflight_deregistersOnEOF pins that a fully-drained
// command response body removes itself from the Conn's in-flight
// set, so a long-lived Conn that issues many commands does not
// accumulate map entries.
func TestConn_inflight_deregistersOnEOF(t *testing.T) {
	t.Parallel()
	store := openFixtureStore(t, "loose-only")

	srv := httptest.NewServer(serveHandler(t, store, "/repo.git"))
	defer srv.Close()

	c := openSmartTestConn(t, srv, "/repo.git")

	const N = 16
	for range N {
		rdr, err := c.Command(t.Context(), "ls-refs",
			cmdBody("ls-refs", nil, []string{"object-format=sha1"}))
		require.NoError(t, err)

		// Drain to EOF — the wrapper must deregister itself.
		for {
			_, err := rdr.ReadPacket()
			if errors.Is(err, io.EOF) {
				break
			}
			require.NoError(t, err)
		}
	}

	c.inflightMu.Lock()
	defer c.inflightMu.Unlock()
	assert.Empty(t, c.inflight,
		"every drained command must deregister; got %d residual entries", len(c.inflight))
}

// TestConn_inflight_abandonedReaderRecoveredByClose pins that a
// reader the caller drops without draining is still recovered by
// Conn.Close. The deregister hook must not regress that path.
func TestConn_inflight_abandonedReaderRecoveredByClose(t *testing.T) {
	t.Parallel()
	var bodyClosed bool
	rt := &countingRoundTripper{respond: func(_ *http.Request, _ int) *http.Response {
		h := http.Header{}
		h.Set("Content-Type", commandAcceptType)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     h,
			Body: &closeNotifier{
				// Large body the caller will abandon without reading.
				Reader: bytes.NewReader(bytes.Repeat([]byte("0"), 4096)),
				onClose: func() {
					bodyClosed = true
				},
			},
		}
	}}

	c := &Conn{
		body:              &closeCounter{Reader: bytes.NewReader(nil)},
		reader:            pktline.NewReader(bytes.NewReader(nil)),
		client:            &http.Client{Transport: rt},
		url:               mustParseURL(t, "https://example.com/repo.git/info/refs"),
		userAgent:         "test/1",
		gitProtocolHeader: "version=2",
	}

	_, err := c.Command(t.Context(), "ls-refs", cmdBody("ls-refs", nil, nil))
	require.NoError(t, err)

	c.inflightMu.Lock()
	beforeClose := len(c.inflight)
	c.inflightMu.Unlock()
	assert.Equal(t, 1, beforeClose,
		"a command response that has not been drained must still be tracked")

	require.NoError(t, c.Close())

	c.inflightMu.Lock()
	defer c.inflightMu.Unlock()
	assert.Empty(t, c.inflight,
		"Close must drain every still-tracked body, leaving the set empty")
	assert.True(t, bodyClosed,
		"Close must close the underlying body of every tracked wrapper")
}

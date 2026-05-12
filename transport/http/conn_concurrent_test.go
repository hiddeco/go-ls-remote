package httpt

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hiddeco/go-ls-remote/pktline"
)

// TestConn_Command_ConcurrentInFlight pins the multi-flight contract on
// [Conn.Command]: a single [Conn] supports multiple commands in flight
// at once, each backed by its own HTTP POST. Two readers obtained
// without draining either must both yield correct payloads.
func TestConn_Command_ConcurrentInFlight(t *testing.T) {
	t.Parallel()
	store := openFixtureStore(t, "loose-only")

	srv := httptest.NewServer(serveHandler(t, store, "/repo.git"))
	defer srv.Close()

	c := openSmartTestConn(t, srv, "/repo.git")

	rdr1, err := c.Command(context.Background(), "ls-refs",
		cmdBody("ls-refs", nil, []string{"object-format=sha1"}))
	require.NoError(t, err)
	require.NotNil(t, rdr1)

	rdr2, err := c.Command(context.Background(), "ls-refs",
		cmdBody("ls-refs", nil, []string{"object-format=sha1"}))
	require.NoError(t, err)
	require.NotNil(t, rdr2)

	pkts1 := readAllPackets(t, rdr1)
	pkts2 := readAllPackets(t, rdr2)
	require.NotEmpty(t, pkts1, "first reader must yield ls-refs packets")
	require.NotEmpty(t, pkts2, "second reader must yield ls-refs packets")

	// Both responses describe the same fixture; the canonical-Git
	// shape carries a `HEAD\n` line and one `refs/heads/main\n` line.
	// Verifying both streams independently confirms neither was
	// truncated by the other's lifecycle.
	hasShape := func(pkts []pktline.Packet) (head, main bool) {
		for _, p := range pkts {
			if p.Kind != pktline.Data {
				continue
			}
			s := string(p.Data)
			if strings.Contains(s, " HEAD\n") {
				head = true
			}
			if strings.Contains(s, " refs/heads/main\n") {
				main = true
			}
		}
		return
	}
	h1, m1 := hasShape(pkts1)
	h2, m2 := hasShape(pkts2)
	assert.True(t, h1 && m1, "first reader missing canonical shape; got %q", pkts1)
	assert.True(t, h2 && m2, "second reader missing canonical shape; got %q", pkts2)
}

// TestConn_Command_ConcurrentGoroutines drives 8 goroutines through a
// shared [Conn], asserting no race detector trip and that every goroutine
// observes a well-formed ls-refs response. Each goroutine drains its own
// reader; with the multi-flight contract no caller needs external
// synchronisation against the others.
func TestConn_Command_ConcurrentGoroutines(t *testing.T) {
	t.Parallel()
	store := openFixtureStore(t, "loose-only")

	srv := httptest.NewServer(serveHandler(t, store, "/repo.git"))
	defer srv.Close()

	c := openSmartTestConn(t, srv, "/repo.git")

	const goroutines = 8
	const iterations = 4

	var wg sync.WaitGroup
	for range goroutines {
		wg.Go(func() {
			for range iterations {
				rdr, err := c.Command(context.Background(), "ls-refs",
					cmdBody("ls-refs", nil, []string{"object-format=sha1"}))
				assert.NoError(t, err)
				if rdr == nil {
					return
				}
				// Drain through ReadPacket so the underlying response
				// body is consumed and released. Failing to drain would
				// leak a body entry into the Conn's tracking set until
				// Close, which is allowed by the contract but is not
				// what well-behaved callers do.
				var sawData bool
				for {
					p, err := rdr.ReadPacket()
					if errors.Is(err, io.EOF) {
						break
					}
					assert.NoError(t, err)
					if p.Kind == pktline.Data {
						sawData = true
					}
				}
				assert.True(t, sawData, "ls-refs response must contain data packets")
			}
		})
	}
	wg.Wait()
}

// TestConn_Close_DrainsAbandonedBody pins the lifecycle of a command
// reader that the caller never drains: [Conn.Close] must release the
// underlying HTTP body so the connection returns to the pool.
//
// The test wires a counting body so we can assert the close path runs
// exactly once and discards the remaining bytes; the underlying
// connection is otherwise indistinguishable from a leaked one.
func TestConn_Close_DrainsAbandonedInflightBody(t *testing.T) {
	t.Parallel()
	var bodyClosed atomic.Int32

	rt := &countingRoundTripper{respond: func(_ *http.Request, _ int) *http.Response {
		h := http.Header{}
		h.Set("Content-Type", commandAcceptType)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     h,
			Body: &closeNotifier{
				Reader: strings.NewReader("0000"),
				onClose: func() {
					bodyClosed.Add(1)
				},
			},
		}
	}}

	c := &Conn{
		body:              &closeCounter{Reader: strings.NewReader("")},
		reader:            pktline.NewReader(strings.NewReader("")),
		client:            &http.Client{Transport: rt},
		url:               mustParseURL(t, "https://example.com/repo.git/info/refs"),
		userAgent:         "test/1",
		gitProtocolHeader: "version=2",
	}

	// Issue a command, abandon the returned reader.
	_, err := c.Command(context.Background(), "ls-refs", cmdBody("ls-refs", nil, nil))
	require.NoError(t, err)

	require.NoError(t, c.Close())
	assert.Equal(t, int32(1), bodyClosed.Load(),
		"Close must release the abandoned command body exactly once")
}

// closeNotifier is an [io.ReadCloser] that invokes onClose on Close.
// Used by the abandoned-body test to confirm [Conn.Close] reaches the
// tracked in-flight body.
type closeNotifier struct {
	io.Reader
	onClose func()
}

func (c *closeNotifier) Close() error {
	if c.onClose != nil {
		c.onClose()
	}
	return nil
}

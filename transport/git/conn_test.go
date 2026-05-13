package gitt

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/transport"
)

// startEchoListener starts a TCP listener on 127.0.0.1, accepts one
// connection in a goroutine, and calls handle with the accepted [net.Conn].
// The listener is closed via [testing.T.Cleanup] when the test ends.
func startEchoListener(t *testing.T, handle func(net.Conn)) (host, port string) {
	t.Helper()

	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		handle(c)
	}()
	h, p, err := net.SplitHostPort(ln.Addr().String())
	require.NoError(t, err)
	return h, p
}

// TestConn_Close_Idempotent verifies the [transport.Conn] idempotent
// contract: a second [Conn.Close] is a no-op returning nil even after
// the underlying TCP connection has been closed.
func TestConn_Close_Idempotent(t *testing.T) {
	t.Parallel()

	host, port := startEchoListener(t, func(c net.Conn) {
		// Accept and discard; the client side is what we are testing.
		defer func() { _ = c.Close() }()
		buf := make([]byte, 512)
		for {
			if _, err := c.Read(buf); err != nil {
				return
			}
		}
	})

	tr := New(WithDialer(&net.Dialer{}))
	u := &transport.URL{
		Scheme: "git",
		Host:   host,
		Port:   port,
		Path:   "/repo",
		Raw:    "git://" + host + ":" + port + "/repo",
	}
	conn, err := tr.Open(t.Context(), u, transport.OpenOptions{})
	require.NoError(t, err)

	assert.NoError(t, conn.Close(), "first Close must return nil")
	assert.NoError(t, conn.Close(), "second Close is a no-op returning nil")
	assert.NoError(t, conn.Close(), "third Close still nil; idempotency is total")
}

// TestConn_Advertisement_ReturnsCachedReader verifies that
// [Conn.Advertisement] returns the cached pkt-line reader and that it
// streams a packet written by the server before the client reads.
func TestConn_Advertisement_ReturnsCachedReader(t *testing.T) {
	t.Parallel()

	const wantPayload = "version 2\n"

	host, port := startEchoListener(t, func(c net.Conn) {
		defer func() { _ = c.Close() }()

		// Drain the initial request pkt-line the client sends on connect.
		r := pktline.NewReader(c)
		_, _ = r.ReadPacket()

		// Write one data packet back so Advertisement().ReadPacket()
		// has something to return.
		w := pktline.NewWriter(c)
		_ = w.WritePacket([]byte(wantPayload))
	})

	tr := New(WithDialer(&net.Dialer{}))
	u := &transport.URL{
		Scheme: "git",
		Host:   host,
		Port:   port,
		Path:   "/repo",
		Raw:    "git://" + host + ":" + port + "/repo",
	}
	conn, err := tr.Open(t.Context(), u, transport.OpenOptions{})
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	pkt, err := conn.Advertisement().ReadPacket()
	require.NoError(t, err)
	assert.Equal(t, wantPayload, string(pkt.Data))
}

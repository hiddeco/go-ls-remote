package gitt

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/transport"
)

// TestTransport_Open_DialsAndSendsInitialRequest verifies the happy
// path: [Transport.Open] dials the listener and emits the initial
// git-daemon pkt-line whose payload matches the wire grammar from
// `gitprotocol-pack.adoc §"Extra Parameters"` and
// `connect.c::git_connect_git` (lines 1288-1298) in canonical Git.
//
// With a nil [transport.OpenOptions.PreferredProtocol] the transport
// auto-negotiates v2, so the request carries a `version=2` trailer.
func TestTransport_Open_DialsAndSendsInitialRequest(t *testing.T) {
	payloadCh := make(chan []byte, 1)

	host, port := startEchoListener(t, func(c net.Conn) {
		defer c.Close()
		r := pktline.NewReader(c)
		pkt, err := r.ReadPacket()
		if err == nil {
			// Clone Data: the buffer may be reused on the next read.
			cp := make([]byte, len(pkt.Data))
			copy(cp, pkt.Data)
			payloadCh <- cp
		} else {
			payloadCh <- nil
		}
		// Keep the connection alive until the client closes it.
		buf := make([]byte, 1)
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
	conn, err := tr.Open(context.Background(), u, transport.OpenOptions{})
	require.NoError(t, err)
	defer conn.Close()

	want := fmt.Sprintf("git-upload-pack /repo\x00host=%s:%s\x00\x00version=2\x00", host, port)
	gotPayload := <-payloadCh
	assert.Equal(t, want, string(gotPayload))
}

// TestTransport_Open_DefaultPort verifies that a URL with an empty
// Port resolves to port 9418 (the well-known git-daemon port). Since
// [Transport] stores a `*net.Dialer`, address capture is done by
// calling [hostAddress] directly — a white-box check of the same
// helper [Transport.Open] uses.
func TestTransport_Open_DefaultPort(t *testing.T) {
	u := &transport.URL{
		Scheme: "git",
		Host:   "example.com",
		Port:   "",
		Path:   "/repo",
		Raw:    "git://example.com/repo",
	}
	assert.Equal(t, "example.com:9418", hostAddress(u))
}

// TestTransport_Open_PortFromURL verifies that a URL with an explicit
// Port passes that port through to the dial address unchanged.
func TestTransport_Open_PortFromURL(t *testing.T) {
	u := &transport.URL{
		Scheme: "git",
		Host:   "example.com",
		Port:   "12345",
		Path:   "/repo",
		Raw:    "git://example.com:12345/repo",
	}
	assert.Equal(t, "example.com:12345", hostAddress(u))
}

// TestHostAddress is a table-driven white-box test for [hostAddress].
// It covers hostname, IPv4, and IPv6 inputs with and without an explicit
// port. The key invariant: IPv6 literals must be bracketed when a port
// is appended (`::1:9418` is not a valid dial address; `[::1]:9418` is).
// The [transport.URL] contract is that `Host` is always unbracketed.
func TestHostAddress(t *testing.T) {
	cases := []struct {
		name string
		host string
		port string
		want string
	}{
		{name: "hostname_no_port", host: "example.com", port: "", want: "example.com:9418"},
		{name: "hostname_with_port", host: "example.com", port: "12345", want: "example.com:12345"},
		{name: "ipv4_no_port", host: "127.0.0.1", port: "", want: "127.0.0.1:9418"},
		{name: "ipv4_with_port", host: "127.0.0.1", port: "12345", want: "127.0.0.1:12345"},
		{name: "ipv6_no_port", host: "::1", port: "", want: "[::1]:9418"},
		{name: "ipv6_with_port", host: "::1", port: "12345", want: "[::1]:12345"},
		{name: "ipv6_full_no_port", host: "2001:db8::1", port: "", want: "[2001:db8::1]:9418"},
		{name: "ipv6_full_with_port", host: "2001:db8::1", port: "12345", want: "[2001:db8::1]:12345"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := &transport.URL{Host: tc.host, Port: tc.port}
			assert.Equal(t, tc.want, hostAddress(u))
		})
	}
}

// TestTransport_Open_PinV1Rejected verifies that requesting protocol v1
// returns an error matching [ErrUnsupportedProtocol] with Op=="dial"
// without invoking the dialer. The listener below never receives a
// connection if v1 rejection fires before the dial, keeping
// `dialCalled` false.
func TestTransport_Open_PinV1Rejected(t *testing.T) {
	var dialCalled atomic.Bool

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		dialCalled.Store(true)
		c.Close()
	}()
	h, p, err := net.SplitHostPort(ln.Addr().String())
	require.NoError(t, err)

	v1 := transport.ProtocolV1
	tr := New(WithDialer(&net.Dialer{}))
	u := &transport.URL{
		Scheme: "git",
		Host:   h,
		Port:   p,
		Path:   "/repo",
		Raw:    "git://" + h + ":" + p + "/repo",
	}
	_, openErr := tr.Open(context.Background(), u, transport.OpenOptions{
		PreferredProtocol: &v1,
	})

	require.Error(t, openErr)
	assert.True(t, errors.Is(openErr, ErrUnsupportedProtocol),
		"err must match ErrUnsupportedProtocol; got %v", openErr)

	var pe *ProtocolError
	require.True(t, errors.As(openErr, &pe))
	assert.Equal(t, "dial", pe.Op)
	assert.False(t, dialCalled.Load(), "dialer must not be invoked when v1 is pinned")
}

// TestTransport_Open_DialError verifies that a TCP dial failure surfaces
// as a `*ProtocolError{Op: "dial"}` wrapping the raw network error, and
// does not match [ErrUnsupportedProtocol]. The mapping is tested via
// [mapDialError] directly to avoid OS-specific "connection refused" vs
// "no route to host" variability.
func TestTransport_Open_DialError(t *testing.T) {
	dialErr := errors.New("connection refused")

	err := mapDialError(dialErr, "git://example.com/repo")
	require.Error(t, err)

	var pe *ProtocolError
	require.True(t, errors.As(err, &pe), "dial error must wrap *ProtocolError")
	assert.Equal(t, "dial", pe.Op)
	assert.False(t, errors.Is(err, ErrUnsupportedProtocol),
		"dial error must not match ErrUnsupportedProtocol")
	assert.True(t, errors.Is(err, dialErr),
		"dial error must wrap the original network error")
}

// TestTransport_Open_ContextCanceled verifies that a pre-cancelled
// context returns [context.Canceled] directly, not wrapped in
// `*ProtocolError`.
func TestTransport_Open_ContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tr := New()
	u := &transport.URL{
		Scheme: "git",
		Host:   "example.com",
		Port:   "9418",
		Path:   "/repo",
		Raw:    "git://example.com/repo",
	}
	_, err := tr.Open(ctx, u, transport.OpenOptions{})
	assert.ErrorIs(t, err, context.Canceled)
}

// failingConn wraps a [net.Conn] and overrides Write to return a
// deterministic error. It is used by [TestTransport_Open_WriteFailureClosesConn]
// to verify that [Transport.Open] closes the connection when
// `wire.WriteStreamRequest` fails. The `closed` channel is closed by
// [Close] so the test can assert the connection was cleaned up.
//
// The seam is injected via the package-private `dialFn` field on
// [Transport]; it is never exposed in the public API.
type failingConn struct {
	net.Conn
	closed chan struct{}
}

func (f *failingConn) Write(_ []byte) (int, error) {
	return 0, errors.New("write rejected")
}

func (f *failingConn) Close() error {
	err := f.Conn.Close()
	select {
	case <-f.closed:
	default:
		close(f.closed)
	}
	return err
}

// TestTransport_Open_WriteFailureClosesConn verifies that when
// `wire.WriteStreamRequest` fails after a successful TCP dial, [Open]
// closes the dialed [net.Conn] before returning an error — exercising
// the `_ = netConn.Close()` cleanup guard in [Open].
//
// The test injects a fake connection (via the package-private `dialFn`
// seam) whose `Write` always returns an error. The seam is intentionally
// not part of the public API; it exists solely so this path can be
// covered without a flaky network-timing dependency.
func TestTransport_Open_WriteFailureClosesConn(t *testing.T) {
	closed := make(chan struct{})

	// net.Pipe gives two synchronised ends; we only need the client side.
	client, server := net.Pipe()
	t.Cleanup(func() { _ = server.Close() })

	fc := &failingConn{Conn: client, closed: closed}

	tr := New()
	// Inject via package-private seam: dialFn overrides the TCP dial.
	tr.dialFn = func(_ context.Context, _, _ string) (net.Conn, error) {
		return fc, nil
	}

	u := &transport.URL{
		Scheme: "git",
		Host:   "example.com",
		Port:   "9418",
		Path:   "/repo",
		Raw:    "git://example.com/repo",
	}
	_, openErr := tr.Open(context.Background(), u, transport.OpenOptions{})

	require.Error(t, openErr, "Open must fail when the write fails")

	var pe *ProtocolError
	require.True(t, errors.As(openErr, &pe),
		"error must be *ProtocolError; got %T: %v", openErr, openErr)
	assert.Equal(t, "dial", pe.Op,
		"Op must be \"dial\" for a write failure during open")
	assert.NotNil(t, pe.Err,
		"ProtocolError.Err must carry the write failure cause")
	assert.Contains(t, pe.Err.Error(), "write initial pkt-line",
		"wrapped cause must identify the write step")

	// Assert the connection was closed after the write failure.
	select {
	case <-closed:
		// expected
	default:
		t.Fatal("Open did not close the connection after write failure")
	}
}

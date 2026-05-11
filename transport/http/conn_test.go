package httpt

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hiddeco/go-ls-remote/pktline"
)

// closeCounter is an [io.ReadCloser] that counts Close calls. It is
// used to assert the connection's idempotent-close contract without
// touching network state.
type closeCounter struct {
	io.Reader
	closes int
}

func (c *closeCounter) Close() error {
	c.closes++
	return nil
}

// mustParseURL parses raw and fails the test on error. Used in
// hand-built [Conn] fixtures where the URL is a constant string.
func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	return u
}

func TestConn_Advertisement_ReturnsCachedReader(t *testing.T) {
	body := &closeCounter{Reader: bytes.NewReader(nil)}
	rdr := pktline.NewReader(body)

	c := &Conn{body: body, reader: rdr, url: mustParseURL(t, "https://example.com/repo.git")}

	assert.Same(t, rdr, c.Advertisement(),
		"Advertisement returns the reader the probe positioned past the preamble")
}

func TestConn_Close_Idempotent(t *testing.T) {
	body := &closeCounter{Reader: strings.NewReader("leftover bytes")}
	rdr := pktline.NewReader(body)

	c := &Conn{body: body, reader: rdr, url: mustParseURL(t, "https://example.com/repo.git")}

	require.NoError(t, c.Close(), "first Close must not error")
	require.NoError(t, c.Close(), "second Close must be a no-op")
	require.NoError(t, c.Close(), "third Close must remain a no-op")

	assert.Equal(t, 1, body.closes,
		"Close must close the underlying body exactly once")
}

func TestConn_Command_DumbReturnsUnsupportedProtocol(t *testing.T) {
	c := &Conn{dumb: true}
	rdr, err := c.Command(context.Background(), "ls-refs", cmdBody("ls-refs", nil, nil))
	assert.Nil(t, rdr)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrUnsupportedProtocol),
		"a dumb Conn must short-circuit Command to ErrUnsupportedProtocol; got %v", err)
}

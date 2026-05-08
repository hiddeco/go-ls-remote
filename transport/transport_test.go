package transport

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/trace"
)

// fakeTransport is a minimal [Transport] implementation used as a test
// fixture across this and other transport-package tests.
type fakeTransport struct {
	schemes []string
}

func (f fakeTransport) Schemes() []string { return f.schemes }

func (fakeTransport) Open(context.Context, *URL, OpenOptions) (Conn, error) {
	return nil, nil
}

// fakeConn is a minimal [Conn] implementation used as a test fixture.
type fakeConn struct{}

func (fakeConn) Advertisement() *pktline.Reader { return nil }

func (fakeConn) Command(context.Context, string, []string, []string) (*pktline.Reader, error) {
	return nil, nil
}

func (fakeConn) Close() error { return nil }

func TestTransport_interfaceCompiles(t *testing.T) {
	var _ Transport = fakeTransport{}
}

func TestConn_interfaceCompiles(t *testing.T) {
	var _ Conn = fakeConn{}
}

func TestOpenOptions_zeroValue(t *testing.T) {
	var o OpenOptions
	assert.Nil(t, o.Tracer)
	assert.Empty(t, o.UserAgent)
	assert.Equal(t, ProtocolV0, o.PreferredProtocol,
		"zero value of PreferredProtocol is ProtocolV0 (the int zero); callers asking for the spec default should set ProtocolAuto explicitly")
	// Sanity: the Tracer field can hold any [trace.Tracer], including
	// nil and concrete implementations.
	o.Tracer = trace.NewWriterTracer(nil)
	assert.NotNil(t, o.Tracer)
}

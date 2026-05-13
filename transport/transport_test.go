package transport

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/trace"
)

// fakeTransport is a minimal [Transport] implementation used as a test
// fixture across this and other transport-package tests. The id field
// distinguishes otherwise-equivalent instances in tests that need to
// observe replacement semantics.
type fakeTransport struct {
	id      string
	schemes []string
}

func (f fakeTransport) Schemes() []string { return f.schemes }

func (fakeTransport) Open(context.Context, *URL, OpenOptions) (Conn, error) {
	return nil, nil
}

// fakeConn is a minimal [Conn] implementation used as a test fixture.
type fakeConn struct{}

func (fakeConn) Advertisement() *pktline.Reader { return nil }

func (fakeConn) Command(context.Context, string, CommandBody) (*pktline.Reader, error) {
	return nil, nil
}

func (fakeConn) Close() error { return nil }

func TestTransport_interfaceCompiles(t *testing.T) {
	t.Parallel()

	var _ Transport = fakeTransport{}
}

func TestConn_interfaceCompiles(t *testing.T) {
	t.Parallel()

	var _ Conn = fakeConn{}
}

// TestOpenOptions_zeroValue pins the zero-value contract: an
// uninitialised [OpenOptions] is "no preference" all the way through.
// PreferredProtocol is a pointer specifically so that "no preference"
// is the zero value, not a sentinel that can be confused with a real
// version (which would have made the integer zero mean v0).
func TestOpenOptions_zeroValue(t *testing.T) {
	t.Parallel()

	var o OpenOptions
	assert.Nil(t, o.Tracer)
	assert.Empty(t, o.UserAgent)
	assert.Nil(t, o.PreferredProtocol,
		"zero value of PreferredProtocol must be nil = auto-negotiate, the spec default")

	// Pin to a concrete version via address-of.
	v := ProtocolV2
	o.PreferredProtocol = &v
	require.NotNil(t, o.PreferredProtocol)
	assert.Equal(t, ProtocolV2, *o.PreferredProtocol)

	// Sanity: the Tracer field can hold any [trace.Tracer], including
	// nil and concrete implementations.
	o.Tracer = trace.NewWriterTracer(nil)
	assert.NotNil(t, o.Tracer)
}

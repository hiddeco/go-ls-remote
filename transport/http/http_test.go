package httpt

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hiddeco/go-ls-remote/transport"
)

// Compile-time guarantee that *Transport satisfies the transport
// contract even while Open is a stub. The runtime check below covers
// Schemes; this line covers method-set conformance.
var _ transport.Transport = (*Transport)(nil)

func TestTransport_Schemes(t *testing.T) {
	tr := New()
	require.NotNil(t, tr)

	assert.Equal(t, []string{"https", "http"}, tr.Schemes(),
		"HTTP transport claims https first then http")
}

func TestTransport_implementsTransportInterface(t *testing.T) {
	// Satisfies the interface dynamically too: assigning to the
	// interface-typed local will fail to compile if the method set
	// drifts.
	var iface transport.Transport = New()
	_ = iface.Schemes()
}

func TestTransport_Open_stub(t *testing.T) {
	tr := New()

	u, err := transport.ParseURL("https://example.com/repo.git")
	require.NoError(t, err)

	conn, err := tr.Open(context.Background(), u, transport.OpenOptions{})
	assert.Nil(t, conn, "stub Open returns no connection")
	require.Error(t, err, "stub Open returns a placeholder error")
	assert.Contains(t, err.Error(), "not implemented",
		"placeholder error must say 'not implemented' so callers see why")
}

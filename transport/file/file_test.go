package filet

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hiddeco/go-ls-remote/transport"
)

// Compile-time guarantee that *Transport satisfies the transport
// contract. The runtime check below covers Schemes; this line covers
// method-set conformance.
var _ transport.Transport = (*Transport)(nil)

func TestTransport_Schemes(t *testing.T) {
	t.Parallel()

	tr := New()
	require.NotNil(t, tr)

	assert.Equal(t, []string{"file"}, tr.Schemes(),
		"file transport claims the file scheme")
}

func TestTransport_implementsTransportInterface(t *testing.T) {
	t.Parallel()

	// Satisfies the interface dynamically too: assigning to the
	// interface-typed local will fail to compile if the method set
	// drifts.
	var iface transport.Transport = New()
	_ = iface.Schemes()
}

func TestNew_nilOptionSkipped(t *testing.T) {
	t.Parallel()

	// Pinning the documented contract: nil entries in opts are
	// silently skipped so callers can pass conditionally built options
	// without guarding each one. No concrete options exist yet — the
	// extension slot is what we exercise here.
	tr := New(nil, nil)
	require.NotNil(t, tr, "New must not panic on nil options")

	assert.Equal(t, []string{"file"}, tr.Schemes())
}

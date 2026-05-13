package lsremote

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hiddeco/go-ls-remote/trace"
	"github.com/hiddeco/go-ls-remote/transport"
)

// recordingTracer is a no-op [trace.Tracer] used only to verify that
// [WithTracer] stores the exact value it was handed.
type recordingTracer struct{}

func (recordingTracer) OnPacketEvent(*trace.PacketEvent) {}
func (recordingTracer) OnEvent(trace.Event)              {}

func TestDialConfigZeroValue(t *testing.T) {
	t.Parallel()
	var c dialConfig
	assert.Nil(t, c.registry,
		"zero-value registry is nil so Dial can fall back to defaults")
	assert.Nil(t, c.tracer,
		"zero-value tracer is nil so emission sites short-circuit")
	assert.Empty(t, c.userAgent,
		"zero-value userAgent is empty so the transport default applies")
	assert.Nil(t, c.protocol,
		"zero-value protocol is nil so negotiation is automatic")
}

func TestWithTransports(t *testing.T) {
	t.Parallel()
	reg := transport.NewRegistry()

	var c dialConfig
	WithTransports(reg).applyDial(&c)

	assert.Same(t, reg, c.registry,
		"WithTransports records the supplied Registry pointer verbatim")
}

func TestWithTransports_Nil(t *testing.T) {
	t.Parallel()
	var c dialConfig
	WithTransports(nil).applyDial(&c)

	assert.Nil(t, c.registry,
		"WithTransports(nil) leaves registry nil so Dial-time defaulting kicks in")
}

func TestWithTracer(t *testing.T) {
	t.Parallel()
	tr := recordingTracer{}

	var c dialConfig
	WithTracer(tr).applyDial(&c)

	assert.Equal(t, tr, c.tracer,
		"WithTracer records the supplied Tracer value")
}

func TestWithTracer_Nil(t *testing.T) {
	t.Parallel()
	var c dialConfig
	WithTracer(nil).applyDial(&c)

	assert.Nil(t, c.tracer,
		"WithTracer(nil) leaves the tracer nil so tracing stays disabled")
}

func TestWithUserAgent(t *testing.T) {
	t.Parallel()
	var c dialConfig
	WithUserAgent("foo/1.0").applyDial(&c)

	assert.Equal(t, "foo/1.0", c.userAgent)
}

func TestWithUserAgent_Empty(t *testing.T) {
	t.Parallel()
	var c dialConfig
	WithUserAgent("").applyDial(&c)

	assert.Empty(t, c.userAgent,
		"WithUserAgent(\"\") leaves the field empty so transport default applies")
}

func TestWithProtocol(t *testing.T) {
	t.Parallel()
	var c dialConfig
	WithProtocol(ProtocolV2).applyDial(&c)

	require.NotNil(t, c.protocol,
		"WithProtocol stores a non-nil pointer so callers can distinguish "+
			"\"pin to v0\" from \"auto-negotiate\"")
	assert.Equal(t, ProtocolV2, *c.protocol)
}

func TestWithProtocol_V0(t *testing.T) {
	t.Parallel()
	// Verify that pinning ProtocolV0 (the zero value of the integer
	// type) round-trips through the pointer correctly — the whole reason
	// dialConfig.protocol is a pointer rather than a plain value.
	var c dialConfig
	WithProtocol(ProtocolV0).applyDial(&c)

	require.NotNil(t, c.protocol)
	assert.Equal(t, ProtocolV0, *c.protocol)
}

func TestOptionComposition(t *testing.T) {
	t.Parallel()
	// Apply every option to a single config and confirm each took
	// effect — the realistic call shape inside Dial.
	reg := transport.NewRegistry()
	tr := recordingTracer{}

	var c dialConfig
	opts := []Option{
		WithTransports(reg),
		WithTracer(tr),
		WithUserAgent("ua/2.0"),
		WithProtocol(ProtocolV2),
	}
	for _, o := range opts {
		o.applyDial(&c)
	}

	assert.Same(t, reg, c.registry)
	assert.Equal(t, tr, c.tracer)
	assert.Equal(t, "ua/2.0", c.userAgent)
	require.NotNil(t, c.protocol)
	assert.Equal(t, ProtocolV2, *c.protocol)
}

func TestWithProtocol_IndependentPointers(t *testing.T) {
	t.Parallel()
	// Two separate WithProtocol calls must produce independent storage
	// — re-applying WithProtocol to a fresh config must not see writes
	// to a prior config's pointer.
	var a, b dialConfig
	WithProtocol(ProtocolV2).applyDial(&a)
	WithProtocol(ProtocolV0).applyDial(&b)

	require.NotNil(t, a.protocol)
	require.NotNil(t, b.protocol)
	assert.Equal(t, ProtocolV2, *a.protocol)
	assert.Equal(t, ProtocolV0, *b.protocol)
}

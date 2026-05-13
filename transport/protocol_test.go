package transport

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestProtocolVersion pins the wire-integer mapping. Each defined value
// must equal the integer that appears in `version N\n` on the wire so
// encoders/decoders can convert with `int(v)` rather than a switch.
func TestProtocolVersion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		v    ProtocolVersion
		want int
	}{
		{"v0", ProtocolV0, 0},
		{"v1", ProtocolV1, 1},
		{"v2", ProtocolV2, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, int(tt.v))
		})
	}
}

func TestProtocolVersion_String(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		v    ProtocolVersion
		want string
	}{
		{"v0", ProtocolV0, "v0"},
		{"v1", ProtocolV1, "v1"},
		{"v2", ProtocolV2, "v2"},
		{"out-of-range positive", ProtocolVersion(99), "unknown(99)"},
		{"out-of-range negative", ProtocolVersion(-1), "unknown(-1)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, tt.v.String())
		})
	}
}

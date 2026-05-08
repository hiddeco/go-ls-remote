package transport

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestProtocolVersion(t *testing.T) {
	tests := []struct {
		name string
		v    ProtocolVersion
		want int
	}{
		{"auto", ProtocolAuto, -1},
		{"v0", ProtocolV0, 0},
		{"v1", ProtocolV1, 1},
		{"v2", ProtocolV2, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, int(tt.v))
		})
	}
}

func TestProtocolVersion_String(t *testing.T) {
	tests := []struct {
		name string
		v    ProtocolVersion
		want string
	}{
		{"auto", ProtocolAuto, "auto"},
		{"v0", ProtocolV0, "v0"},
		{"v1", ProtocolV1, "v1"},
		{"v2", ProtocolV2, "v2"},
		{"out-of-range positive", ProtocolVersion(99), "unknown(99)"},
		{"out-of-range negative", ProtocolVersion(-2), "unknown(-2)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.v.String())
		})
	}
}

package pktline

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestKind(t *testing.T) {
	tests := []struct {
		name string
		k    Kind
		want uint8
	}{
		{"data", Data, 0},
		{"flush", Flush, 1},
		{"delim", Delim, 2},
		{"response-end", ResponseEnd, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, uint8(tt.k))
		})
	}
}

// TestMaxPayload pins the constant against canonical Git's
// LARGE_PACKET_MAX (65520) minus the 4-byte length prefix.
func TestMaxPayload(t *testing.T) {
	assert.Equal(t, 65516, MaxPayload)
}

func TestPacket(t *testing.T) {
	t.Run("zero value is empty Data packet", func(t *testing.T) {
		var p Packet
		assert.Equal(t, Data, p.Kind)
		assert.Nil(t, p.Data)
	})
}

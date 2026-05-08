package pktline

import (
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReader_ReadPacket(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantKind Kind
		wantData string
		wantErr  error
	}{
		{
			name:     "data packet with payload",
			input:    "0007hi\n",
			wantKind: Data,
			wantData: "hi\n",
		},
		{
			name:     "empty data packet",
			input:    "0004",
			wantKind: Data,
			wantData: "",
		},
		{
			name:    "empty stream returns EOF",
			input:   "",
			wantErr: io.EOF,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewReader(strings.NewReader(tt.input))
			pkt, err := r.ReadPacket()
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantKind, pkt.Kind)
			assert.Equal(t, tt.wantData, string(pkt.Data))
		})
	}
}

// TestReader_ReadPacket_consecutive verifies that two back-to-back
// reads return their respective payloads. Note that the second
// ReadPacket invalidates the first packet's Data slice (buffer
// aliasing); this test only inspects each packet before the next read.
func TestReader_ReadPacket_consecutive(t *testing.T) {
	r := NewReader(strings.NewReader("0007hi\n0008foo\n"))

	p1, err := r.ReadPacket()
	require.NoError(t, err)
	assert.Equal(t, Data, p1.Kind)
	assert.Equal(t, "hi\n", string(p1.Data))

	p2, err := r.ReadPacket()
	require.NoError(t, err)
	assert.Equal(t, Data, p2.Kind)
	assert.Equal(t, "foo\n", string(p2.Data))
}

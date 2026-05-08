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
		{
			name:     "flush packet",
			input:    "0000",
			wantKind: Flush,
			wantData: "",
		},
		{
			name:     "delim packet",
			input:    "0001",
			wantKind: Delim,
			wantData: "",
		},
		{
			name:     "response-end packet",
			input:    "0002",
			wantKind: ResponseEnd,
			wantData: "",
		},
		{
			name:    "truncated length prefix",
			input:   "000",
			wantErr: io.ErrUnexpectedEOF,
		},
		{
			name:    "truncated payload (none follows)",
			input:   "0008",
			wantErr: io.ErrUnexpectedEOF,
		},
		{
			name:    "truncated payload (partial)",
			input:   "0008x",
			wantErr: io.ErrUnexpectedEOF,
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

// TestReader_ReadPacket_consecutive verifies that back-to-back reads
// return their respective payloads, including across mixed
// data/control kinds. Note that each ReadPacket invalidates the
// previous packet's Data slice (buffer aliasing); this test only
// inspects each packet before the next read.
func TestReader_ReadPacket_consecutive(t *testing.T) {
	t.Run("data then data", func(t *testing.T) {
		r := NewReader(strings.NewReader("0007hi\n0008foo\n"))

		p1, err := r.ReadPacket()
		require.NoError(t, err)
		assert.Equal(t, Data, p1.Kind)
		assert.Equal(t, "hi\n", string(p1.Data))

		p2, err := r.ReadPacket()
		require.NoError(t, err)
		assert.Equal(t, Data, p2.Kind)
		assert.Equal(t, "foo\n", string(p2.Data))
	})

	t.Run("delim then data", func(t *testing.T) {
		r := NewReader(strings.NewReader("00010007hi\n"))

		p1, err := r.ReadPacket()
		require.NoError(t, err)
		assert.Equal(t, Delim, p1.Kind)
		assert.Nil(t, p1.Data)

		p2, err := r.ReadPacket()
		require.NoError(t, err)
		assert.Equal(t, Data, p2.Kind)
		assert.Equal(t, "hi\n", string(p2.Data))
	})
}

// TestReader_ReadPacket_invalidLength verifies that length prefix
// values which do not denote a valid packet are rejected with a
// non-nil error. 0000, 0001, and 0002 are reserved for control
// packets; 0003 is reserved per canonical Git's `pkt-line.c`.
func TestReader_ReadPacket_invalidLength(t *testing.T) {
	r := NewReader(strings.NewReader("0003"))
	_, err := r.ReadPacket()
	require.Error(t, err)
	assert.ErrorContains(t, err, "0003")
}

// TestReader_ReadPacket_invalidHex verifies that a non-hex length
// prefix is rejected. The canonical implementation in `pkt-line.c`
// also rejects non-hex on read.
func TestReader_ReadPacket_invalidHex(t *testing.T) {
	r := NewReader(strings.NewReader("zzzzhello"))
	_, err := r.ReadPacket()
	require.Error(t, err)
	assert.ErrorContains(t, err, "hex")
}

// TestReader_ReadPacket_maxPayload exercises the largest payload size
// the format permits, exactly [MaxPayload] bytes. The on-wire length
// prefix is `fff0` (= 65520 = MaxPayload + 4).
func TestReader_ReadPacket_maxPayload(t *testing.T) {
	payload := strings.Repeat("a", MaxPayload)
	r := NewReader(strings.NewReader("fff0" + payload))

	pkt, err := r.ReadPacket()
	require.NoError(t, err)
	assert.Equal(t, Data, pkt.Kind)
	assert.Equal(t, MaxPayload, len(pkt.Data))
	assert.Equal(t, payload, string(pkt.Data))
}

// TestReader_ReadPacket_bufferAliasingContract pins the documented
// behaviour that [Packet.Data] aliases the [Reader]'s internal buffer
// and is invalidated by the next [Reader.ReadPacket] call.
//
// Two packets with the same payload size — so the buffer is reused
// without growth — let us observe in-place overwrite: the slice
// retained from the first packet, after a second read, points at the
// same memory now holding the second packet's bytes.
func TestReader_ReadPacket_bufferAliasingContract(t *testing.T) {
	r := NewReader(strings.NewReader("0007hi\n0007by\n"))

	p1, err := r.ReadPacket()
	require.NoError(t, err)
	keep := p1.Data // intentionally retain across the next call

	_, err = r.ReadPacket()
	require.NoError(t, err)

	assert.Equal(t, "by\n", string(keep),
		"buffer was not reused in place; aliasing contract requires reuse for same-size payloads")
}

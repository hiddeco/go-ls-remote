package pktline

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriter_WritePacket(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
		want    string
	}{
		{
			name:    "empty payload",
			payload: nil,
			want:    "0004",
		},
		{
			name:    "small payload",
			payload: []byte("hi\n"),
			want:    "0007hi\n",
		},
		{
			name:    "ASCII payload",
			payload: []byte("command=ls-refs\n"),
			want:    "0014command=ls-refs\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			w := NewWriter(&buf)
			require.NoError(t, w.WritePacket(tt.payload))
			assert.Equal(t, tt.want, buf.String())
		})
	}
}

func TestWriter_controlPackets(t *testing.T) {
	tests := []struct {
		name  string
		write func(*Writer) error
		want  string
	}{
		{"flush", (*Writer).WriteFlush, "0000"},
		{"delim", (*Writer).WriteDelim, "0001"},
		{"response-end", (*Writer).WriteResponseEnd, "0002"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			w := NewWriter(&buf)
			require.NoError(t, tt.write(w))
			assert.Equal(t, tt.want, buf.String())
		})
	}
}

// TestWriter_WritePacket_maxPayload exercises the largest payload size
// the format permits, exactly [MaxPayload] bytes. The on-wire length
// prefix is `fff0` (= MaxPayload + 4).
func TestWriter_WritePacket_maxPayload(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)

	require.NoError(t, w.WritePacket(bytes.Repeat([]byte{'a'}, MaxPayload)))
	assert.Equal(t, 4+MaxPayload, buf.Len())
	assert.True(t, bytes.HasPrefix(buf.Bytes(), []byte("fff0")),
		"length prefix = %q, want fff0", buf.Bytes()[:4])
}

// TestWriter_WritePacket_overflow verifies that payloads exceeding
// [MaxPayload] are rejected before any byte is written.
func TestWriter_WritePacket_overflow(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)

	err := w.WritePacket(bytes.Repeat([]byte{'a'}, MaxPayload+1))
	require.Error(t, err)
	assert.ErrorContains(t, err, "MaxPayload")
	assert.Zero(t, buf.Len(), "no bytes should be written when payload is over the limit")
}

// TestWriter_roundTrip sends a small request through [NewWriter] and
// reads it back through [NewReader], exercising both codecs together.
func TestWriter_roundTrip(t *testing.T) {
	inputs := []string{
		"command=ls-refs\n",
		"agent=test/1.0\n",
		"ref-prefix refs/tags/\n",
	}

	var buf bytes.Buffer
	w := NewWriter(&buf)
	for _, p := range inputs {
		require.NoError(t, w.WritePacket([]byte(p)))
	}
	require.NoError(t, w.WriteFlush())

	r := NewReader(strings.NewReader(buf.String()))
	for _, want := range inputs {
		pkt, err := r.ReadPacket()
		require.NoError(t, err)
		assert.Equal(t, Data, pkt.Kind)
		assert.Equal(t, want, string(pkt.Data))
	}
	pkt, err := r.ReadPacket()
	require.NoError(t, err)
	assert.Equal(t, Flush, pkt.Kind)
}

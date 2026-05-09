package wire

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/transport"
)

// buildAdvertisement encodes the supplied packets into pkt-line framing
// and returns a [pktline.Reader] over the result. Each entry is either a
// data payload (`packet{data: ...}`) or a control packet (`packet{kind:
// ...}`); a zero-valued kind on a non-nil data slice is a normal data
// packet.
type packet struct {
	data []byte
	kind pktline.Kind
}

func buildAdvertisement(t *testing.T, pkts ...packet) *pktline.Reader {
	t.Helper()
	var buf bytes.Buffer
	w := pktline.NewWriter(&buf)
	for _, p := range pkts {
		switch p.kind {
		case pktline.Flush:
			require.NoError(t, w.WriteFlush())
		case pktline.Delim:
			require.NoError(t, w.WriteDelim())
		case pktline.ResponseEnd:
			require.NoError(t, w.WriteResponseEnd())
		default:
			require.NoError(t, w.WritePacket(p.data))
		}
	}
	return pktline.NewReader(&buf)
}

func TestParseAdvertisement(t *testing.T) {
	v0 := transport.ProtocolV0
	v1 := transport.ProtocolV1
	v2 := transport.ProtocolV2

	t.Run("v2 happy path collects caps until flush", func(t *testing.T) {
		r := buildAdvertisement(t,
			packet{data: []byte("version 2\n")},
			packet{data: []byte("agent=git/2.45.0\n")},
			packet{data: []byte("object-format=sha256\n")},
			packet{data: []byte("ls-refs=unborn\n")},
			packet{kind: pktline.Flush},
		)
		ad, err := ParseAdvertisement(r, nil)
		require.NoError(t, err)
		assert.Equal(t, transport.ProtocolV2, ad.Version)
		assert.Nil(t, ad.Refs)
		require.Len(t, ad.Caps, 3)
		assert.Equal(t, RawCapability{Name: "agent", Value: "git/2.45.0"}, ad.Caps[0])
		assert.Equal(t, RawCapability{Name: "object-format", Value: "sha256"}, ad.Caps[1])
		assert.Equal(t, RawCapability{Name: "ls-refs", Value: "unborn"}, ad.Caps[2])
	})

	t.Run("v0 flush only yields empty advertisement", func(t *testing.T) {
		r := buildAdvertisement(t, packet{kind: pktline.Flush})
		ad, err := ParseAdvertisement(r, nil)
		require.NoError(t, err)
		assert.Equal(t, transport.ProtocolV0, ad.Version)
		assert.Nil(t, ad.Caps)
		assert.Nil(t, ad.Refs)
	})

	t.Run("v0 data packet is the first ref line", func(t *testing.T) {
		r := buildAdvertisement(t,
			packet{data: []byte("0123456789abcdef0123456789abcdef01234567 HEAD\x00agent=git\n")},
			packet{kind: pktline.Flush},
		)
		ad, err := ParseAdvertisement(r, nil)
		require.NoError(t, err)
		assert.Equal(t, transport.ProtocolV0, ad.Version)
	})

	t.Run("v1 line then data is v1", func(t *testing.T) {
		r := buildAdvertisement(t,
			packet{data: []byte("version 1\n")},
			packet{data: []byte("0123456789abcdef0123456789abcdef01234567 HEAD\x00agent=git\n")},
			packet{kind: pktline.Flush},
		)
		ad, err := ParseAdvertisement(r, nil)
		require.NoError(t, err)
		assert.Equal(t, transport.ProtocolV1, ad.Version)
	})

	t.Run("explicit version 0 is rejected", func(t *testing.T) {
		r := buildAdvertisement(t,
			packet{data: []byte("version 0\n")},
			packet{kind: pktline.Flush},
		)
		_, err := ParseAdvertisement(r, nil)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrUnsupportedProtocol),
			"expected ErrUnsupportedProtocol, got %v", err)
	})

	t.Run("unknown version digit is rejected", func(t *testing.T) {
		r := buildAdvertisement(t,
			packet{data: []byte("version 7\n")},
			packet{kind: pktline.Flush},
		)
		_, err := ParseAdvertisement(r, nil)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrUnsupportedProtocol),
			"expected ErrUnsupportedProtocol, got %v", err)
	})

	t.Run("unknown version text is rejected", func(t *testing.T) {
		r := buildAdvertisement(t,
			packet{data: []byte("version foo\n")},
			packet{kind: pktline.Flush},
		)
		_, err := ParseAdvertisement(r, nil)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrUnsupportedProtocol),
			"expected ErrUnsupportedProtocol, got %v", err)
	})

	t.Run("truncated input surfaces EOF", func(t *testing.T) {
		r := pktline.NewReader(bytes.NewReader(nil))
		_, err := ParseAdvertisement(r, nil)
		require.Error(t, err)
		assert.True(t,
			errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF),
			"expected EOF or ErrUnexpectedEOF, got %v", err)
	})

	t.Run("want v2 matches server v2", func(t *testing.T) {
		r := buildAdvertisement(t,
			packet{data: []byte("version 2\n")},
			packet{kind: pktline.Flush},
		)
		ad, err := ParseAdvertisement(r, &v2)
		require.NoError(t, err)
		assert.Equal(t, transport.ProtocolV2, ad.Version)
	})

	t.Run("want v2 mismatches server v0", func(t *testing.T) {
		r := buildAdvertisement(t, packet{kind: pktline.Flush})
		_, err := ParseAdvertisement(r, &v2)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrUnsupportedProtocol),
			"expected ErrUnsupportedProtocol, got %v", err)
	})

	t.Run("want v0 matches server v0", func(t *testing.T) {
		r := buildAdvertisement(t, packet{kind: pktline.Flush})
		ad, err := ParseAdvertisement(r, &v0)
		require.NoError(t, err)
		assert.Equal(t, transport.ProtocolV0, ad.Version)
	})

	t.Run("want v1 mismatches server v2", func(t *testing.T) {
		r := buildAdvertisement(t,
			packet{data: []byte("version 2\n")},
			packet{kind: pktline.Flush},
		)
		_, err := ParseAdvertisement(r, &v1)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrUnsupportedProtocol),
			"expected ErrUnsupportedProtocol, got %v", err)
	})

	t.Run("nil want accepts any negotiated version", func(t *testing.T) {
		// Repeats the v2 happy path with nil want for explicit clarity.
		r := buildAdvertisement(t,
			packet{data: []byte("version 2\n")},
			packet{kind: pktline.Flush},
		)
		ad, err := ParseAdvertisement(r, nil)
		require.NoError(t, err)
		assert.Equal(t, transport.ProtocolV2, ad.Version)
	})
}

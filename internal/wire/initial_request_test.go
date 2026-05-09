package wire

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/transport"
)

func TestHTTPProtocolHeader(t *testing.T) {
	v0 := transport.ProtocolV0
	v1 := transport.ProtocolV1
	v2 := transport.ProtocolV2

	cases := []struct {
		name string
		v    *transport.ProtocolVersion
		want string
	}{
		{"nil auto-negotiates to v2", nil, "version=2"},
		{"v0 pinned", &v0, "version=0"},
		{"v1 pinned", &v1, "version=1"},
		{"v2 pinned", &v2, "version=2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, HTTPProtocolHeader(tc.v))
		})
	}
}

func TestWriteStreamRequest(t *testing.T) {
	v0 := transport.ProtocolV0
	v1 := transport.ProtocolV1
	v2 := transport.ProtocolV2

	cases := []struct {
		name    string
		url     *transport.URL
		v       *transport.ProtocolVersion
		payload []byte
	}{
		{
			name:    "nil version auto-negotiates to v2",
			url:     &transport.URL{Scheme: "git", Host: "example.com", Path: "/repo.git"},
			v:       nil,
			payload: []byte("git-upload-pack /repo.git\x00host=example.com\x00\x00version=2\x00"),
		},
		{
			name:    "v0 omits version trailer",
			url:     &transport.URL{Scheme: "git", Host: "example.com", Path: "/repo.git"},
			v:       &v0,
			payload: []byte("git-upload-pack /repo.git\x00host=example.com\x00"),
		},
		{
			name:    "v1 emits version=1 trailer",
			url:     &transport.URL{Scheme: "git", Host: "example.com", Path: "/repo.git"},
			v:       &v1,
			payload: []byte("git-upload-pack /repo.git\x00host=example.com\x00\x00version=1\x00"),
		},
		{
			name:    "v2 emits version=2 trailer",
			url:     &transport.URL{Scheme: "git", Host: "example.com", Path: "/repo.git"},
			v:       &v2,
			payload: []byte("git-upload-pack /repo.git\x00host=example.com\x00\x00version=2\x00"),
		},
		{
			name:    "non-default port appends colon-port to host parameter",
			url:     &transport.URL{Scheme: "git", Host: "example.com", Port: "9418", Path: "/repo.git"},
			v:       &v2,
			payload: []byte("git-upload-pack /repo.git\x00host=example.com:9418\x00\x00version=2\x00"),
		},
		{
			name:    "IPv6 host with port uses bracketed authority",
			url:     &transport.URL{Scheme: "git", Host: "fe80::1", Port: "9418", Path: "/repo.git"},
			v:       &v2,
			payload: []byte("git-upload-pack /repo.git\x00host=[fe80::1]:9418\x00\x00version=2\x00"),
		},
		{
			name:    "IPv6 host without port emits bare literal",
			url:     &transport.URL{Scheme: "git", Host: "fe80::1", Path: "/repo.git"},
			v:       &v2,
			payload: []byte("git-upload-pack /repo.git\x00host=fe80::1\x00\x00version=2\x00"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			w := pktline.NewWriter(&buf)
			require.NoError(t, WriteStreamRequest(w, tc.url, tc.v))

			want := append(pktLengthPrefix(t, len(tc.payload)+4), tc.payload...)
			assert.Equal(t, want, buf.Bytes())
		})
	}
}

// pktLengthPrefix returns the four-character lowercase hex length prefix
// used by the pkt-line framing for a packet of total length n bytes
// (prefix included). The hand-rolled formatter mirrors `pkt-line.c`'s
// `format_packet`.
func pktLengthPrefix(t *testing.T, n int) []byte {
	t.Helper()
	const hex = "0123456789abcdef"
	return []byte{
		hex[(n>>12)&0xf],
		hex[(n>>8)&0xf],
		hex[(n>>4)&0xf],
		hex[n&0xf],
	}
}

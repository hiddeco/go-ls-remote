package wire

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hiddeco/go-ls-remote/pktline"
)

func TestEncodeObjectInfo(t *testing.T) {
	const (
		oidA = "1111111111111111111111111111111111111111"
		oidB = "2222222222222222222222222222222222222222"
		oidC = "3333333333333333333333333333333333333333"
	)

	cases := []struct {
		name      string
		oids      []string
		args      ObjectInfoArgs
		caps      RawCapabilities
		userAgent string
		wantBody  func(t *testing.T) []byte
	}{
		{
			name: "single OID, size off",
			oids: []string{oidA},
			args: ObjectInfoArgs{},
			caps: nil,
			wantBody: func(t *testing.T) []byte {
				var b bytes.Buffer
				b.Write(pkt(t, "command=object-info\n"))
				b.WriteString("0001")
				b.Write(pkt(t, "oid "+oidA+"\n"))
				b.WriteString("0000")
				return b.Bytes()
			},
		},
		{
			name: "single OID, size on",
			oids: []string{oidA},
			args: ObjectInfoArgs{Size: true},
			caps: nil,
			wantBody: func(t *testing.T) []byte {
				var b bytes.Buffer
				b.Write(pkt(t, "command=object-info\n"))
				b.WriteString("0001")
				b.Write(pkt(t, "size\n"))
				b.Write(pkt(t, "oid "+oidA+"\n"))
				b.WriteString("0000")
				return b.Bytes()
			},
		},
		{
			name: "multiple OIDs, size off",
			oids: []string{oidA, oidB, oidC},
			args: ObjectInfoArgs{},
			caps: nil,
			wantBody: func(t *testing.T) []byte {
				var b bytes.Buffer
				b.Write(pkt(t, "command=object-info\n"))
				b.WriteString("0001")
				b.Write(pkt(t, "oid "+oidA+"\n"))
				b.Write(pkt(t, "oid "+oidB+"\n"))
				b.Write(pkt(t, "oid "+oidC+"\n"))
				b.WriteString("0000")
				return b.Bytes()
			},
		},
		{
			name: "multiple OIDs, size on",
			oids: []string{oidA, oidB, oidC},
			args: ObjectInfoArgs{Size: true},
			caps: nil,
			wantBody: func(t *testing.T) []byte {
				var b bytes.Buffer
				b.Write(pkt(t, "command=object-info\n"))
				b.WriteString("0001")
				b.Write(pkt(t, "size\n"))
				b.Write(pkt(t, "oid "+oidA+"\n"))
				b.Write(pkt(t, "oid "+oidB+"\n"))
				b.Write(pkt(t, "oid "+oidC+"\n"))
				b.WriteString("0000")
				return b.Bytes()
			},
		},
		{
			name: "agent echo uses default user agent when empty",
			oids: []string{oidA},
			args: ObjectInfoArgs{},
			caps: RawCapabilities{{Name: "agent", Value: "git/2.45.0"}},
			wantBody: func(t *testing.T) []byte {
				var b bytes.Buffer
				b.Write(pkt(t, "command=object-info\n"))
				b.Write(pkt(t, "agent=go-ls-remote\n"))
				b.WriteString("0001")
				b.Write(pkt(t, "oid "+oidA+"\n"))
				b.WriteString("0000")
				return b.Bytes()
			},
		},
		{
			name:      "agent override",
			oids:      []string{oidA},
			args:      ObjectInfoArgs{},
			caps:      RawCapabilities{{Name: "agent", Value: "git/2.45.0"}},
			userAgent: "my-tool/1.0",
			wantBody: func(t *testing.T) []byte {
				var b bytes.Buffer
				b.Write(pkt(t, "command=object-info\n"))
				b.Write(pkt(t, "agent=my-tool/1.0\n"))
				b.WriteString("0001")
				b.Write(pkt(t, "oid "+oidA+"\n"))
				b.WriteString("0000")
				return b.Bytes()
			},
		},
		{
			name: "object-format echo",
			oids: []string{oidA},
			args: ObjectInfoArgs{},
			caps: RawCapabilities{{Name: "object-format", Value: "sha256"}},
			wantBody: func(t *testing.T) []byte {
				var b bytes.Buffer
				b.Write(pkt(t, "command=object-info\n"))
				b.Write(pkt(t, "object-format=sha256\n"))
				b.WriteString("0001")
				b.Write(pkt(t, "oid "+oidA+"\n"))
				b.WriteString("0000")
				return b.Bytes()
			},
		},
		{
			name: "object-format boolean is not echoed",
			oids: []string{oidA},
			args: ObjectInfoArgs{},
			caps: RawCapabilities{{Name: "object-format"}},
			wantBody: func(t *testing.T) []byte {
				var b bytes.Buffer
				b.Write(pkt(t, "command=object-info\n"))
				b.WriteString("0001")
				b.Write(pkt(t, "oid "+oidA+"\n"))
				b.WriteString("0000")
				return b.Bytes()
			},
		},
		{
			name: "agent then object-format in canonical order",
			oids: []string{oidA},
			args: ObjectInfoArgs{},
			caps: RawCapabilities{
				{Name: "agent", Value: "git/2.45.0"},
				{Name: "object-format", Value: "sha1"},
			},
			wantBody: func(t *testing.T) []byte {
				var b bytes.Buffer
				b.Write(pkt(t, "command=object-info\n"))
				b.Write(pkt(t, "agent=go-ls-remote\n"))
				b.Write(pkt(t, "object-format=sha1\n"))
				b.WriteString("0001")
				b.Write(pkt(t, "oid "+oidA+"\n"))
				b.WriteString("0000")
				return b.Bytes()
			},
		},
		{
			name: "empty OIDs slice with size on",
			oids: nil,
			args: ObjectInfoArgs{Size: true},
			caps: nil,
			wantBody: func(t *testing.T) []byte {
				var b bytes.Buffer
				b.Write(pkt(t, "command=object-info\n"))
				b.WriteString("0001")
				b.Write(pkt(t, "size\n"))
				b.WriteString("0000")
				return b.Bytes()
			},
		},
		{
			name: "empty OIDs slice with size off",
			oids: nil,
			args: ObjectInfoArgs{},
			caps: nil,
			wantBody: func(t *testing.T) []byte {
				var b bytes.Buffer
				b.Write(pkt(t, "command=object-info\n"))
				b.WriteString("0001")
				b.WriteString("0000")
				return b.Bytes()
			},
		},
		{
			name: "full happy path",
			oids: []string{oidA, oidB, oidC},
			args: ObjectInfoArgs{Size: true},
			caps: RawCapabilities{
				{Name: "agent", Value: "git/2.45.0"},
				{Name: "object-format", Value: "sha1"},
			},
			userAgent: "my-tool/1.0",
			wantBody: func(t *testing.T) []byte {
				var b bytes.Buffer
				b.Write(pkt(t, "command=object-info\n"))
				b.Write(pkt(t, "agent=my-tool/1.0\n"))
				b.Write(pkt(t, "object-format=sha1\n"))
				b.WriteString("0001")
				b.Write(pkt(t, "size\n"))
				b.Write(pkt(t, "oid "+oidA+"\n"))
				b.Write(pkt(t, "oid "+oidB+"\n"))
				b.Write(pkt(t, "oid "+oidC+"\n"))
				b.WriteString("0000")
				return b.Bytes()
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			w := pktline.NewWriter(&buf)

			require.NoError(t, EncodeObjectInfo(w, tc.oids, tc.args, tc.caps, tc.userAgent))
			assert.Equal(t, tc.wantBody(t), buf.Bytes())
		})
	}
}

// buildObjectInfoStream encodes payloads as data packets followed by a
// flush, matching the canonical v2 `object-info` response framing
// (`gitprotocol-v2.adoc` §"object-info"). Each payload should already
// carry its trailing LF.
func buildObjectInfoStream(t *testing.T, payloads ...string) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	w := pktline.NewWriter(&buf)
	for _, p := range payloads {
		require.NoError(t, w.WritePacket([]byte(p)))
	}
	require.NoError(t, w.WriteFlush())
	return &buf
}

func TestDecodeObjectInfo(t *testing.T) {
	const (
		oid1 = "1111111111111111111111111111111111111111"
		oid2 = "2222222222222222222222222222222222222222"
		oid3 = "3333333333333333333333333333333333333333"
	)

	t.Run("empty (just flush)", func(t *testing.T) {
		var buf bytes.Buffer
		w := pktline.NewWriter(&buf)
		require.NoError(t, w.WriteFlush())

		infos, err := DecodeObjectInfo(pktline.NewReader(&buf))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "attrs")
		assert.Empty(t, infos)
	})

	t.Run("attrs only, no oids", func(t *testing.T) {
		buf := buildObjectInfoStream(t, "size\n")

		infos, err := DecodeObjectInfo(pktline.NewReader(buf))
		require.NoError(t, err)
		assert.Empty(t, infos)
	})

	t.Run("single OID with size", func(t *testing.T) {
		buf := buildObjectInfoStream(t, "size\n", oid1+" 42\n")

		infos, err := DecodeObjectInfo(pktline.NewReader(buf))
		require.NoError(t, err)
		assert.Equal(t, []RawObjectInfo{{OID: oid1, Size: 42}}, infos)
	})

	t.Run("multiple OIDs preserved order", func(t *testing.T) {
		buf := buildObjectInfoStream(t,
			"size\n",
			oid1+" 100\n",
			oid2+" 200\n",
			oid3+" 300\n",
		)

		infos, err := DecodeObjectInfo(pktline.NewReader(buf))
		require.NoError(t, err)
		assert.Equal(t, []RawObjectInfo{
			{OID: oid1, Size: 100},
			{OID: oid2, Size: 200},
			{OID: oid3, Size: 300},
		}, infos)
	})

	t.Run("missing OID dropped", func(t *testing.T) {
		buf := buildObjectInfoStream(t,
			"size\n",
			oid1+" 42\n",
			oid2+" \n", // trailing space, empty size — server cannot resolve
			oid3+" 99\n",
		)

		infos, err := DecodeObjectInfo(pktline.NewReader(buf))
		require.NoError(t, err)
		assert.Equal(t, []RawObjectInfo{
			{OID: oid1, Size: 42},
			{OID: oid3, Size: 99},
		}, infos)
	})

	t.Run("malformed size token", func(t *testing.T) {
		buf := buildObjectInfoStream(t,
			"size\n",
			oid1+" not-a-number\n",
		)

		infos, err := DecodeObjectInfo(pktline.NewReader(buf))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not-a-number")
		assert.Empty(t, infos)
	})

	t.Run("ERR mid-stream after attrs", func(t *testing.T) {
		buf := buildObjectInfoStream(t,
			"size\n",
			"ERR repository disabled\n",
		)

		infos, err := DecodeObjectInfo(pktline.NewReader(buf))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "repository disabled")
		assert.Empty(t, infos)
	})

	t.Run("ERR before attrs", func(t *testing.T) {
		buf := buildObjectInfoStream(t, "ERR something bad\n")

		infos, err := DecodeObjectInfo(pktline.NewReader(buf))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "something bad")
		assert.Empty(t, infos)
	})

	t.Run("unexpected control packet (delim) mid-stream", func(t *testing.T) {
		var buf bytes.Buffer
		w := pktline.NewWriter(&buf)
		require.NoError(t, w.WritePacket([]byte("size\n")))
		require.NoError(t, w.WriteDelim())
		require.NoError(t, w.WriteFlush())

		infos, err := DecodeObjectInfo(pktline.NewReader(&buf))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unexpected control packet")
		assert.Empty(t, infos)
	})

	t.Run("truncated (eof before flush)", func(t *testing.T) {
		var buf bytes.Buffer
		w := pktline.NewWriter(&buf)
		require.NoError(t, w.WritePacket([]byte("size\n")))
		require.NoError(t, w.WritePacket([]byte(oid1+" 42\n")))

		infos, err := DecodeObjectInfo(pktline.NewReader(&buf))
		require.Error(t, err)
		assert.True(t, errors.Is(err, io.ErrUnexpectedEOF),
			"want io.ErrUnexpectedEOF, got %v", err)
		assert.Empty(t, infos)
	})

	t.Run("size attribute absent (empty attrs)", func(t *testing.T) {
		// Degenerate but legal: server sends an empty attrs line, so
		// per-OID lines carry no size token. Decoder returns OIDs with
		// `Size=0`.
		buf := buildObjectInfoStream(t,
			"\n",
			oid1+"\n",
			oid2+"\n",
		)

		infos, err := DecodeObjectInfo(pktline.NewReader(buf))
		require.NoError(t, err)
		assert.Equal(t, []RawObjectInfo{
			{OID: oid1, Size: 0},
			{OID: oid2, Size: 0},
		}, infos)
	})
}

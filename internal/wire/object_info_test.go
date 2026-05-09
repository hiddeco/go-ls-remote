package wire

import (
	"bytes"
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

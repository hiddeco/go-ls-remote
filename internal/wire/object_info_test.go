package wire

import (
	"bytes"
	"errors"
	"io"
	"strconv"
	"strings"
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
				b.Write(pkt(t, "agent="+DefaultUserAgent+"\n"))
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
				b.Write(pkt(t, "agent="+DefaultUserAgent+"\n"))
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
// ([gitprotocol-v2.adoc §"object-info"]). Each payload should already
// carry its trailing LF.
//
// [gitprotocol-v2.adoc §"object-info"]: https://github.com/git/git/blob/v2.54.0/Documentation/gitprotocol-v2.adoc#object-info
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
		// [protocol-caps.c::send_info lines 44-45] short-circuit on an
		// empty OID list and emit neither attrs nor per-OID lines — the
		// response is a bare flush. The decoder must accept that as an
		// empty result, not as a missing-attrs error.
		//
		// [protocol-caps.c::send_info lines 44-45]: https://github.com/git/git/blob/v2.54.0/protocol-caps.c#L44-L45
		var buf bytes.Buffer
		w := pktline.NewWriter(&buf)
		require.NoError(t, w.WriteFlush())

		infos, err := DecodeObjectInfo(pktline.NewReader(&buf))
		require.NoError(t, err)
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
		assert.ErrorIs(t, err, ErrServerRefused)
		assert.Contains(t, err.Error(), "repository disabled")
		assert.Empty(t, infos)
	})

	t.Run("ERR before attrs", func(t *testing.T) {
		buf := buildObjectInfoStream(t, "ERR something bad\n")

		infos, err := DecodeObjectInfo(pktline.NewReader(buf))
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrServerRefused)
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

	t.Run("empty OID lines dropped", func(t *testing.T) {
		// Regression for a fuzz finding: a per-OID line that begins with
		// a space (or is empty) made the decoder surface a `RawObjectInfo`
		// with an empty `OID`. `send_info` never emits an empty OID, so
		// such lines are malformed and must be dropped.
		buf := buildObjectInfoStream(t,
			"\n",    // attrs absent
			"\n",    // empty per-OID line
			" 42\n", // leading space, no OID, tail "42"
			" \n",   // leading space only
			oid1+"\n",
		)

		infos, err := DecodeObjectInfo(pktline.NewReader(buf))
		require.NoError(t, err)
		assert.Equal(t, []RawObjectInfo{{OID: oid1}}, infos)
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

	t.Run("attrs line elided, single OID (canonical no-size)", func(t *testing.T) {
		// [protocol-caps.c::send_info lines 47-48] gate the `size\n` attrs
		// PKT-LINE on `info->size`; when the client did not request the
		// `size` argument, canonical Git emits no attrs line at all and
		// jumps straight to per-OID `<oid>\n` lines. The decoder must
		// recognise the first packet as a per-OID line in that shape and
		// not consume it as a degenerate attrs line.
		//
		// [protocol-caps.c::send_info lines 47-48]: https://github.com/git/git/blob/v2.54.0/protocol-caps.c#L47-L48
		buf := buildObjectInfoStream(t,
			oid1+"\n",
		)

		infos, err := DecodeObjectInfo(pktline.NewReader(buf))
		require.NoError(t, err)
		assert.Equal(t, []RawObjectInfo{{OID: oid1}}, infos)
	})

	t.Run("attrs line elided, multiple OIDs (canonical no-size)", func(t *testing.T) {
		buf := buildObjectInfoStream(t,
			oid1+"\n",
			oid2+"\n",
			oid3+"\n",
		)

		infos, err := DecodeObjectInfo(pktline.NewReader(buf))
		require.NoError(t, err)
		assert.Equal(t, []RawObjectInfo{
			{OID: oid1},
			{OID: oid2},
			{OID: oid3},
		}, infos)
	})

}

// readRequestArgs walks an encoded `object-info` request stream and
// returns the recovered `oid` arguments together with the size-flag.
// The wire shape it expects mirrors `EncodeObjectInfo` (and
// [gitprotocol-v2.adoc §"object-info" lines 556-585]): a
// `command=object-info` data packet, zero or more capability-echo
// data packets, a `0001` delim, then the body of `size` and `oid <hex>`
// lines closed by a `0000` flush. A failure to match any of those
// constraints fails the test.
//
// [gitprotocol-v2.adoc §"object-info" lines 556-585]: https://github.com/git/git/blob/v2.54.0/Documentation/gitprotocol-v2.adoc?plain=1#L556-L585
func readRequestArgs(t *testing.T, raw []byte) (oids []string, sizeFlag bool) {
	t.Helper()
	r := pktline.NewReader(bytes.NewReader(raw))

	// First packet: `command=object-info`.
	first, err := r.ReadPacket()
	require.NoError(t, err)
	require.Equal(t, pktline.Data, first.Kind)
	require.Equal(t, "command=object-info\n", string(first.Data))

	// Header section: capability-echo lines until the delim.
	for {
		p, err := r.ReadPacket()
		require.NoError(t, err)
		if p.Kind == pktline.Delim {
			break
		}
		require.Equal(t, pktline.Data, p.Kind,
			"unexpected control packet in header: %v", p.Kind)
	}

	// Body section: `size` and/or `oid <hex>` lines until flush.
	for {
		p, err := r.ReadPacket()
		require.NoError(t, err)
		if p.Kind == pktline.Flush {
			return oids, sizeFlag
		}
		require.Equal(t, pktline.Data, p.Kind,
			"unexpected control packet in body: %v", p.Kind)
		line := string(bytes.TrimSuffix(p.Data, []byte{'\n'}))
		switch {
		case line == "size":
			sizeFlag = true
		case strings.HasPrefix(line, "oid "):
			oids = append(oids, strings.TrimPrefix(line, "oid "))
		default:
			t.Fatalf("unrecognised body line %q", line)
		}
	}
}

// emitObjectInfoResponse serialises a server-side `object-info`
// response from the given infos. It mirrors [protocol-caps.c::send_info]
// exactly: with `size` requested, the `size\n` attrs PKT-LINE precedes
// the `<oid> <size>\n` rows ([send_info:47-48]); without `size`, no
// attrs PKT-LINE is emitted at all and rows are bare `<oid>\n`
// ([send_info:63]). The trailing flush closes the response.
//
// [protocol-caps.c::send_info]: https://github.com/git/git/blob/v2.54.0/protocol-caps.c#L37
// [send_info:47-48]: https://github.com/git/git/blob/v2.54.0/protocol-caps.c#L47-L48
// [send_info:63]: https://github.com/git/git/blob/v2.54.0/protocol-caps.c#L63
func emitObjectInfoResponse(t *testing.T, infos []RawObjectInfo, withSize bool) *bytes.Buffer {
	t.Helper()
	payloads := make([]string, 0, 1+len(infos))
	if withSize {
		payloads = append(payloads, "size\n")
	}
	for _, info := range infos {
		if withSize {
			payloads = append(payloads,
				info.OID+" "+strconv.FormatInt(info.Size, 10)+"\n")
		} else {
			payloads = append(payloads, info.OID+"\n")
		}
	}
	return buildObjectInfoStream(t, payloads...)
}

// TestObjectInfo_roundTrip pins encode/decode against silent drift.
// `EncodeObjectInfo` writes the client request (`command=object-info`
// header, `0001` delim, `size` plus `oid <hex>` body, flush) while
// `DecodeObjectInfo` reads the server response (attrs line,
// `<oid>[ <size>]` rows, flush) — the two are *not* mirror images of
// each other ([protocol-caps.c::cap_object_info] vs
// [protocol-caps.c::send_info]). The cases below therefore lock two
// independent loops:
//
//  1. Request: encode an `ObjectInfoArgs` plus OIDs, re-parse the
//     produced pkt-line stream, and check that the OID list and the
//     size-flag survived the trip.
//  2. Response: synthesise a canonical server response that matches
//     the request's size-flag, decode it, re-emit a server stream
//     from the decoded rows, decode that, and assert idempotence.
//
// Together the two loops keep encoder and decoder honest even though
// no single function exercises both ends.
//
// [protocol-caps.c::cap_object_info]: https://github.com/git/git/blob/v2.54.0/protocol-caps.c#L79
// [protocol-caps.c::send_info]: https://github.com/git/git/blob/v2.54.0/protocol-caps.c#L37
func TestObjectInfo_roundTrip(t *testing.T) {
	const (
		oidA = "1111111111111111111111111111111111111111"
		oidB = "2222222222222222222222222222222222222222"
		oidC = "3333333333333333333333333333333333333333"
	)

	cases := []struct {
		name      string
		oids      []string
		args      ObjectInfoArgs
		wantInfos []RawObjectInfo
	}{
		{
			name:      "single OID, size off",
			oids:      []string{oidA},
			args:      ObjectInfoArgs{},
			wantInfos: []RawObjectInfo{{OID: oidA}},
		},
		{
			name:      "single OID, size on",
			oids:      []string{oidA},
			args:      ObjectInfoArgs{Size: true},
			wantInfos: []RawObjectInfo{{OID: oidA, Size: 42}},
		},
		{
			name: "multiple OIDs, size on",
			oids: []string{oidA, oidB, oidC},
			args: ObjectInfoArgs{Size: true},
			wantInfos: []RawObjectInfo{
				{OID: oidA, Size: 100},
				{OID: oidB, Size: 200},
				{OID: oidC, Size: 300},
			},
		},
		{
			name: "multiple OIDs, size off",
			oids: []string{oidA, oidB, oidC},
			args: ObjectInfoArgs{},
			wantInfos: []RawObjectInfo{
				{OID: oidA},
				{OID: oidB},
				{OID: oidC},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("request", func(t *testing.T) {
				var buf bytes.Buffer
				w := pktline.NewWriter(&buf)
				require.NoError(t,
					EncodeObjectInfo(w, tc.oids, tc.args, nil, ""))

				gotOIDs, gotSize := readRequestArgs(t, buf.Bytes())
				assert.Equal(t, tc.oids, gotOIDs)
				assert.Equal(t, tc.args.Size, gotSize)
			})

			t.Run("response idempotent", func(t *testing.T) {
				first := emitObjectInfoResponse(t, tc.wantInfos, tc.args.Size)
				gotFirst, err := DecodeObjectInfo(pktline.NewReader(first))
				require.NoError(t, err)
				assert.Equal(t, tc.wantInfos, gotFirst)

				second := emitObjectInfoResponse(t, gotFirst, tc.args.Size)
				gotSecond, err := DecodeObjectInfo(pktline.NewReader(second))
				require.NoError(t, err)
				assert.Equal(t, gotFirst, gotSecond)
			})
		})
	}
}

package wire

import (
	"bytes"
	"strconv"
	"testing"

	"github.com/hiddeco/go-ls-remote/pktline"
)

// benchObjectInfoSink defeats dead-code elimination on the
// `DecodeObjectInfo` micro-benchmarks; without it the compiler can
// erase the loop body, since the decoded slice is otherwise unused.
var benchObjectInfoSink []RawObjectInfo

// buildBenchObjectInfoStream serialises a server-side `object-info`
// response into a contiguous byte slice the bench can rewind per
// iteration. It mirrors `protocol-caps.c::send_info`'s success-path
// framing: with `size` requested, a `size\n` attrs PKT-LINE precedes
// the `<oid> <size>\n` rows (`send_info:47-48`); without `size`, no
// attrs PKT-LINE is emitted and rows are bare `<oid>\n`
// (`send_info:63`). When withMissing is true every fourth row is
// replaced with the canonical `<oid> ` shape that `send_info` writes
// when `odb_read_object_info` cannot resolve the OID —
// `DecodeObjectInfo` filters those, so this exercises the drop path
// alongside the resolved-row hot path.
func buildBenchObjectInfoStream(b *testing.B, n int, withSize, withMissing bool) []byte {
	b.Helper()
	var buf bytes.Buffer
	w := pktline.NewWriter(&buf)

	if withSize {
		if err := w.WritePacket([]byte("size\n")); err != nil {
			b.Fatal(err)
		}
	}

	for i := range n {
		// Synthesise a 40-char lowercase-hex OID by repeating a single
		// digit. The decoder does not validate the OID's bytes (that is
		// the server's job in `cap_object_info`); any 40 hex chars
		// suffice to reproduce the FieldsSeq / strings.Cut path.
		oid := strconv.FormatInt(int64(i+1), 16)
		for len(oid) < 40 {
			oid = "0" + oid
		}
		var line string
		switch {
		case withMissing && i%4 == 3:
			// `<oid> ` — server failed to resolve. Decoder drops.
			line = oid + " \n"
		case withSize:
			line = oid + " " + strconv.Itoa(i+1) + "\n"
		default:
			line = oid + "\n"
		}
		if err := w.WritePacket([]byte(line)); err != nil {
			b.Fatal(err)
		}
	}
	if err := w.WriteFlush(); err != nil {
		b.Fatal(err)
	}
	return buf.Bytes()
}

// BenchmarkDecodeObjectInfo measures the steady-state cost of
// decoding a v2 `object-info` response. `DecodeObjectInfo` runs once
// per call but `parseObjectInfoLine` runs once per OID in the
// response, so the per-OID path dominates anything that asks about
// hundreds of objects at a time (dependency-resolution tools, batch
// metadata probes). The header attrs line goes through
// `strings.FieldsSeq(string(attrsLine))` and each per-OID line through
// `strings.Cut`; both are on the hot path the bench protects.
//
// The two attribute shapes (`with-size` and `without-size`) reflect
// the request-driven shape switch in `protocol-caps.c::send_info`:
// when the client did not echo `size`, every row is the bare-OID
// form, which skips `strconv.ParseInt` entirely. The mixed-with-empty
// row uses the trailing-space `<oid> ` shape that `send_info` writes
// for an OID it cannot resolve, exercising the filter path that
// `DecodeObjectInfo` applies via `parseObjectInfoLine`'s drop signal.
//
// See `internal/wire/object_info.go` for the parser; canonical shape
// is in `protocol-caps.c::send_info` (lines 40-77).
func BenchmarkDecodeObjectInfo(b *testing.B) {
	type variant struct {
		name        string
		withSize    bool
		withMissing bool
	}
	variants := []variant{
		{name: "with-size", withSize: true},
		{name: "without-size", withSize: false},
		{name: "with-size+missing", withSize: true, withMissing: true},
	}

	for _, v := range variants {
		for _, n := range []int{10, 100, 1000} {
			name := v.name + "/n=" + strconv.Itoa(n)
			b.Run(name, func(b *testing.B) {
				raw := buildBenchObjectInfoStream(b, n, v.withSize, v.withMissing)
				rd := bytes.NewReader(raw)

				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					rd.Reset(raw)
					infos, err := DecodeObjectInfo(pktline.NewReader(rd))
					if err != nil {
						b.Fatal(err)
					}
					benchObjectInfoSink = infos
				}
			})
		}
	}
}

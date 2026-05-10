package pktline

import (
	"bytes"
	"fmt"
	"io"
	"strconv"
	"testing"
)

// Package-level sinks defeat dead-code elimination on micro-benchmarks
// whose results would otherwise be unused — without them the compiler
// can erase loops that look like `for b.Loop() { f() }`.
var (
	benchUintSink uint64
	benchByteSink byte
	benchStrSink  string
)

// loopingReader returns bytes from src, wrapping back to offset 0
// when exhausted. A single Read call never wraps mid-buffer, so
// when src is a concatenation of whole pkt-lines the wrap point
// always falls on a packet boundary and [Reader.ReadPacket] never
// sees a torn header or payload.
type loopingReader struct {
	src []byte
	off int
}

func (r *loopingReader) Read(p []byte) (int, error) {
	if r.off >= len(r.src) {
		r.off = 0
	}
	n := copy(p, r.src[r.off:])
	r.off += n
	return n, nil
}

func BenchmarkReader_ReadPacket_Data(b *testing.B) {
	// 8-byte payload yields a 12-byte pkt-line; the leading `000c`
	// is the on-wire length prefix. The first ReadPacket grows the
	// internal buffer from zero; every subsequent call hits the
	// `cap(r.buf) >= payloadLen` fast path.
	pkt := []byte("000c12345678")
	r := NewReader(&loopingReader{src: bytes.Repeat(pkt, 1024)})

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := r.ReadPacket(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkReader_ReadPacket_Flush(b *testing.B) {
	// Flush is the cheapest packet shape: a 4-byte header and no
	// payload read. Distinct sub-bench because the codec branches
	// before the buffer-grow logic.
	r := NewReader(&loopingReader{src: bytes.Repeat([]byte("0000"), 1024)})

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := r.ReadPacket(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkWriter_WritePacket(b *testing.B) {
	// Small payload representative of a v2 ref line; the writer
	// reuses its internal `out` buffer after the first call.
	payload := []byte("12345678")
	w := NewWriter(io.Discard)

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if err := w.WritePacket(payload); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkParseHexLength and BenchmarkParseHexLength_Strconv compare
// the hand-rolled four-byte hex parser against `strconv.ParseUint`.
// The hand-rolled version exists to avoid the per-call `string(b)`
// allocation that the strconv path requires; these benches pin both
// the speed and the allocation count of the comparison.

func BenchmarkParseHexLength(b *testing.B) {
	hdr := [4]byte{'0', '0', 'c', '4'}
	var acc int

	b.ReportAllocs()
	for b.Loop() {
		v, err := parseHexLength(hdr)
		if err != nil {
			b.Fatal(err)
		}
		acc ^= v
	}
	benchUintSink = uint64(acc)
}

func BenchmarkParseHexLength_Strconv(b *testing.B) {
	hdr := [4]byte{'0', '0', 'c', '4'}
	var acc uint64

	b.ReportAllocs()
	for b.Loop() {
		// Mirrors how a strconv-based reader would have to call it:
		// the byte slice must be converted to a string.
		v, err := strconv.ParseUint(string(hdr[:]), 16, 16)
		if err != nil {
			b.Fatal(err)
		}
		acc ^= v
	}
	benchUintSink = acc
}

// BenchmarkEncodeHexLength and BenchmarkEncodeHexLength_Sprintf
// compare the hand-rolled writer against `fmt.Sprintf("%04x", v)`.
// The Sprintf form returns a fresh string per call; the hand-rolled
// form writes into a caller-owned buffer with no allocation.

func BenchmarkEncodeHexLength(b *testing.B) {
	var buf [4]byte
	var acc byte

	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		// `i & 0xffff` defeats constant-folding without straying
		// outside the 4-hex-digit range the encoder targets.
		encodeHexLength(buf[:], i&0xffff)
		acc ^= buf[0]
	}
	benchByteSink = acc
}

func BenchmarkEncodeHexLength_Sprintf(b *testing.B) {
	var acc string

	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		acc = fmt.Sprintf("%04x", i&0xffff)
	}
	benchStrSink = acc
}

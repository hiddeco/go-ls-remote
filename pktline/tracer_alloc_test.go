package pktline

import (
	"bytes"
	"testing"

	"github.com/hiddeco/go-ls-remote/trace"
)

// discardingTracer is a non-nil [trace.Tracer] whose methods return
// immediately. Used to keep `trace.IsEnabled` true on the emit path so
// every benchmark and alloc test exercises the full emit code path.
type discardingTracer struct{}

func (discardingTracer) OnPacketEvent(*trace.PacketEvent) {}
func (discardingTracer) OnEvent(trace.Event)              {}

// TestWriter_emit_zeroAllocsPerCall pins the per-emit allocation budget
// of `Writer.emit` once a tracer is wired in. The pre-allocated
// `*trace.PacketEvent` on the writer is reused across emits and passed
// directly to `Tracer.OnPacketEvent` — no heap copy per pkt-line.
func TestWriter_emit_zeroAllocsPerCall(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf,
		WithWriterTracerURL(discardingTracer{}, trace.DirectionOutbound, "test://"))
	payload := []byte("hello\n")

	// Warmup: WritePacket may grow `w.out` on the first call. After
	// the warmup the scratch is sized for the payload and subsequent
	// calls must not allocate at all.
	if err := w.WritePacket(payload); err != nil {
		t.Fatal(err)
	}

	avg := testing.AllocsPerRun(100, func() {
		buf.Reset()
		if err := w.WritePacket(payload); err != nil {
			t.Fatal(err)
		}
	})
	if avg != 0 {
		t.Fatalf("Writer.WritePacket allocs/op = %.2f, want 0", avg)
	}
}

// TestReader_emit_zeroAllocsPerCall pins the per-emit allocation budget
// of `Reader.emit`. Same contract as the writer side: a pre-allocated
// `*trace.PacketEvent` on the reader is reused across calls and passed
// directly to `Tracer.OnPacketEvent` — no heap copy per pkt-line.
func TestReader_emit_zeroAllocsPerCall(t *testing.T) {
	// Build a stream of identical data packets so each iteration of the
	// benchmark loop reads one without exhausting the source.
	const iterations = 256
	var src bytes.Buffer
	{
		w := NewWriter(&src)
		for range iterations {
			if err := w.WritePacket([]byte("hello\n")); err != nil {
				t.Fatal(err)
			}
		}
	}

	r := NewReader(bytes.NewReader(src.Bytes()),
		WithReaderTracerURL(discardingTracer{}, trace.DirectionInbound, "test://"))

	// Warmup: the Reader grows `r.buf` to fit the first payload.
	if _, err := r.ReadPacket(); err != nil {
		t.Fatal(err)
	}

	// AllocsPerRun calls f once before measuring and then runs it
	// `runs` more times. With `iterations` packets in the source we
	// have headroom for the warmup plus the measured calls.
	const runs = 50
	avg := testing.AllocsPerRun(runs, func() {
		if _, err := r.ReadPacket(); err != nil {
			t.Fatal(err)
		}
	})
	if avg != 0 {
		t.Fatalf("Reader.ReadPacket allocs/op = %.2f, want 0", avg)
	}
}

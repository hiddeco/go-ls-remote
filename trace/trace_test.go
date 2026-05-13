package trace

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestDirection(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		d    Direction
		want uint8
	}{
		{"inbound", DirectionInbound, 1},
		{"outbound", DirectionOutbound, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, uint8(tt.d))
		})
	}
}

type fakeEvent struct{ t time.Time }

func (f fakeEvent) When() time.Time { return f.t }

func TestEvent(t *testing.T) {
	t.Parallel()
	now := time.Unix(1234567890, 0)
	var e Event = fakeEvent{t: now}
	assert.Equal(t, now, e.When())
}

type fakeTracer struct {
	got []Event
}

func (f *fakeTracer) OnPacketEvent(e *PacketEvent) {
	cloned := *e
	if cloned.Bytes != nil {
		cloned.Bytes = bytes.Clone(cloned.Bytes)
	}
	f.got = append(f.got, cloned)
}
func (f *fakeTracer) OnEvent(e Event) { f.got = append(f.got, e) }

func TestTracer(t *testing.T) {
	t.Parallel()
	tr := &fakeTracer{}
	var asTracer Tracer = tr
	asTracer.OnEvent(fakeEvent{t: time.Unix(0, 0)})
	assert.Len(t, tr.got, 1)
}

func TestPacketKind(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		k    PacketKind
		want uint8
	}{
		{"data", PacketData, 1},
		{"flush", PacketFlush, 2},
		{"delim", PacketDelim, 3},
		{"response-end", PacketResponseEnd, 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, uint8(tt.k))
		})
	}
}

package trace

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestIsEnabled(t *testing.T) {
	assert.False(t, IsEnabled(nil))
	assert.True(t, IsEnabled(&fakeTracer{}))
}

func TestEmit(t *testing.T) {
	t.Run("nil tracer is a no-op", func(t *testing.T) {
		// Must not panic on nil receiver.
		Emit(nil, fakeEvent{t: time.Unix(0, 0)})
	})

	t.Run("non-nil tracer receives the event", func(t *testing.T) {
		tr := &fakeTracer{}
		want := fakeEvent{t: time.Unix(1234567890, 0)}
		Emit(tr, want)
		assert.Equal(t, []Event{want}, tr.got)
	})
}

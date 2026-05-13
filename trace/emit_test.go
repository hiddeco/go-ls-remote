package trace

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestIsEnabled(t *testing.T) {
	t.Parallel()
	assert.False(t, IsEnabled(nil))
	assert.True(t, IsEnabled(&fakeTracer{}))
}

func TestEmit(t *testing.T) {
	t.Parallel()
	t.Run("nil tracer is a no-op", func(t *testing.T) {
		t.Parallel()
		// Must not panic on nil receiver.
		Emit(nil, fakeEvent{t: time.Unix(0, 0)})
	})

	t.Run("non-nil tracer receives the event", func(t *testing.T) {
		t.Parallel()
		tr := &fakeTracer{}
		want := fakeEvent{t: time.Unix(1234567890, 0)}
		Emit(tr, want)
		assert.Equal(t, []Event{want}, tr.got)
	})
}

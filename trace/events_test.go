package trace

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEvents_When asserts that every built-in event type satisfies
// [Event] and propagates its Time field through When(). The typed slice
// element type forces compile-time verification of the interface
// satisfaction; the assertion verifies the runtime value.
func TestEvents_When(t *testing.T) {
	now := time.Unix(1234567890, 0)
	tests := []struct {
		name string
		e    Event
	}{
		{"PacketEvent", PacketEvent{
			Time: now, Direction: DirectionInbound, URL: "https://example.com/repo",
			Bytes: []byte("hi"), Kind: PacketData,
		}},
		{"HTTPEvent", HTTPEvent{
			Time: now, Method: "GET", URL: "https://example.com/info/refs",
			Status: 200, Duration: time.Second,
		}},
		{"NegotiateEvent", NegotiateEvent{
			Time: now, URL: "https://example.com/repo", Version: 2,
			ServerAgent: "git/2.45", Capabilities: []string{"ls-refs", "object-info"},
		}},
		{"CommandEvent", CommandEvent{
			Time: now, URL: "https://example.com/repo", Name: "ls-refs",
			Phase: CommandStart,
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, now, tt.e.When())
		})
	}
}

// TestCommandEvent_Err verifies that a non-nil Err on the End phase
// round-trips losslessly through the struct alongside Phase and
// Duration, the other fields a consumer reads to distinguish End
// frames from Start frames.
func TestCommandEvent_Err(t *testing.T) {
	boom := errors.New("boom")
	e := CommandEvent{
		Phase:    CommandEnd,
		Duration: 5 * time.Millisecond,
		Err:      boom,
	}
	assert.Equal(t, CommandEnd, e.Phase)
	assert.Equal(t, 5*time.Millisecond, e.Duration)
	require.ErrorIs(t, e.Err, boom)
}

func TestCommandPhase(t *testing.T) {
	tests := []struct {
		name string
		p    CommandPhase
		want uint8
	}{
		{"start", CommandStart, 1},
		{"end", CommandEnd, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, uint8(tt.p))
		})
	}
}

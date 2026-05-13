package transport

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSchemeError_Error(t *testing.T) {
	t.Parallel()
	e := &SchemeError{Msg: "x"}
	assert.Equal(t, "x", e.Error())
}

func TestSchemeError_Is(t *testing.T) {
	t.Parallel()
	parent := errors.New("generic parent")
	unrelated := errors.New("unrelated")

	e := &SchemeError{Parent: parent, Msg: "scheme: something"}

	tests := []struct {
		name   string
		err    error
		target error
		want   bool
	}{
		{
			name:   "pointer identity matches itself",
			err:    e,
			target: e,
			want:   true,
		},
		{
			name:   "matches parent via Is",
			err:    e,
			target: parent,
			want:   true,
		},
		{
			name:   "does not match unrelated error",
			err:    e,
			target: unrelated,
			want:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var se *SchemeError
			require.ErrorAs(t, tt.err, &se,
				"test setup error: tt.err must be a *SchemeError; got %T", tt.err)
			assert.Equal(t, tt.want, se.Is(tt.target))
		})
	}

	// Two distinct *SchemeError values with the same Parent must NOT
	// match each other via errors.Is: the bridge is one-directional
	// (scheme → parent), not between sibling sentinels.
	t.Run("distinct SchemeErrors with same Parent do not match", func(t *testing.T) {
		t.Parallel()
		e1 := &SchemeError{Parent: parent, Msg: "scheme1: foo"}
		e2 := &SchemeError{Parent: parent, Msg: "scheme2: bar"}
		assert.NotErrorIs(t, e1, e2,
			"errors.Is(e1, e2) must be false even when Parents are equal")
	})

	// Verify the full errors.Is walk works (not just the direct Is method).
	t.Run("errors.Is walk matches parent", func(t *testing.T) {
		t.Parallel()
		assert.ErrorIs(t, e, parent,
			"errors.Is must reach the parent via the public walk")
	})
}

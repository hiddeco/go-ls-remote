package transport

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// makeFake constructs a fakeTransport claiming the given schemes,
// tagged with id to distinguish otherwise-equivalent instances.
// Tests use it as a stand-in for any concrete transport implementation.
func makeFake(id string, schemes ...string) fakeTransport {
	return fakeTransport{id: id, schemes: schemes}
}

func TestRegistry_LookupHitAndMiss(t *testing.T) {
	r := NewRegistry(makeFake("only", "fake"))

	got, ok := r.Lookup("fake")
	require.True(t, ok)
	assert.Equal(t, "only", got.(fakeTransport).id)

	_, ok = r.Lookup("nope")
	assert.False(t, ok)
}

func TestRegistry_LookupCaseInsensitive(t *testing.T) {
	r := NewRegistry(makeFake("only", "Fake"))

	tests := []string{"fake", "FAKE", "Fake", "FaKe"}
	for _, s := range tests {
		t.Run(s, func(t *testing.T) {
			_, ok := r.Lookup(s)
			assert.True(t, ok, "lookup of %q should succeed", s)
		})
	}
}

func TestRegistry_RegisterReplaces(t *testing.T) {
	r := NewRegistry(makeFake("first", "fake"))
	r.Register(makeFake("second", "fake"))

	got, ok := r.Lookup("fake")
	require.True(t, ok)
	assert.Equal(t, "second", got.(fakeTransport).id,
		"later Register should replace earlier binding for the same scheme")
}

func TestRegistry_Schemes(t *testing.T) {
	r := NewRegistry(makeFake("a", "fake"), makeFake("b", "primary", "alt"))

	got := r.Schemes()
	slices.Sort(got)
	assert.Equal(t, []string{"alt", "fake", "primary"}, got)
}

func TestRegistry_emptyRegistryRoundTrip(t *testing.T) {
	r := NewRegistry()
	assert.Empty(t, r.Schemes())

	_, ok := r.Lookup("anything")
	assert.False(t, ok)
}

func TestRegistry_RegisterPanicsOnNil(t *testing.T) {
	t.Run("via NewRegistry", func(t *testing.T) {
		assert.Panics(t, func() { NewRegistry(nil) })
	})
	t.Run("via Register", func(t *testing.T) {
		r := NewRegistry()
		assert.Panics(t, func() { r.Register(nil) })
	})
}

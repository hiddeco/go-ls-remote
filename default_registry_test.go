package lsremote

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_defaultRegistry(t *testing.T) {
	t.Parallel()

	t.Run("non-nil registry", func(t *testing.T) {
		t.Parallel()

		got := defaultRegistry()
		require.NotNil(t, got,
			"defaultRegistry must return a usable Registry, not nil")
	})

	t.Run("registers both HTTP schemes", func(t *testing.T) {
		t.Parallel()

		got := defaultRegistry()
		schemes := got.Schemes()
		assert.ElementsMatch(t, []string{"https", "http"}, schemes,
			"default registry must expose exactly the HTTP transport's schemes")
	})

	t.Run("lookup https resolves", func(t *testing.T) {
		t.Parallel()

		got := defaultRegistry()
		tr, ok := got.Lookup("https")
		require.True(t, ok, "https must resolve in the default registry")
		assert.NotNil(t, tr, "Lookup must return a non-nil Transport on hit")
	})

	t.Run("lookup http resolves", func(t *testing.T) {
		t.Parallel()

		got := defaultRegistry()
		tr, ok := got.Lookup("http")
		require.True(t, ok, "http must resolve in the default registry")
		assert.NotNil(t, tr, "Lookup must return a non-nil Transport on hit")
	})

	t.Run("lookup ssh misses", func(t *testing.T) {
		t.Parallel()

		got := defaultRegistry()
		_, ok := got.Lookup("ssh")
		assert.False(t, ok,
			"default registry is HTTP-only; ssh must not resolve")
	})

	t.Run("each call returns an independent registry", func(t *testing.T) {
		t.Parallel()

		// Callers must not share registry state via the default — a
		// subsequent Register call on one Dial's registry must not be
		// observable from another Dial's.
		a := defaultRegistry()
		b := defaultRegistry()
		assert.NotSame(t, a, b,
			"defaultRegistry must allocate a fresh Registry per call")
	})
}

package gitt

import (
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWithDialer(t *testing.T) {
	t.Parallel()

	t.Run("explicit dialer is returned", func(t *testing.T) {
		t.Parallel()

		d := &net.Dialer{Timeout: 5 * time.Second}
		tr := New(WithDialer(d))
		require.NotNil(t, tr)
		got := tr.resolvedDialer()
		assert.Same(t, d, got)
	})

	t.Run("nil option uses package default", func(t *testing.T) {
		t.Parallel()

		tr := New()
		got := tr.resolvedDialer()
		require.NotNil(t, got)
		assert.Equal(t, 30*time.Second, got.Timeout,
			"default dialer timeout must be 30s")
	})

	t.Run("WithDialer nil uses package default", func(t *testing.T) {
		t.Parallel()

		tr := New(WithDialer(nil))
		got := tr.resolvedDialer()
		require.NotNil(t, got)
		assert.Equal(t, 30*time.Second, got.Timeout,
			"default dialer timeout must be 30s")
	})
}

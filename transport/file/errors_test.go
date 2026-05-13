package filet

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/hiddeco/go-ls-remote/transport"
)

func TestSentinels_BridgeToTransport(t *testing.T) {
	t.Parallel()

	// ErrNotFound and ErrUnsupportedProtocol are declared as
	// *transport.SchemeError and must satisfy errors.Is for their
	// matching generic parent.
	t.Run("ErrNotFound bridges to transport.ErrNotFound", func(t *testing.T) {
		t.Parallel()

		assert.ErrorIs(t, ErrNotFound, transport.ErrNotFound,
			"errors.Is(filet.ErrNotFound, transport.ErrNotFound) must be true")
	})

	t.Run("ErrUnsupportedProtocol bridges to transport.ErrUnsupportedProtocol", func(t *testing.T) {
		t.Parallel()

		assert.ErrorIs(t, ErrUnsupportedProtocol, transport.ErrUnsupportedProtocol,
			"errors.Is(filet.ErrUnsupportedProtocol, transport.ErrUnsupportedProtocol) must be true")
	})

	// ErrServerRefused and ErrUnsupportedFormat are scheme-local and
	// must NOT match any generic transport sentinel.
	t.Run("ErrServerRefused has no generic bridge", func(t *testing.T) {
		t.Parallel()

		allGeneric := []error{
			transport.ErrNotFound,
			transport.ErrAuthRequired,
			transport.ErrAuthFailed,
			transport.ErrUnsupportedProtocol,
		}
		for _, g := range allGeneric {
			assert.NotErrorIs(t, ErrServerRefused, g,
				"ErrServerRefused must not match generic sentinel %v", g)
		}
	})

	t.Run("ErrUnsupportedFormat has no generic bridge", func(t *testing.T) {
		t.Parallel()

		allGeneric := []error{
			transport.ErrNotFound,
			transport.ErrAuthRequired,
			transport.ErrAuthFailed,
			transport.ErrUnsupportedProtocol,
		}
		for _, g := range allGeneric {
			assert.NotErrorIs(t, ErrUnsupportedFormat, g,
				"ErrUnsupportedFormat must not match generic sentinel %v", g)
		}
	})
}

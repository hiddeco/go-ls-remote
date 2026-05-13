package ssht

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hiddeco/go-ls-remote/transport"
)

// TestSentinels_BridgeToTransport verifies the scheme-local sentinels
// declared as [*transport.SchemeError] match their generic parent via
// [errors.Is], mirroring the bridge contract of the other transports.
func TestSentinels_BridgeToTransport(t *testing.T) {
	t.Parallel()
	t.Run("ErrAuthRequired bridges to transport.ErrAuthRequired", func(t *testing.T) {
		t.Parallel()
		assert.ErrorIs(t, ErrAuthRequired, transport.ErrAuthRequired)
	})
	t.Run("ErrAuthFailed bridges to transport.ErrAuthFailed", func(t *testing.T) {
		t.Parallel()
		assert.ErrorIs(t, ErrAuthFailed, transport.ErrAuthFailed)
	})
	t.Run("ErrNotFound bridges to transport.ErrNotFound", func(t *testing.T) {
		t.Parallel()
		assert.ErrorIs(t, ErrNotFound, transport.ErrNotFound)
	})
}

// TestProtocolError_Error verifies the rendered form is
// `transport/ssh: <op> <URL>: <cause>` with zero-valued fields elided.
func TestProtocolError_Error(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  *ProtocolError
		want string
	}{
		{
			name: "op + url + wrapped cause",
			err:  &ProtocolError{URL: "ssh://git@example.com/repo.git", Op: "dial", Err: ErrAuthFailed},
			want: "transport/ssh: dial ssh://git@example.com/repo.git: transport/ssh: authentication failed",
		},
		{
			name: "no url, no op, only cause",
			err:  &ProtocolError{Err: errors.New("boom")},
			want: "transport/ssh:: boom",
		},
		{
			name: "server diagnostic with no Err",
			err:  &ProtocolError{Op: "dial", URL: "ssh://h/r", Server: "fatal: nope"},
			want: "transport/ssh: dial ssh://h/r: fatal: nope",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, tc.err.Error())
		})
	}
}

// TestProtocolError_Unwrap verifies the wrapped cause is exposed via
// [errors.Unwrap] / [errors.Is].
func TestProtocolError_Unwrap(t *testing.T) {
	t.Parallel()
	err := &ProtocolError{Op: "dial", Err: ErrAuthFailed}
	require.ErrorIs(t, err, ErrAuthFailed)
	assert.ErrorIs(t, err, transport.ErrAuthFailed)
}

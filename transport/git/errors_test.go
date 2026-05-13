package gitt

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/hiddeco/go-ls-remote/transport"
)

// TestProtocolError_Error verifies the rendered form is
// `transport/git <op> <URL>: <cause>` with zero-valued fields elided.
// The prefix is always `transport/git` (no trailing colon); a colon
// before the cause is added only when cause is non-empty.
func TestProtocolError_Error(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  *ProtocolError
		want string
	}{
		{
			name: "all fields zero",
			err:  &ProtocolError{},
			want: "transport/git",
		},
		{
			name: "err only",
			err:  &ProtocolError{Err: errors.New("boom")},
			want: "transport/git: boom",
		},
		{
			name: "server only",
			err:  &ProtocolError{Server: "rejected"},
			want: "transport/git: rejected",
		},
		{
			name: "op + url + wrapped cause",
			err:  &ProtocolError{URL: "git://h/r", Op: "dial", Err: errors.New("x")},
			want: "transport/git: dial git://h/r: x",
		},
		{
			name: "server diagnostic with no Err",
			err:  &ProtocolError{Op: "dial", URL: "git://h/r", Server: "fatal: nope"},
			want: "transport/git: dial git://h/r: fatal: nope",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, tc.err.Error())
		})
	}
}

// TestProtocolError_Unwrap verifies [errors.Is] sees through the
// wrapper to the sentinel and through [transport.SchemeError] to the
// generic parent.
func TestProtocolError_Unwrap(t *testing.T) {
	t.Parallel()
	err := &ProtocolError{Op: "dial", Err: ErrUnsupportedProtocol}

	t.Run("matches package sentinel", func(t *testing.T) {
		t.Parallel()
		assert.ErrorIs(t, err, ErrUnsupportedProtocol)
	})
	t.Run("bridges to transport.ErrUnsupportedProtocol", func(t *testing.T) {
		t.Parallel()
		assert.ErrorIs(t, err, transport.ErrUnsupportedProtocol)
	})
}

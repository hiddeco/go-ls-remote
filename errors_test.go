package lsremote

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Each sentinel exists with the documented message. The fixture is a
// table so a missing or mis-worded message is obvious at a glance.
func TestSentinels(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"ErrNotFound", ErrNotFound, "lsremote: repository not found"},
		{"ErrAuthRequired", ErrAuthRequired, "lsremote: authentication required"},
		{"ErrAuthFailed", ErrAuthFailed, "lsremote: authentication failed"},
		{"ErrUnsupportedProtocol", ErrUnsupportedProtocol, "lsremote: protocol/operation not supported by server"},
		{"ErrServerRefused", ErrServerRefused, "lsremote: server refused"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.NotNil(t, tc.err)
			assert.Equal(t, tc.want, tc.err.Error())
		})
	}
}

func TestProtocolError_Unwrap(t *testing.T) {
	t.Run("returns the wrapped Err", func(t *testing.T) {
		want := errors.New("boom")
		pe := &ProtocolError{Op: "dial", Err: want}
		assert.Same(t, want, pe.Unwrap())
	})

	t.Run("nil Err unwraps to nil", func(t *testing.T) {
		pe := &ProtocolError{Op: "dial"}
		assert.Nil(t, pe.Unwrap())
	})
}

func TestProtocolError_ErrorsIs(t *testing.T) {
	t.Run("direct sentinel match", func(t *testing.T) {
		pe := &ProtocolError{Op: "ls-refs", Err: ErrNotFound}
		assert.True(t, errors.Is(pe, ErrNotFound))
		assert.False(t, errors.Is(pe, ErrAuthFailed))
	})

	t.Run("transitively wrapped sentinel match", func(t *testing.T) {
		inner := fmt.Errorf("transport says: %w", ErrAuthRequired)
		pe := &ProtocolError{Op: "advertisement", Err: inner}
		assert.True(t, errors.Is(pe, ErrAuthRequired))
	})

	t.Run("non-matching sentinel returns false", func(t *testing.T) {
		pe := &ProtocolError{Op: "probe", Err: ErrNotFound}
		assert.False(t, errors.Is(pe, ErrServerRefused))
	})
}

func TestProtocolError_Error(t *testing.T) {
	t.Run("includes op and wrapped error", func(t *testing.T) {
		pe := &ProtocolError{
			Op:  "ls-refs",
			URL: "https://example.test/repo.git",
			Err: ErrNotFound,
		}
		s := pe.Error()
		assert.True(t, strings.HasPrefix(s, "lsremote:"),
			"Error() output should share the sentinel prefix, got %q", s)
		assert.Contains(t, s, "ls-refs")
		assert.Contains(t, s, ErrNotFound.Error())
		assert.Contains(t, s, "https://example.test/repo.git")
	})

	t.Run("redacts credentials in URL", func(t *testing.T) {
		pe := &ProtocolError{
			Op:  "dial",
			URL: "https://alice:secret@example.test/repo.git",
			Err: ErrAuthFailed,
		}
		s := pe.Error()
		assert.NotContains(t, s, "secret",
			"the URL password must never leak into Error() output")
		assert.Contains(t, s, "***",
			"transport.RedactURL replaces the password with ***")
		assert.Contains(t, s, "alice",
			"the username should be preserved in the redacted form")
	})

	t.Run("omits status when zero", func(t *testing.T) {
		pe := &ProtocolError{
			Op:  "dial",
			URL: "https://example.test/repo.git",
			Err: ErrNotFound,
		}
		s := pe.Error()
		assert.NotContains(t, strings.ToLower(s), "status 0")
		assert.NotContains(t, strings.ToLower(s), "status")
	})

	t.Run("includes status when non-zero", func(t *testing.T) {
		pe := &ProtocolError{
			Op:     "probe",
			URL:    "https://example.test/repo.git",
			Status: 500,
			Err:    ErrServerRefused,
		}
		s := pe.Error()
		assert.Contains(t, s, "500")
		assert.Contains(t, strings.ToLower(s), "status")
	})

	t.Run("omits server section when empty", func(t *testing.T) {
		pe := &ProtocolError{
			Op:  "ls-refs",
			URL: "https://example.test/repo.git",
			Err: ErrNotFound,
		}
		assert.NotContains(t, pe.Error(), "server:")
	})

	t.Run("includes server section when present", func(t *testing.T) {
		pe := &ProtocolError{
			Op:     "ls-refs",
			URL:    "https://example.test/repo.git",
			Err:    ErrServerRefused,
			Server: "upload-pack: not our ref",
		}
		s := pe.Error()
		assert.Contains(t, s, "server:")
		assert.Contains(t, s, "upload-pack: not our ref")
	})

	t.Run("does not panic with nil Err", func(t *testing.T) {
		pe := &ProtocolError{
			Op:  "dial",
			URL: "https://example.test/repo.git",
		}
		assert.NotPanics(t, func() {
			_ = pe.Error()
		})
	})

	t.Run("does not panic with a long Server payload", func(t *testing.T) {
		pe := &ProtocolError{
			Op:     "ls-refs",
			URL:    "https://example.test/repo.git",
			Err:    ErrServerRefused,
			Server: strings.Repeat("x", 1024),
		}
		assert.NotPanics(t, func() {
			_ = pe.Error()
		})
	})
}

func TestTruncateServer(t *testing.T) {
	t.Run("leaves a short string unchanged", func(t *testing.T) {
		const in = "upload-pack: not our ref"
		assert.Equal(t, in, truncateServer(in))
	})

	t.Run("leaves an exactly-1024-byte string unchanged", func(t *testing.T) {
		in := strings.Repeat("x", 1024)
		out := truncateServer(in)
		assert.Equal(t, 1024, len(out))
		assert.Equal(t, in, out)
	})

	t.Run("truncates a longer string to 1024 bytes", func(t *testing.T) {
		in := strings.Repeat("y", 2048)
		out := truncateServer(in)
		assert.Equal(t, 1024, len(out))
		assert.Equal(t, strings.Repeat("y", 1024), out)
	})

	t.Run("empty stays empty", func(t *testing.T) {
		assert.Equal(t, "", truncateServer(""))
	})
}

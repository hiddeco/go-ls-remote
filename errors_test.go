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
	t.Parallel()
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
			t.Parallel()
			require.Error(t, tc.err)
			assert.Equal(t, tc.want, tc.err.Error())
		})
	}
}

func TestProtocolError_Unwrap(t *testing.T) {
	t.Parallel()
	t.Run("returns the wrapped Err", func(t *testing.T) {
		t.Parallel()
		want := errors.New("boom")
		pe := &ProtocolError{Op: "dial", Err: want}
		assert.Same(t, want, pe.Unwrap())
	})

	t.Run("nil Err unwraps to nil", func(t *testing.T) {
		t.Parallel()
		pe := &ProtocolError{Op: "dial"}
		assert.NoError(t, pe.Unwrap())
	})
}

func TestProtocolError_ErrorsIs(t *testing.T) {
	t.Parallel()
	t.Run("direct sentinel match", func(t *testing.T) {
		t.Parallel()
		pe := &ProtocolError{Op: "ls-refs", Err: ErrNotFound}
		require.ErrorIs(t, pe, ErrNotFound)
		assert.NotErrorIs(t, pe, ErrAuthFailed)
	})

	t.Run("transitively wrapped sentinel match", func(t *testing.T) {
		t.Parallel()
		inner := fmt.Errorf("transport says: %w", ErrAuthRequired)
		pe := &ProtocolError{Op: "advertisement", Err: inner}
		assert.ErrorIs(t, pe, ErrAuthRequired)
	})

	t.Run("non-matching sentinel returns false", func(t *testing.T) {
		t.Parallel()
		pe := &ProtocolError{Op: "probe", Err: ErrNotFound}
		assert.NotErrorIs(t, pe, ErrServerRefused)
	})
}

func TestProtocolError_Error(t *testing.T) {
	t.Parallel()
	t.Run("includes op and wrapped error", func(t *testing.T) {
		t.Parallel()
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
		t.Parallel()
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
		t.Parallel()
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
		t.Parallel()
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
		t.Parallel()
		pe := &ProtocolError{
			Op:  "ls-refs",
			URL: "https://example.test/repo.git",
			Err: ErrNotFound,
		}
		assert.NotContains(t, pe.Error(), "server:")
	})

	t.Run("includes server section when present", func(t *testing.T) {
		t.Parallel()
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
		t.Parallel()
		pe := &ProtocolError{
			Op:  "dial",
			URL: "https://example.test/repo.git",
		}
		assert.NotPanics(t, func() {
			_ = pe.Error()
		})
	})

	t.Run("does not panic with a long Server payload", func(t *testing.T) {
		t.Parallel()
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

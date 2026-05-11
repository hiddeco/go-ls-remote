package httpt

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/hiddeco/go-ls-remote/transport"
)

func TestProtocolError_Error_StatusOnly(t *testing.T) {
	err := &ProtocolError{
		URL:    "https://example.com/repo.git/info/refs?service=git-upload-pack",
		Op:     "probe",
		Status: 500,
	}

	got := err.Error()
	assert.Contains(t, got, "transport/http")
	assert.Contains(t, got, "probe")
	assert.Contains(t, got, "https://example.com/repo.git/info/refs?service=git-upload-pack")
	assert.Contains(t, got, "500")
}

func TestProtocolError_Error_WithServerMessage(t *testing.T) {
	err := &ProtocolError{
		URL:    "https://example.com/repo.git/info/refs?service=git-upload-pack",
		Op:     "probe",
		Status: 500,
		Server: "internal explosion",
	}

	got := err.Error()
	assert.Contains(t, got, "internal explosion",
		"server-supplied message should appear in the formatted error")
}

func TestProtocolError_Error_WithWrappedErr(t *testing.T) {
	cause := errors.New("malformed preamble")
	err := &ProtocolError{
		URL:    "https://example.com/repo.git/info/refs?service=git-upload-pack",
		Op:     "probe",
		Status: 200,
		Err:    cause,
	}

	got := err.Error()
	assert.Contains(t, got, "malformed preamble")
}

func TestProtocolError_Unwrap(t *testing.T) {
	cause := errors.New("boom")
	err := &ProtocolError{Err: cause}

	assert.Same(t, cause, err.Unwrap(),
		"Unwrap returns the wrapped cause for errors.Is/As")
	assert.True(t, errors.Is(err, cause))
}

func TestProtocolError_Error_OmitsZeroStatus(t *testing.T) {
	err := &ProtocolError{
		URL: "https://example.com/repo.git/info/refs?service=git-upload-pack",
		Op:  "probe",
		Err: errors.New("connect: refused"),
	}

	got := err.Error()
	assert.NotContains(t, got, "HTTP 0",
		"a zero Status means pre-response; the formatter should not lie about an HTTP code")
	assert.Contains(t, got, "connect: refused")
}

func TestSentinels_Distinct(t *testing.T) {
	// All four sentinels share the `transport/http:` prefix but must be
	// distinguishable via errors.Is so callers can branch on them.
	all := []error{ErrAuthRequired, ErrAuthFailed, ErrNotFound, ErrUnsupportedProtocol}
	for i, a := range all {
		for j, b := range all {
			if i == j {
				continue
			}
			assert.False(t, errors.Is(a, b),
				"sentinels at indices %d and %d must not match", i, j)
		}
	}
}

func TestSentinels_PrefixedWithPackage(t *testing.T) {
	for _, e := range []error{ErrAuthRequired, ErrAuthFailed, ErrNotFound, ErrUnsupportedProtocol} {
		assert.True(t, strings.HasPrefix(e.Error(), "transport/http:"),
			"sentinel %q must carry the package prefix for grep-friendly logs", e)
	}
}

func TestSentinels_BridgeToTransport(t *testing.T) {
	tests := []struct {
		name    string
		scheme  error
		generic error
	}{
		{
			name:    "ErrAuthRequired bridges to transport.ErrAuthRequired",
			scheme:  ErrAuthRequired,
			generic: transport.ErrAuthRequired,
		},
		{
			name:    "ErrAuthFailed bridges to transport.ErrAuthFailed",
			scheme:  ErrAuthFailed,
			generic: transport.ErrAuthFailed,
		},
		{
			name:    "ErrNotFound bridges to transport.ErrNotFound",
			scheme:  ErrNotFound,
			generic: transport.ErrNotFound,
		},
		{
			name:    "ErrUnsupportedProtocol bridges to transport.ErrUnsupportedProtocol",
			scheme:  ErrUnsupportedProtocol,
			generic: transport.ErrUnsupportedProtocol,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.True(t, errors.Is(tt.scheme, tt.generic),
				"errors.Is(%v, %v) must be true", tt.scheme, tt.generic)
		})
	}

	// Negative: no cross-pollination between distinct sentinels.
	assert.False(t, errors.Is(ErrNotFound, transport.ErrAuthRequired),
		"ErrNotFound must not match transport.ErrAuthRequired")
}

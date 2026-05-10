package httpt

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
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
	// All three sentinels share the `transport/http:` prefix but must be
	// distinguishable via errors.Is so callers can branch on them.
	all := []error{ErrAuthRequired, ErrAuthFailed, ErrNotFound}
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
	for _, e := range []error{ErrAuthRequired, ErrAuthFailed, ErrNotFound} {
		assert.True(t, strings.HasPrefix(e.Error(), "transport/http:"),
			"sentinel %q must carry the package prefix for grep-friendly logs", e)
	}
}

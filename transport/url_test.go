package transport

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want URL // Raw is filled in by the assertion from in.
	}{
		{
			name: "https no port",
			in:   "https://github.com/torvalds/linux.git",
			want: URL{Scheme: "https", Host: "github.com", Path: "/torvalds/linux.git"},
		},
		{
			name: "http with port",
			in:   "http://example.com:8080/repo",
			want: URL{Scheme: "http", Host: "example.com", Port: "8080", Path: "/repo"},
		},
		{
			name: "https with userinfo",
			in:   "https://alice:secret@example.com/repo.git",
			want: URL{Scheme: "https", User: "alice:secret", Host: "example.com", Path: "/repo.git"},
		},
		{
			name: "https with port and userinfo",
			in:   "https://user@example.com:443/repo",
			want: URL{Scheme: "https", User: "user", Host: "example.com", Port: "443", Path: "/repo"},
		},
		{
			name: "ssh RFC form",
			in:   "ssh://git@example.com:2222/repo.git",
			want: URL{Scheme: "ssh", User: "git", Host: "example.com", Port: "2222", Path: "/repo.git"},
		},
		{
			name: "git daemon",
			in:   "git://example.com:9418/repo.git",
			want: URL{Scheme: "git", Host: "example.com", Port: "9418", Path: "/repo.git"},
		},
		{
			name: "scheme uppercased is normalised",
			in:   "HTTPS://example.com/repo",
			want: URL{Scheme: "https", Host: "example.com", Path: "/repo"},
		},
		{
			name: "scp-style with user",
			in:   "git@github.com:torvalds/linux.git",
			want: URL{Scheme: "ssh", User: "git", Host: "github.com", Path: "/torvalds/linux.git"},
		},
		{
			name: "scp-style without user",
			in:   "github.com:torvalds/linux.git",
			want: URL{Scheme: "ssh", Host: "github.com", Path: "/torvalds/linux.git"},
		},
		{
			name: "scp-style with bracketed IPv6",
			in:   "git@[fe80::1]:repo.git",
			want: URL{Scheme: "ssh", User: "git", Host: "fe80::1", Path: "/repo.git"},
		},
		{
			name: "scp-style ignores apparent port (canonical Git behaviour)",
			// `host:NN:rest` parses as host=`host`, path=`NN:rest` per
			// connect.c::parse_connect_url; scp-style has no port.
			in:   "git@host:22:repo.git",
			want: URL{Scheme: "ssh", User: "git", Host: "host", Path: "/22:repo.git"},
		},
		{
			name: "file scheme absolute",
			in:   "file:///srv/repos/foo.git",
			want: URL{Scheme: "file", Path: "/srv/repos/foo.git"},
		},
		{
			name: "bare absolute path",
			in:   "/srv/repos/foo.git",
			want: URL{Scheme: "file", Path: "/srv/repos/foo.git"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseURL(tt.in)
			require.NoError(t, err)
			want := tt.want
			want.Raw = tt.in
			assert.Equal(t, &want, got)
		})
	}
}

// TestParseURL_RedactsCredentialsInErrors verifies that ParseURL never
// leaks a password into its error text. Each case supplies a URL with
// userinfo "alice:secret" and asserts that "secret" is absent from the
// rendered error while "alice" and the redaction marker "***" are present.
// Wrapping with fmt.Errorf("%w: %q", sentinel, RedactURL(s)) preserves
// the sentinel chain, so errors.Is still matches.
func TestParseURL_RedactsCredentialsInErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		in      string
		wantErr error
	}{
		{
			// Unterminated IPv6 bracket; userinfo contains a password.
			name:    "invalid IPv6 — unterminated bracket",
			in:      "https://alice:secret@[::1/repo.git",
			wantErr: ErrInvalidIPv6,
		},
		{
			// IPv6 bracket closed but followed by junk (no colon before port).
			name:    "invalid IPv6 — junk after bracket",
			in:      "https://alice:secret@[::1]junk/repo.git",
			wantErr: ErrInvalidIPv6,
		},
		{
			// Authority-branch missing-host: "https://alice:secret@/repo"
			// passes scheme detection but yields an empty host.
			name:    "missing host — authority branch",
			in:      "https://alice:secret@/repo.git",
			wantErr: ErrMissingHost,
		},
		{
			// Authority-branch missing-host, port-only authority:
			// "https://alice:secret@:8080/repo" parses scheme, strips
			// userinfo, then finds host="" before the colon.
			name:    "missing host — port-only authority",
			in:      "https://alice:secret@:8080/repo.git",
			wantErr: ErrMissingHost,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseURL(tt.in)
			require.Error(t, err)
			require.ErrorIs(t, err, tt.wantErr)
			msg := err.Error()
			assert.NotContains(t, msg, "secret",
				"error text must not contain the password")
			assert.Contains(t, msg, "***",
				"error text must contain redaction marker")
			assert.Contains(t, msg, "alice",
				"error text must preserve the username")
		})
	}
}

func TestParseURL_errors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want error
	}{
		{"empty input", "", ErrEmptyURL},
		{"unsupported scheme", "svn://example.com/repo", ErrUnsupportedScheme},
		{"unknown form (no scheme, no slash)", "garbage", ErrUnrecognizedURL},
		{"relative path", "relative/path", ErrUnrecognizedURL},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseURL(tt.in)
			require.ErrorIs(t, err, tt.want)
		})
	}
}

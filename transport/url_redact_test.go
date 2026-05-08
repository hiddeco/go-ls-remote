package transport

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRedactURL(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "no userinfo is unchanged",
			in:   "https://example.com/repo",
			want: "https://example.com/repo",
		},
		{
			name: "user and password redacts password only",
			in:   "https://alice:secret@example.com/repo",
			want: "https://alice:***@example.com/repo",
		},
		{
			name: "user only is unchanged",
			in:   "https://alice@example.com/repo",
			want: "https://alice@example.com/repo",
		},
		{
			name: "ssh user is unchanged",
			in:   "ssh://git@host/repo",
			want: "ssh://git@host/repo",
		},
		{
			name: "scp-style is unchanged (no scheme)",
			in:   "git@github.com:owner/repo.git",
			want: "git@github.com:owner/repo.git",
		},
		{
			name: "garbage is unchanged",
			in:   "not a url",
			want: "not a url",
		},
		{
			name: "@ in path is not treated as userinfo",
			in:   "https://example.com/path/with@symbol",
			want: "https://example.com/path/with@symbol",
		},
		{
			name: "user with colon and password preserved before slash",
			in:   "https://user:p%40ss@example.com:443/repo",
			want: "https://user:***@example.com:443/repo",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, RedactURL(tt.in))
		})
	}
}

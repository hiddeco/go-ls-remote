package filet

import (
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestWindowsURLPathToOSPath(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "forward-slash drive letter (RFC 8089 canonical)",
			in:   "/C:/Users/runner/repo/.git",
			want: `C:\Users\runner\repo\.git`,
		},
		{
			name: "backslash drive letter (loose form canonical Git accepts)",
			in:   `/C:\Users\runner\repo\.git`,
			want: `C:\Users\runner\repo\.git`,
		},
		{
			name: "lowercase drive letter accepted",
			in:   "/d:/work/repo",
			want: `d:\work\repo`,
		},
		{
			name: "non-drive-letter absolute path — slashes converted, leading slash kept",
			in:   "/tmp/repo",
			want: `\tmp\repo`,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := windowsURLPathToOSPath(c.in)
			assert.Equal(t, c.want, got)
		})
	}
}

func TestURLPathToOSPath_IdentityOnNonWindows(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("identity transform only on non-Windows")
	}
	got := urlPathToOSPath("/tmp/repo/.git")
	assert.Equal(t, "/tmp/repo/.git", got)
}

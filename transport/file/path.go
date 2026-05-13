package filet

import (
	"runtime"
	"strings"
)

// urlPathToOSPath converts a slash-separated URL path, as produced by
// `transport.ParseURL`, into an OS-native path suitable for
// `objstore.Open`. On non-Windows hosts the path is already in OS form
// and is returned unchanged.
//
// On Windows the leading `/` that `ParseURL` prepends to every
// `file://` URL is stripped when it precedes a drive letter, and any
// `/` separators are folded to `\` so the result matches the shape
// `os.Stat` expects. Canonical Git accepts both `file:///C:/repo` and
// `file://C:\repo` (see `connect.c::url_is_local_not_ssh` at
// `connect.c:710`); both forms come out as `C:\repo` after this
// transform.
//
// [connect.c:710]: https://github.com/git/git/blob/v2.54.0/connect.c#L710
func urlPathToOSPath(urlPath string) string {
	if runtime.GOOS != "windows" {
		return urlPath
	}
	return windowsURLPathToOSPath(urlPath)
}

// windowsURLPathToOSPath is the Windows-specific transform. It is
// exposed package-internally so the table-test in `path_test.go`
// exercises every branch from a non-Windows developer host —
// `filepath.FromSlash` would be a no-op off Windows, so the
// `/` → `\` fold is done explicitly here.
//
// The transform only handles the one shape `transport.ParseURL`
// emits: a slash-prefixed path. Inputs that do not start with `/`
// are not produced by the parser and have their slashes folded
// unchanged.
func windowsURLPathToOSPath(urlPath string) string {
	rest := urlPath
	if len(rest) >= 3 && rest[0] == '/' && isDriveLetter(rest[1]) && rest[2] == ':' {
		rest = rest[1:]
	}
	return strings.ReplaceAll(rest, "/", `\`)
}

func isDriveLetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

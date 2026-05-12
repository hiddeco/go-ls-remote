//go:build !windows

package httpt

import (
	"bytes"
	"context"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNetrc_Resolve_WorldWritableSymlinkWarns pins that a netrc
// reached via a world-writable symlink emits the loose-permissions
// warning even when the link target itself is mode 0600. `os.Stat`
// follows symlinks and would see the safe target mode; `os.Lstat`
// sees the link's own mode, which is the surface an attacker
// substituting the link controls.
func TestNetrc_Resolve_WorldWritableSymlinkWarns(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "target.netrc")
	require.NoError(t, os.WriteFile(target, []byte("machine example.com login a password b\n"), 0o600))

	link := filepath.Join(dir, ".netrc")
	require.NoError(t, os.Symlink(target, link))

	// lchmod(2) is absent on modern Darwin and unsupported on Linux.
	// The call below will fail at runtime; we skip gracefully so the
	// test does not become a noisy false-negative on those platforms.
	pathp, err := syscall.BytePtrFromString(link)
	if err != nil {
		t.Skipf("could not encode symlink path: %v", err)
	}
	// syscall number 274 is lchmod on FreeBSD/NetBSD; Darwin removed it.
	// On every supported platform this returns an error, triggering the
	// skip — the test is kept so it can be enabled on a platform where
	// lchmod works (e.g. FreeBSD) without source changes.
	if _, _, errno := syscall.Syscall(274, uintptr(unsafe.Pointer(pathp)), 0o777, 0); errno != 0 {
		t.Skipf("lchmod not available on this platform: %v", errno)
	}

	var buf bytes.Buffer
	resolver := newNetrcResolver(link, &buf)

	u, err2 := url.Parse("https://example.com/repo")
	require.NoError(t, err2)
	_, err2 = resolver.Resolve(context.Background(), u)
	require.NoError(t, err2)

	assert.Contains(t, buf.String(), "readable or writable by group or world",
		"a world-writable symlink must emit the loose-permissions warning regardless of target mode")
}

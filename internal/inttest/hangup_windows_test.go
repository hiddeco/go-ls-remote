//go:build windows

package inttest

import (
	"fmt"
	"net"
	"os"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestIsClientHangup_WSAErrnos pins the Windows-only matcher for the
// two Winsock errnos the runtime surfaces during a peer-initiated
// connection teardown:
//
//   - WSAECONNRESET (`wsarecv: An existing connection was forcibly
//     closed by the remote host.`)
//   - WSAECONNABORTED (`wsasend: An established connection was
//     aborted by the software in your host machine.`)
//
// Both arose on `windows-latest` from `TestNewSSHServer_*` and
// `TestNewGitServer_*` when clients dropped mid-handshake; the
// generic `errors.Is(err, syscall.ECONNRESET)` check missed them
// because `syscall.ECONNRESET` resolves to an
// `APPLICATION_ERROR + offset` value on Windows that does not
// equal the WSA errno the net package returns.
func TestIsClientHangup_WSAErrnos(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		syscall string
		errno   syscall.Errno
	}{
		{"WSAECONNRESET via wsarecv", "wsarecv", syscall.WSAECONNRESET},
		{"WSAECONNABORTED via wsasend", "wsasend", syscall.WSAECONNABORTED},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Mirror the wrap chain the Go runtime emits for a
			// real socket error: `fmt.Errorf` (server-side context)
			// wrapping `*net.OpError` wrapping `*os.SyscallError`
			// wrapping the WSA errno.
			err := fmt.Errorf("server: read v2 request: %w", &net.OpError{
				Op:  "read",
				Net: "tcp",
				Err: &os.SyscallError{Syscall: tc.syscall, Err: tc.errno},
			})

			assert.True(t, isClientHangupError(err),
				"SSH harness matcher must recognise %v", tc.errno)
			assert.True(t, isGitClientHangupError(err),
				"git harness matcher must recognise %v", tc.errno)
			assert.True(t, isPlatformHangup(err),
				"platform helper must recognise %v", tc.errno)
		})
	}
}

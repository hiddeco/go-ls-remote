//go:build !windows

package inttest

// isPlatformHangup is a Windows-only matcher; on POSIX the
// canonical `syscall.ECONNRESET` and `syscall.EPIPE` shapes the
// generic detector already checks cover every hangup the runtime
// emits, so this stub returns false.
func isPlatformHangup(error) bool { return false }

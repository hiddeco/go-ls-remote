//go:build windows

package inttest

import (
	"errors"
	"syscall"
)

// isPlatformHangup reports whether err carries a Winsock errno the
// runtime emits when a peer tears the connection down: WSAECONNRESET
// on a `wsarecv` after the peer closes hard, WSAECONNABORTED on a
// `wsasend` after the local stack gives up on a connection the peer
// never drained.
//
// The matcher cannot rely on `errors.Is(err, syscall.ECONNRESET)`
// here: the POSIX-named `syscall.ECONNRESET` and
// `syscall.ECONNABORTED` constants are invented `APPLICATION_ERROR +
// offset` values on Windows ([zerrors_windows.go]) that never equal
// the WSA errno values the net package surfaces.
//
// [zerrors_windows.go]: https://github.com/golang/go/blob/go1.26.3/src/syscall/zerrors_windows.go#L38-L40
func isPlatformHangup(err error) bool {
	return errors.Is(err, syscall.WSAECONNRESET) ||
		errors.Is(err, syscall.WSAECONNABORTED)
}

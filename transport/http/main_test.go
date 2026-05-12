package httpt

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain installs the package-wide goroutine-leak guard.
// `transport/http` manages response-body draining, in-flight body
// tracking on `*Conn`, redirect-policy callbacks, and the 401
// re-probe — every one of those paths owns goroutine-adjacent
// resources whose teardown a regression could skip.
//
// The `net/http` package spawns idle persistConn read/write loops
// that outlive any single request and are reclaimed asynchronously
// on `Transport.CloseIdleConnections`. The exemptions below cover
// that stdlib-internal noise so the harness flags only leaks the
// library can act on. Add a function-specific ignore only after
// confirming the goroutine is genuinely independent of test
// teardown.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		goleak.IgnoreTopFunction("net/http.(*persistConn).readLoop"),
		goleak.IgnoreTopFunction("net/http.(*persistConn).writeLoop"),
		goleak.IgnoreAnyFunction("internal/poll.runtime_pollWait"),
	)
}

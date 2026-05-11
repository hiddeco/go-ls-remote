package ssht

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain installs the package-wide goroutine-leak guard. The agent
// path is the load-bearing leak surface for this package: [Agent]'s
// `Resolve` opens a Unix-socket conn to `$SSH_AUTH_SOCK` that lives
// for the duration of the SSH publickey handshake, and the in-process
// `agent.Keyring` server used in tests spawns one `agent.ServeAgent`
// goroutine per accepted connection. Both must be released after each
// test; `goleak.VerifyTestMain` catches regressions in either the
// resolver's cleanup hook or `startAgentServer`'s teardown.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

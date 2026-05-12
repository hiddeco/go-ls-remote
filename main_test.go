package lsremote

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain installs the package-wide goroutine-leak guard for the
// root `lsremote` package. The integration tests in
// `inttest_*_test.go` stand up real transport harnesses; a leak
// originating in the root-package iterator wrappers (see
// `helpers.go::Refs`) or the session lifecycle (see
// `session.go::refsV2`'s deferred drain) is exactly what
// `VerifyTestMain` catches here.
//
// The exemption list mirrors the per-transport harnesses
// (`transport/http`, `transport/file`, `transport/ssh`): stdlib
// `net/http` persistConn background loops and the runtime poller
// are exempt. Add a function-specific ignore only after
// confirming the goroutine is genuinely independent of test
// teardown.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		goleak.IgnoreTopFunction("net/http.(*persistConn).readLoop"),
		goleak.IgnoreTopFunction("net/http.(*persistConn).writeLoop"),
		goleak.IgnoreAnyFunction("internal/poll.runtime_pollWait"),
	)
}

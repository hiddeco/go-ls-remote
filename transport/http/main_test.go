package httpt

import (
	"net/http"
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

// newTestTransport returns a [*Transport] configured exactly like
// [New], but with an [http.Client] backed by a private
// [http.Transport] pinned to t. Sharing `http.DefaultClient` across
// parallel tests in this package is unsafe: `httptest.Server.Close`
// documentedly calls `http.DefaultTransport.CloseIdleConnections` as
// a courtesy (`net/http/httptest/server.go`, the "help them out and
// close any idle connections" block), which closes persistConns
// belonging to siblings that share the default transport. The
// victim's in-flight request surfaces as `net/http: HTTP/1.x
// transport connection broken: http: CloseIdleConnections called` —
// the `errCloseIdleConns` sentinel only escapes `Transport.RoundTrip`
// via that helper, so the diagnosis is unambiguous.
//
// The private transport is closed via `t.Cleanup` so the goroutine
// guard above sees no orphaned idle conns at process exit. Caller
// options are appended after the injected `WithClient` so a test
// that explicitly passes its own client (e.g. for option-ordering
// assertions) still wins.
func newTestTransport(t *testing.T, opts ...Option) *Transport {
	t.Helper()
	rt := &http.Transport{}
	t.Cleanup(rt.CloseIdleConnections)
	full := make([]Option, 0, len(opts)+1)
	full = append(full, WithClient(&http.Client{Transport: rt}))
	full = append(full, opts...)
	return New(full...)
}

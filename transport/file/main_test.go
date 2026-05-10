package filet

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain installs the package-wide goroutine-leak guard. The file
// transport spawns an in-process server goroutine for the lifetime of
// each [Conn] (see [Conn.Close] and the dispatch loop it tears down),
// which is the load-bearing leak surface for this package: any test
// that opens a [Conn] and forgets to close it — or any regression that
// makes [Conn.Close] return without joining the goroutine — is exactly
// what `goleak.VerifyTestMain` will catch. Running the check here once
// per package run is the canonical Go-community shape and replaces the
// per-test `runtime.NumGoroutine` polling that preceded it, which was
// racy under `go test -parallel N`.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// Package inttest is the integration-test support layer that wires
// fixture repositories to in-process transports.
//
// The package exposes a curated [Entry] matrix — names, declared ref
// sets, and a small number of sample `object-info` results — together
// with helpers that materialise each fixture into a fresh
// `t.TempDir()` and open it through `internal/objstore`. Each transport
// (HTTP, SSH, git daemon, local) reuses the same matrix so the
// end-to-end tests can iterate one source of truth and assert that
// every transport reports the same answers.
//
// inttest is test-support code only. Nothing under it is imported from
// production paths; the package lives here, rather than as `_test.go`
// helpers next to a single test file, because the harness types must
// be shared across packages that test different transports. The
// `internal/` placement signals this: the harnesses and matrix are
// consumed exclusively by the module's own `_test.go` files and the
// import graph forbids any production caller.
package inttest

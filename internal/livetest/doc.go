//go:build live

// Package livetest exercises the library end-to-end against real Git
// hosting providers (GitHub, GitLab, and similar) over their public
// network endpoints.
//
// The tests here are slow, network-dependent, and observe state owned
// by third parties, so they must not run as part of the default unit
// test suite. Every file in this package carries a `//go:build live`
// constraint and the package is reached only via the explicit
// invocation:
//
//	go test -tags=live ./internal/livetest/...
//
// A separate untagged stub keeps the directory non-empty under default
// build flags so `go vet ./...` still covers it.
package livetest

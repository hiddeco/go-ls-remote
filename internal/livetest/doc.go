//go:build live

// Package livetest exercises the library end-to-end against real Git
// hosting providers — GitHub, GitLab, Codeberg, Bitbucket, and Gitea —
// over their public network endpoints.
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
//
// The exposed tests are [TestTags], [TestDefaultBranch], [TestObjectInfo],
// and [TestProtocolVersion]. The first three pivot on the dynamic
// auth-mode matrix built by [Provider.authModes], iterated by
// [forEachProviderMode]: a `none` row runs against the unauthenticated
// HTTPS endpoint, and additional credentialed rows (`https-token`,
// `https-token-private`, `ssh-key`, `ssh-agent`) appear when the
// corresponding `LSREMOTE_<NAME>_*` env vars are set.
// [TestProtocolVersion] bypasses the matrix and asserts each provider
// negotiates v2 over its public HTTPS endpoint.
package livetest

package transport

import "testing"

// BenchmarkRedactURL pins the cost of the diagnostic redaction helper
// the transport packages run on every URL they surface. A session
// under tracing emits one [trace.HTTPEvent] per round-trip and one
// [trace.PacketEvent] per outbound pkt-line, both of which carry a
// redacted URL produced by this function; in steady state that puts
// `RedactURL` on a per-pkt-line hot path.
//
// Sub-benches exercise the four shapes the function dispatches on:
//
//   - `no-userinfo`: every probe URL passes through this arm
//     regardless of credential configuration. It is the
//     single-pass, allocation-free fast path and the most common
//     input shape under measurement.
//   - `password`: the redaction case — userinfo present, password
//     present. Allocates one rebuilt string.
//   - `unencoded-at`: forces the [strings.LastIndexByte] traversal
//     across an authority that contains an embedded `@` (technically
//     a syntax violation but observed in real inputs).
//   - `non-rfc`: scp-style without `://`, exercising the early-exit
//     branch.
//
// Inputs are constants; the timed loop measures the redaction body
// only.
func BenchmarkRedactURL(b *testing.B) {
	cases := []struct {
		name, in string
	}{
		{"no-userinfo", "https://github.com/foo/bar.git/info/refs?service=git-upload-pack"},
		{"password", "https://alice:secret@example.com:8443/repo.git/info/refs?service=git-upload-pack"},
		{"unencoded-at", "https://alice:p@ssword@example.com/repo.git/info/refs?service=git-upload-pack"},
		{"non-rfc", "git@github.com:owner/repo.git"},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				_ = RedactURL(tc.in)
			}
		})
	}
}

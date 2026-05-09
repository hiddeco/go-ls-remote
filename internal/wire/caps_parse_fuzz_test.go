package wire

import "testing"

// FuzzParseCapabilities exercises [ParseCapabilities] with arbitrary
// strings. The contract under test is the parser's totality: it must
// not panic on any input. ParseCapabilities returns no error by design
// (matching canonical Git's `parse_feature_value`), so this target
// relies on Go's fuzz engine to catch panics rather than asserting
// output invariants. Seeds cover the boolean and `name=value` shapes,
// a multi-symref-style input, and a whitespace-mix payload.
func FuzzParseCapabilities(f *testing.F) {
	seeds := []string{
		"",
		"thin-pack",
		"agent=git/2.45.0",
		"symref=HEAD:refs/heads/main symref=refs/remotes/origin/HEAD:refs/heads/main",
		" \tthin-pack \nagent=git\nls-refs=unborn ",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, in string) {
		_ = ParseCapabilities(in)
	})
}

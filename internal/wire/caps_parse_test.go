package wire

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseCapabilities(t *testing.T) {
	t.Parallel()
	t.Run("empty input yields empty slice", func(t *testing.T) {
		t.Parallel()
		assert.Empty(t, ParseCapabilities(""))
	})

	t.Run("whitespace-only input yields empty slice", func(t *testing.T) {
		t.Parallel()
		assert.Empty(t, ParseCapabilities("   \t\n  "))
	})

	t.Run("single boolean cap", func(t *testing.T) {
		t.Parallel()
		got := ParseCapabilities("multi_ack")
		assert.Equal(t, RawCapabilities{
			{Name: "multi_ack", Value: ""},
		}, got)
	})

	t.Run("single name=value cap", func(t *testing.T) {
		t.Parallel()
		got := ParseCapabilities("agent=git/2.45.0")
		assert.Equal(t, RawCapabilities{
			{Name: "agent", Value: "git/2.45.0"},
		}, got)
	})

	t.Run("mixed boolean and value caps preserve order", func(t *testing.T) {
		t.Parallel()
		got := ParseCapabilities("multi_ack thin-pack agent=git/2.45.0 ofs-delta")
		assert.Equal(t, RawCapabilities{
			{Name: "multi_ack"},
			{Name: "thin-pack"},
			{Name: "agent", Value: "git/2.45.0"},
			{Name: "ofs-delta"},
		}, got)
	})

	t.Run("duplicate name=value caps preserve order", func(t *testing.T) {
		t.Parallel()
		got := ParseCapabilities("symref=HEAD:refs/heads/main symref=ORIG_HEAD:refs/heads/main")
		assert.Equal(t, RawCapabilities{
			{Name: "symref", Value: "HEAD:refs/heads/main"},
			{Name: "symref", Value: "ORIG_HEAD:refs/heads/main"},
		}, got)
	})

	t.Run("empty value after equals is preserved", func(t *testing.T) {
		t.Parallel()
		got := ParseCapabilities("object-format=")
		assert.Equal(t, RawCapabilities{
			{Name: "object-format", Value: ""},
		}, got)
	})

	t.Run("leading and trailing whitespace are skipped", func(t *testing.T) {
		t.Parallel()
		got := ParseCapabilities("  multi_ack agent=git/2.45.0  ")
		assert.Equal(t, RawCapabilities{
			{Name: "multi_ack"},
			{Name: "agent", Value: "git/2.45.0"},
		}, got)
	})

	t.Run("multiple internal whitespace runs collapse", func(t *testing.T) {
		t.Parallel()
		got := ParseCapabilities("multi_ack \t\n agent=git/2.45.0\n\nthin-pack")
		assert.Equal(t, RawCapabilities{
			{Name: "multi_ack"},
			{Name: "agent", Value: "git/2.45.0"},
			{Name: "thin-pack"},
		}, got)
	})

	t.Run("substring guard: searching for multi does not match multi_ack", func(t *testing.T) {
		t.Parallel()
		// canonical Git's `parse_feature_value` ([connect.c:614-659]) guards
		// against a substring of one feature matching the prefix of another.
		// The slice-based parser tokenises rather than substring-searching,
		// so the guard is automatic: a search for `multi` finds nothing.
		//
		// [connect.c:614-659]: https://github.com/git/git/blob/v2.54.0/connect.c#L614-L659
		caps := ParseCapabilities("multi_ack thin-pack")
		_, ok := caps.Get("multi")
		assert.False(t, ok, "Get(multi) should not match multi_ack")
		assert.False(t, caps.Has("multi"))
		assert.Empty(t, caps.All("multi"))
	})

	t.Run("equals sign in value is preserved", func(t *testing.T) {
		t.Parallel()
		// Only the first `=` separates name from value; subsequent ones
		// belong to the value. This matches canonical's `*value == '='`
		// check at [connect.c:640] which fires only on the first occurrence.
		//
		// [connect.c:640]: https://github.com/git/git/blob/v2.54.0/connect.c#L640
		got := ParseCapabilities("foo=a=b=c")
		assert.Equal(t, RawCapabilities{
			{Name: "foo", Value: "a=b=c"},
		}, got)
	})

	t.Run("name with leading equals is treated as empty-name boolean", func(t *testing.T) {
		t.Parallel()
		// Canonical Git would not emit such a token; we accept it as a
		// degenerate boolean so the parser stays total over byte input.
		got := ParseCapabilities("=value")
		assert.Equal(t, RawCapabilities{
			{Name: "", Value: "value"},
		}, got)
	})
}

func TestRawCapabilities_Get(t *testing.T) {
	t.Parallel()
	caps := ParseCapabilities("multi_ack agent=git/2.45.0 symref=HEAD:refs/heads/main symref=ORIG_HEAD:refs/heads/main")

	t.Run("returns first match for repeated name", func(t *testing.T) {
		t.Parallel()
		v, ok := caps.Get("symref")
		assert.True(t, ok)
		assert.Equal(t, "HEAD:refs/heads/main", v)
	})

	t.Run("returns value for unique name", func(t *testing.T) {
		t.Parallel()
		v, ok := caps.Get("agent")
		assert.True(t, ok)
		assert.Equal(t, "git/2.45.0", v)
	})

	t.Run("returns empty value for boolean cap", func(t *testing.T) {
		t.Parallel()
		v, ok := caps.Get("multi_ack")
		assert.True(t, ok)
		assert.Equal(t, "", v)
	})

	t.Run("returns false for missing name", func(t *testing.T) {
		t.Parallel()
		v, ok := caps.Get("nope")
		assert.False(t, ok)
		assert.Equal(t, "", v)
	})

	t.Run("nil receiver yields not found", func(t *testing.T) {
		t.Parallel()
		var caps RawCapabilities
		v, ok := caps.Get("anything")
		assert.False(t, ok)
		assert.Equal(t, "", v)
	})
}

func TestRawCapabilities_All(t *testing.T) {
	t.Parallel()
	caps := ParseCapabilities("symref=HEAD:refs/heads/main agent=git/2.45.0 symref=ORIG_HEAD:refs/heads/main")

	t.Run("returns every value in encounter order", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, []string{
			"HEAD:refs/heads/main",
			"ORIG_HEAD:refs/heads/main",
		}, caps.All("symref"))
	})

	t.Run("returns single-element slice for unique name", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, []string{"git/2.45.0"}, caps.All("agent"))
	})

	t.Run("returns nil for missing name", func(t *testing.T) {
		t.Parallel()
		assert.Nil(t, caps.All("nope"))
	})

	t.Run("nil receiver yields nil", func(t *testing.T) {
		t.Parallel()
		var caps RawCapabilities
		assert.Nil(t, caps.All("anything"))
	})
}

func TestRawCapabilities_Has(t *testing.T) {
	t.Parallel()
	caps := ParseCapabilities("multi_ack agent=git/2.45.0 object-format=")

	t.Run("reports true for boolean cap", func(t *testing.T) {
		t.Parallel()
		assert.True(t, caps.Has("multi_ack"))
	})

	t.Run("reports true for name=value cap", func(t *testing.T) {
		t.Parallel()
		assert.True(t, caps.Has("agent"))
	})

	t.Run("reports true for empty-value cap", func(t *testing.T) {
		t.Parallel()
		assert.True(t, caps.Has("object-format"))
	})

	t.Run("reports false for missing cap", func(t *testing.T) {
		t.Parallel()
		assert.False(t, caps.Has("nope"))
	})

	t.Run("nil receiver reports false", func(t *testing.T) {
		t.Parallel()
		var caps RawCapabilities
		assert.False(t, caps.Has("anything"))
	})
}

func TestRawCapabilities_Names(t *testing.T) {
	t.Parallel()
	t.Run("returns names in encounter order including duplicates", func(t *testing.T) {
		t.Parallel()
		caps := ParseCapabilities("multi_ack symref=HEAD:refs/heads/main agent=git/2.45.0 symref=ORIG_HEAD:refs/heads/main")
		assert.Equal(t, []string{"multi_ack", "symref", "agent", "symref"}, caps.Names())
	})

	t.Run("empty input yields empty slice", func(t *testing.T) {
		t.Parallel()
		caps := ParseCapabilities("")
		assert.Empty(t, caps.Names())
	})

	t.Run("nil receiver yields nil", func(t *testing.T) {
		t.Parallel()
		var caps RawCapabilities
		assert.Nil(t, caps.Names())
	})
}

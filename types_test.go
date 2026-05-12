package lsremote

import (
	"testing"

	"github.com/hiddeco/go-ls-remote/transport"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRef(t *testing.T) {
	t.Run("zero value", func(t *testing.T) {
		var r Ref
		assert.Equal(t, "", r.Name)
		assert.Equal(t, "", r.Hash)
		assert.Equal(t, "", r.Peeled)
		assert.Equal(t, "", r.Symref)
	})

	t.Run("populated symref with peeled tag", func(t *testing.T) {
		r := Ref{
			Name:   "refs/tags/v1.0.0",
			Hash:   "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Peeled: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		}
		assert.Equal(t, "refs/tags/v1.0.0", r.Name)
		assert.Equal(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", r.Hash)
		assert.Equal(t, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", r.Peeled)
		assert.Equal(t, "", r.Symref)
	})

	t.Run("populated HEAD symref", func(t *testing.T) {
		r := Ref{
			Name:   "HEAD",
			Hash:   "cccccccccccccccccccccccccccccccccccccccc",
			Symref: "refs/heads/main",
		}
		assert.Equal(t, "HEAD", r.Name)
		assert.Equal(t, "cccccccccccccccccccccccccccccccccccccccc", r.Hash)
		assert.Equal(t, "refs/heads/main", r.Symref)
		assert.Equal(t, "", r.Peeled)
	})
}

func TestObjectInfo(t *testing.T) {
	t.Run("zero value", func(t *testing.T) {
		var o ObjectInfo
		assert.Equal(t, "", o.Hash)
		assert.Equal(t, int64(0), o.Size)
	})

	t.Run("size sentinel for not requested", func(t *testing.T) {
		o := ObjectInfo{
			Hash: "dddddddddddddddddddddddddddddddddddddddd",
			Size: -1,
		}
		assert.Equal(t, "dddddddddddddddddddddddddddddddddddddddd", o.Hash)
		assert.Equal(t, int64(-1), o.Size,
			"Size of -1 indicates the field was not requested or not returned")
	})
}

func TestSymref(t *testing.T) {
	t.Run("zero value", func(t *testing.T) {
		var s Symref
		assert.Equal(t, "", s.Name)
		assert.Equal(t, "", s.Target)
	})

	t.Run("HEAD pointing at main", func(t *testing.T) {
		s := Symref{Name: "HEAD", Target: "refs/heads/main"}
		assert.Equal(t, "HEAD", s.Name)
		assert.Equal(t, "refs/heads/main", s.Target)
	})
}

func TestObjectFormat(t *testing.T) {
	assert.Equal(t, ObjectFormat("sha1"), ObjectFormatSHA1)
	assert.Equal(t, ObjectFormat("sha256"), ObjectFormatSHA256)
	assert.Equal(t, "sha1", string(ObjectFormatSHA1))
	assert.Equal(t, "sha256", string(ObjectFormatSHA256))
}

func TestCapabilities(t *testing.T) {
	t.Run("zero value", func(t *testing.T) {
		var c Capabilities
		assert.Equal(t, ProtocolVersion(0), c.Version)
		assert.Equal(t, "", c.Agent)
		assert.Equal(t, ObjectFormat(""), c.ObjectFormat)
		assert.Nil(t, c.Commands)
		assert.Nil(t, c.LSRefsArgs)
		assert.Nil(t, c.ObjectInfoArgs)
		assert.Nil(t, c.Symrefs)
		assert.Nil(t, c.Raw)
	})

	t.Run("nil Raw read returns nil slice", func(t *testing.T) {
		var c Capabilities
		assert.Nil(t, c.Raw["agent"])
	})

	t.Run("Raw preserves repeated capability values", func(t *testing.T) {
		c := Capabilities{
			Raw: map[string][]string{
				"symref": {"HEAD:refs/heads/main", "refs/remotes/origin/HEAD:refs/heads/main"},
			},
		}
		require.Len(t, c.Raw["symref"], 2)
		assert.Equal(t, "HEAD:refs/heads/main", c.Raw["symref"][0])
		assert.Equal(t, "refs/remotes/origin/HEAD:refs/heads/main", c.Raw["symref"][1])
	})

	t.Run("populated v2 capability set", func(t *testing.T) {
		c := Capabilities{
			Version:        ProtocolV2,
			Agent:          "git/2.45.0",
			ObjectFormat:   ObjectFormatSHA1,
			Commands:       []string{"ls-refs", "fetch", "object-info"},
			LSRefsArgs:     []string{"unborn", "ref-prefix", "symrefs", "peel"},
			ObjectInfoArgs: []string{"size"},
		}
		assert.Equal(t, ProtocolV2, c.Version)
		assert.Equal(t, "git/2.45.0", c.Agent)
		assert.Equal(t, ObjectFormatSHA1, c.ObjectFormat)
		assert.Contains(t, c.Commands, "object-info")
		assert.Contains(t, c.LSRefsArgs, "symrefs")
		assert.Contains(t, c.ObjectInfoArgs, "size")
		assert.Empty(t, c.Symrefs,
			"v2 servers do not advertise symrefs at the capability level")
	})

	t.Run("populated v0 symrefs", func(t *testing.T) {
		c := Capabilities{
			Version: ProtocolV0,
			Symrefs: []Symref{
				{Name: "HEAD", Target: "refs/heads/main"},
			},
		}
		assert.Equal(t, ProtocolV0, c.Version)
		require.Len(t, c.Symrefs, 1)
		assert.Equal(t, "HEAD", c.Symrefs[0].Name)
		assert.Equal(t, "refs/heads/main", c.Symrefs[0].Target)
	})
}

// TestProtocolVersionAlias pins that [ProtocolVersion] is a Go type
// alias of [transport.ProtocolVersion], not a fresh named type. The
// alias means values are interchangeable without conversion, and the
// exported constants resolve to the same package-level constants the
// transport package defines.
func TestProtocolVersionAlias(t *testing.T) {
	t.Run("constants are equal across packages", func(t *testing.T) {
		assert.Equal(t, transport.ProtocolV0, ProtocolV0)
		assert.Equal(t, transport.ProtocolV1, ProtocolV1)
		assert.Equal(t, transport.ProtocolV2, ProtocolV2)
	})

	t.Run("alias values are assignable without conversion", func(t *testing.T) {
		// The assignments compile only if ProtocolVersion is a type
		// alias (not a distinct named type): no explicit conversion
		// is performed in either direction. The explicit type
		// declarations are the assertion — staticcheck's ST1023
		// "inferred type" suggestion would defeat the point.
		var fromTransport transport.ProtocolVersion = ProtocolV2 //nolint:staticcheck // ST1023: explicit type is the test
		var fromLsremote ProtocolVersion = transport.ProtocolV2  //nolint:staticcheck // ST1023: explicit type is the test
		assert.Equal(t, fromTransport, fromLsremote)
	})

	t.Run("String formatting is inherited from transport", func(t *testing.T) {
		assert.Equal(t, "v2", ProtocolV2.String())
	})
}

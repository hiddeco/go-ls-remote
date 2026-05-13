package lsremote

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hiddeco/go-ls-remote/internal/wire"
)

func Test_convertCaps(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		raw     wire.RawCapabilities
		version ProtocolVersion
		check   func(t *testing.T, c Capabilities)
	}{
		{
			name:    "empty raw, v2",
			raw:     nil,
			version: ProtocolV2,
			check: func(t *testing.T, c Capabilities) {
				assert.Equal(t, ProtocolV2, c.Version)
				assert.Empty(t, c.Agent)
				assert.Equal(t, ObjectFormat(""), c.ObjectFormat)
				assert.Nil(t, c.Commands)
				assert.Nil(t, c.LSRefsArgs)
				assert.Nil(t, c.ObjectInfoArgs)
				assert.Nil(t, c.Symrefs)
				assert.NotNil(t, c.Raw)
				assert.Empty(t, c.Raw)
			},
		},
		{
			name: "v0 with symref, agent, object-format=sha1",
			raw: wire.RawCapabilities{
				{Name: "symref", Value: "HEAD:refs/heads/main"},
				{Name: "agent", Value: "git/2.40.0"},
				{Name: "object-format", Value: "sha1"},
			},
			version: ProtocolV0,
			check: func(t *testing.T, c Capabilities) {
				assert.Equal(t, ProtocolV0, c.Version)
				assert.Equal(t, "git/2.40.0", c.Agent)
				assert.Equal(t, ObjectFormatSHA1, c.ObjectFormat)
				assert.Nil(t, c.Commands)
				assert.Equal(t, []Symref{
					{Name: "HEAD", Target: "refs/heads/main"},
				}, c.Symrefs)
				assert.Nil(t, c.LSRefsArgs)
				assert.Nil(t, c.ObjectInfoArgs)
			},
		},
		{
			name: "v2 commands and per-command args",
			raw: wire.RawCapabilities{
				{Name: "agent", Value: "git/2.45.0"},
				{Name: "object-format", Value: "sha256"},
				{Name: "ls-refs", Value: "unborn"},
				{Name: "fetch", Value: "shallow filter"},
				{Name: "object-info", Value: "size"},
			},
			version: ProtocolV2,
			check: func(t *testing.T, c Capabilities) {
				assert.Equal(t, ProtocolV2, c.Version)
				assert.Equal(t, "git/2.45.0", c.Agent)
				assert.Equal(t, ObjectFormatSHA256, c.ObjectFormat)
				assert.Equal(t, []string{"ls-refs", "fetch", "object-info"}, c.Commands)
				assert.Equal(t, []string{"unborn"}, c.LSRefsArgs)
				assert.Equal(t, []string{"size"}, c.ObjectInfoArgs)
				assert.Equal(t, []string{"shallow filter"}, c.Raw["fetch"])
				assert.Empty(t, c.Symrefs)
			},
		},
		{
			name: "v2 boolean ls-refs has empty args slice, not nil",
			raw: wire.RawCapabilities{
				{Name: "ls-refs", Value: ""},
			},
			version: ProtocolV2,
			check: func(t *testing.T, c Capabilities) {
				assert.NotNil(t, c.LSRefsArgs)
				assert.Empty(t, c.LSRefsArgs)
				assert.Equal(t, []string{"ls-refs"}, c.Commands)
			},
		},
		{
			name: "v2 unknown command name not in allow-list is dropped",
			raw: wire.RawCapabilities{
				{Name: "ls-refs", Value: "unborn"},
				{Name: "future-cmd", Value: "x y"},
			},
			version: ProtocolV2,
			check: func(t *testing.T, c Capabilities) {
				assert.Equal(t, []string{"ls-refs"}, c.Commands)
			},
		},
		{
			name: "v2 bundle-uri is in allow-list",
			raw: wire.RawCapabilities{
				{Name: "ls-refs", Value: ""},
				{Name: "bundle-uri", Value: ""},
			},
			version: ProtocolV2,
			check: func(t *testing.T, c Capabilities) {
				assert.Equal(t, []string{"ls-refs", "bundle-uri"}, c.Commands)
			},
		},
		{
			name: "v0 duplicate symref entries both retained",
			raw: wire.RawCapabilities{
				{Name: "symref", Value: "HEAD:refs/heads/main"},
				{Name: "symref", Value: "refs/remotes/origin/HEAD:refs/heads/main"},
			},
			version: ProtocolV0,
			check: func(t *testing.T, c Capabilities) {
				assert.Equal(t, []Symref{
					{Name: "HEAD", Target: "refs/heads/main"},
					{Name: "refs/remotes/origin/HEAD", Target: "refs/heads/main"},
				}, c.Symrefs)
			},
		},
		{
			name: "v0 malformed symref without colon is skipped",
			raw: wire.RawCapabilities{
				{Name: "symref", Value: "HEAD:refs/heads/main"},
				{Name: "symref", Value: "garbage"},
				{Name: "symref", Value: ":nohead"},
				{Name: "symref", Value: "notarget:"},
			},
			version: ProtocolV0,
			check: func(t *testing.T, c Capabilities) {
				assert.Equal(t, []Symref{
					{Name: "HEAD", Target: "refs/heads/main"},
				}, c.Symrefs)
			},
		},
		{
			name:    "empty raw, v0 → SHA1",
			raw:     nil,
			version: ProtocolV0,
			check: func(t *testing.T, c Capabilities) {
				assert.Equal(t, ObjectFormatSHA1, c.ObjectFormat)
			},
		},
		{
			name:    "empty raw, v1 → SHA1",
			raw:     nil,
			version: ProtocolV1,
			check: func(t *testing.T, c Capabilities) {
				assert.Equal(t, ObjectFormatSHA1, c.ObjectFormat)
			},
		},
		{
			name: "v0 explicit sha256 wins over SHA1 default",
			raw: wire.RawCapabilities{
				{Name: "object-format", Value: "sha256"},
			},
			version: ProtocolV0,
			check: func(t *testing.T, c Capabilities) {
				assert.Equal(t, ObjectFormatSHA256, c.ObjectFormat)
			},
		},
		{
			name: "object-format unknown value preserved raw",
			raw: wire.RawCapabilities{
				{Name: "object-format", Value: "sha512"},
			},
			version: ProtocolV2,
			check: func(t *testing.T, c Capabilities) {
				assert.Equal(t, ObjectFormat("sha512"), c.ObjectFormat)
			},
		},
		{
			name: "raw map preserves repeated values in order",
			raw: wire.RawCapabilities{
				{Name: "agent", Value: "git/2.45.0"},
				{Name: "symref", Value: "HEAD:refs/heads/main"},
				{Name: "symref", Value: "refs/remotes/origin/HEAD:refs/heads/main"},
				{Name: "multi_ack", Value: ""},
			},
			version: ProtocolV0,
			check: func(t *testing.T, c Capabilities) {
				require.NotNil(t, c.Raw)
				assert.Equal(t, []string{"git/2.45.0"}, c.Raw["agent"])
				assert.Equal(t, []string{
					"HEAD:refs/heads/main",
					"refs/remotes/origin/HEAD:refs/heads/main",
				}, c.Raw["symref"])
				assert.Equal(t, []string{""}, c.Raw["multi_ack"])
			},
		},
		{
			name: "v2 ls-refs with multiple args via whitespace",
			raw: wire.RawCapabilities{
				{Name: "ls-refs", Value: "peel symrefs unborn"},
			},
			version: ProtocolV2,
			check: func(t *testing.T, c Capabilities) {
				assert.Equal(t, []string{"peel", "symrefs", "unborn"}, c.LSRefsArgs)
			},
		},
		{
			name: "v2 does not populate Symrefs even if symref capability present",
			raw: wire.RawCapabilities{
				{Name: "symref", Value: "HEAD:refs/heads/main"},
			},
			version: ProtocolV2,
			check: func(t *testing.T, c Capabilities) {
				assert.Empty(t, c.Symrefs)
			},
		},
		{
			name: "version reflects the argument exactly",
			raw:  wire.RawCapabilities{},
			// Constructing a Capabilities with V1 even though raw is empty —
			// the public Version reflects the negotiated version verbatim.
			version: ProtocolV1,
			check: func(t *testing.T, c Capabilities) {
				assert.Equal(t, ProtocolV1, c.Version)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := convertCaps(tc.raw, tc.version)
			tc.check(t, got)
		})
	}
}

// Test_convertCaps_rawIsolation pins that mutating the returned
// Capabilities.Raw does not alter the source RawCapabilities slice.
func Test_convertCaps_rawIsolation(t *testing.T) {
	t.Parallel()

	raw := wire.RawCapabilities{
		{Name: "agent", Value: "git/2.45.0"},
	}
	c := convertCaps(raw, ProtocolV2)
	c.Raw["agent"] = append(c.Raw["agent"], "tampered")
	c.Raw["new-key"] = []string{"injected"}

	// Source unchanged.
	require.Len(t, raw, 1)
	assert.Equal(t, "git/2.45.0", raw[0].Value)
}

// Test_convertCaps_v2Typical_AllocBudget pins an allocation upper bound
// for `convertCaps` on a typical v2 capability advertisement. The fixture
// matches `Benchmark_convertCaps_v2Typical`. The budget guards against
// regressions in slice pre-sizing and the v2 command-name walk.
//
// The structural floor is set by the `Capabilities.Raw` map escaping to
// the heap (header + buckets + one slice per unique capability name),
// the `LSRefsArgs` and `ObjectInfoArgs` slices produced by
// `strings.Fields`, and the pre-sized `Commands` slice.
//
//nolint:paralleltest // testing.AllocsPerRun panics in parallel tests.
func Test_convertCaps_v2Typical_AllocBudget(t *testing.T) {
	raw := wire.RawCapabilities{
		{Name: "agent", Value: "git/2.44.0"},
		{Name: "object-format", Value: "sha1"},
		{Name: "ls-refs", Value: "unborn"},
		{Name: "fetch", Value: "shallow filter"},
		{Name: "object-info", Value: "size"},
		{Name: "server-option"},
		{Name: "session-id", Value: "abc123def456"},
		{Name: "bundle-uri"},
	}

	got := testing.AllocsPerRun(100, func() {
		_ = convertCaps(raw, ProtocolV2)
	})
	const budget = 13.0
	if got > budget {
		t.Fatalf("convertCaps v2 typical: allocs/op=%v exceeds budget=%v", got, budget)
	}
}

// Test_convertCaps_v0WithSymrefs_AllocBudget pins an allocation upper
// bound for `convertCaps` on a v0 advertisement with symrefs. The fixture
// matches `Benchmark_convertCaps_v0WithSymrefs`. The structural floor is
// dominated by the escaping `Capabilities.Raw` map and the growth of
// `Capabilities.Symrefs` as the three `symref` entries are appended.
//
//nolint:paralleltest // testing.AllocsPerRun panics in parallel tests.
func Test_convertCaps_v0WithSymrefs_AllocBudget(t *testing.T) {
	raw := wire.RawCapabilities{
		{Name: "agent", Value: "git/2.44.0"},
		{Name: "object-format", Value: "sha1"},
		{Name: "symref", Value: "HEAD:refs/heads/main"},
		{Name: "symref", Value: "refs/remotes/origin/HEAD:refs/heads/main"},
		{Name: "symref", Value: "refs/remotes/upstream/HEAD:refs/heads/develop"},
		{Name: "multi_ack"},
		{Name: "side-band-64k"},
	}

	got := testing.AllocsPerRun(100, func() {
		_ = convertCaps(raw, ProtocolV0)
	})
	const budget = 12.0
	if got > budget {
		t.Fatalf("convertCaps v0 with symrefs: allocs/op=%v exceeds budget=%v", got, budget)
	}
}

func Test_convertRef(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   wire.RawRef
		want Ref
	}{
		{
			name: "plain ref",
			in:   wire.RawRef{OID: "abc", Name: "refs/heads/main"},
			want: Ref{Hash: "abc", Name: "refs/heads/main"},
		},
		{
			name: "peeled tag",
			in: wire.RawRef{
				OID:    "aaa",
				Name:   "refs/tags/v1.0.0",
				Peeled: "bbb",
			},
			want: Ref{
				Hash:   "aaa",
				Name:   "refs/tags/v1.0.0",
				Peeled: "bbb",
			},
		},
		{
			name: "symref HEAD",
			in: wire.RawRef{
				OID:    "ccc",
				Name:   "HEAD",
				Symref: "refs/heads/main",
			},
			want: Ref{
				Hash:   "ccc",
				Name:   "HEAD",
				Symref: "refs/heads/main",
			},
		},
		{
			name: "unborn HEAD",
			in: wire.RawRef{
				Name:   "HEAD",
				Symref: "refs/heads/main",
				Unborn: true,
			},
			want: Ref{
				Name:   "HEAD",
				Hash:   "",
				Symref: "refs/heads/main",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := convertRef(tc.in)
			assert.Equal(t, tc.want, got)
		})
	}
}

func Test_convertRefs(t *testing.T) {
	t.Parallel()

	t.Run("nil input returns nil", func(t *testing.T) {
		t.Parallel()

		assert.Nil(t, convertRefs(nil))
	})

	t.Run("empty slice returns empty non-nil slice", func(t *testing.T) {
		t.Parallel()

		got := convertRefs([]wire.RawRef{})
		assert.NotNil(t, got)
		assert.Empty(t, got)
	})

	t.Run("multiple refs preserve order", func(t *testing.T) {
		t.Parallel()

		in := []wire.RawRef{
			{OID: "a", Name: "refs/heads/main"},
			{OID: "b", Name: "refs/heads/dev", Peeled: "p"},
			{Name: "HEAD", Symref: "refs/heads/main", Unborn: true},
		}
		got := convertRefs(in)
		assert.Equal(t, []Ref{
			{Hash: "a", Name: "refs/heads/main"},
			{Hash: "b", Name: "refs/heads/dev", Peeled: "p"},
			{Name: "HEAD", Symref: "refs/heads/main"},
		}, got)
	})
}

func Test_convertObjectInfo(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   wire.RawObjectInfo
		want ObjectInfo
	}{
		{
			name: "size requested and returned",
			in:   wire.RawObjectInfo{OID: "x", Size: 42},
			want: ObjectInfo{Hash: "x", Size: 42},
		},
		{
			name: "zero size propagated verbatim",
			in:   wire.RawObjectInfo{OID: "y", Size: 0},
			want: ObjectInfo{Hash: "y", Size: 0},
		},
		{
			name: "large size",
			in:   wire.RawObjectInfo{OID: "z", Size: 1 << 40},
			want: ObjectInfo{Hash: "z", Size: 1 << 40},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := convertObjectInfo(tc.in)
			assert.Equal(t, tc.want, got)
		})
	}
}

func Test_convertObjectInfos(t *testing.T) {
	t.Parallel()

	t.Run("nil input returns nil", func(t *testing.T) {
		t.Parallel()

		assert.Nil(t, convertObjectInfos(nil))
	})

	t.Run("empty slice returns empty non-nil slice", func(t *testing.T) {
		t.Parallel()

		got := convertObjectInfos([]wire.RawObjectInfo{})
		assert.NotNil(t, got)
		assert.Empty(t, got)
	})

	t.Run("multiple rows preserve order", func(t *testing.T) {
		t.Parallel()

		in := []wire.RawObjectInfo{
			{OID: "a", Size: 1},
			{OID: "b", Size: 2},
			{OID: "c", Size: 3},
		}
		got := convertObjectInfos(in)
		assert.Equal(t, []ObjectInfo{
			{Hash: "a", Size: 1},
			{Hash: "b", Size: 2},
			{Hash: "c", Size: 3},
		}, got)
	})
}

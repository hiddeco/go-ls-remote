package wire

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/transport"
)

// Test fixtures: SHA-1 hashes (40 lowercase hex chars).
const (
	oidHEAD   = "0123456789abcdef0123456789abcdef01234567"
	oidMain   = "fedcba9876543210fedcba9876543210fedcba98"
	oidTag    = "1111111111111111111111111111111111111111"
	oidTagDef = "2222222222222222222222222222222222222222"
	oidShlw   = "3333333333333333333333333333333333333333"
	oidZero   = "0000000000000000000000000000000000000000"
)

func TestParseAdvertisement_v0v1(t *testing.T) {
	t.Parallel()

	t.Run("v0 single ref no caps", func(t *testing.T) {
		t.Parallel()

		r := buildAdvertisement(t,
			packet{data: []byte(oidMain + " refs/heads/main\x00\n")},
			packet{kind: pktline.Flush},
		)
		ad, err := ParseAdvertisement(r, nil)
		require.NoError(t, err)
		assert.Equal(t, transport.ProtocolV0, ad.Version)
		assert.Nil(t, ad.Caps)
		require.Len(t, ad.Refs, 1)
		assert.Equal(t, RawRef{OID: oidMain, Name: "refs/heads/main"}, ad.Refs[0])
	})

	t.Run("v0 first ref with caps then plain ref", func(t *testing.T) {
		t.Parallel()

		r := buildAdvertisement(t,
			packet{data: []byte(oidHEAD + " HEAD\x00multi_ack agent=git/2.45.0\n")},
			packet{data: []byte(oidMain + " refs/heads/main\n")},
			packet{kind: pktline.Flush},
		)
		ad, err := ParseAdvertisement(r, nil)
		require.NoError(t, err)
		assert.Equal(t, transport.ProtocolV0, ad.Version)
		require.Len(t, ad.Caps, 2)
		assert.Equal(t, RawCapability{Name: "multi_ack"}, ad.Caps[0])
		assert.Equal(t, RawCapability{Name: "agent", Value: "git/2.45.0"}, ad.Caps[1])
		require.Len(t, ad.Refs, 2)
		assert.Equal(t, RawRef{OID: oidHEAD, Name: "HEAD"}, ad.Refs[0])
		assert.Equal(t, RawRef{OID: oidMain, Name: "refs/heads/main"}, ad.Refs[1])
	})

	t.Run("v0 peeled tag attaches to preceding ref", func(t *testing.T) {
		t.Parallel()

		r := buildAdvertisement(t,
			packet{data: []byte(oidHEAD + " refs/heads/main\x00\n")},
			packet{data: []byte(oidTag + " refs/tags/v1\n")},
			packet{data: []byte(oidTagDef + " refs/tags/v1^{}\n")},
			packet{kind: pktline.Flush},
		)
		ad, err := ParseAdvertisement(r, nil)
		require.NoError(t, err)
		assert.Equal(t, transport.ProtocolV0, ad.Version)
		require.Len(t, ad.Refs, 2)
		assert.Equal(t, RawRef{OID: oidHEAD, Name: "refs/heads/main"}, ad.Refs[0])
		assert.Equal(t, RawRef{OID: oidTag, Name: "refs/tags/v1", Peeled: oidTagDef}, ad.Refs[1])
	})

	t.Run("v0 empty repo placeholder discarded", func(t *testing.T) {
		t.Parallel()

		r := buildAdvertisement(t,
			packet{data: []byte(oidZero + " capabilities^{}\x00agent=git/2.45.0 multi_ack\n")},
			packet{kind: pktline.Flush},
		)
		ad, err := ParseAdvertisement(r, nil)
		require.NoError(t, err)
		assert.Equal(t, transport.ProtocolV0, ad.Version)
		assert.Empty(t, ad.Refs)
		require.Len(t, ad.Caps, 2)
		assert.Equal(t, RawCapability{Name: "agent", Value: "git/2.45.0"}, ad.Caps[0])
		assert.Equal(t, RawCapability{Name: "multi_ack"}, ad.Caps[1])
	})

	t.Run("v0 multi-symref applied to refs", func(t *testing.T) {
		t.Parallel()

		caps := "symref=HEAD:refs/heads/main symref=ORIG_HEAD:refs/heads/main"
		r := buildAdvertisement(t,
			packet{data: []byte(oidHEAD + " HEAD\x00" + caps + "\n")},
			packet{data: []byte(oidHEAD + " ORIG_HEAD\n")},
			packet{data: []byte(oidMain + " refs/heads/main\n")},
			packet{kind: pktline.Flush},
		)
		ad, err := ParseAdvertisement(r, nil)
		require.NoError(t, err)
		require.Len(t, ad.Refs, 3)
		assert.Equal(t, RawRef{OID: oidHEAD, Name: "HEAD", Symref: "refs/heads/main"}, ad.Refs[0])
		assert.Equal(t, RawRef{OID: oidHEAD, Name: "ORIG_HEAD", Symref: "refs/heads/main"}, ad.Refs[1])
		assert.Equal(t, RawRef{OID: oidMain, Name: "refs/heads/main"}, ad.Refs[2])
	})

	t.Run("v0 symref splits on first colon", func(t *testing.T) {
		t.Parallel()

		// Refname containing `:` is rare but legal under the BNF. The
		// split must apply only to the first `:`.
		caps := "symref=HEAD:refs/heads/weird:name"
		r := buildAdvertisement(t,
			packet{data: []byte(oidHEAD + " HEAD\x00" + caps + "\n")},
			packet{kind: pktline.Flush},
		)
		ad, err := ParseAdvertisement(r, nil)
		require.NoError(t, err)
		require.Len(t, ad.Refs, 1)
		assert.Equal(t, "refs/heads/weird:name", ad.Refs[0].Symref)
	})

	t.Run("v0 shallow line silently skipped", func(t *testing.T) {
		t.Parallel()

		r := buildAdvertisement(t,
			packet{data: []byte(oidMain + " refs/heads/main\x00\n")},
			packet{data: []byte("shallow " + oidShlw + "\n")},
			packet{kind: pktline.Flush},
		)
		ad, err := ParseAdvertisement(r, nil)
		require.NoError(t, err)
		require.Len(t, ad.Refs, 1)
		assert.Equal(t, RawRef{OID: oidMain, Name: "refs/heads/main"}, ad.Refs[0])
	})

	t.Run("v0 dangling symref leaves no error", func(t *testing.T) {
		t.Parallel()

		// `symref=NOSUCH:...` — no matching ref. Canonical Git silently
		// drops the dangling entry.
		caps := "symref=NOSUCH:refs/heads/main"
		r := buildAdvertisement(t,
			packet{data: []byte(oidMain + " refs/heads/main\x00" + caps + "\n")},
			packet{kind: pktline.Flush},
		)
		ad, err := ParseAdvertisement(r, nil)
		require.NoError(t, err)
		require.Len(t, ad.Refs, 1)
		assert.Empty(t, ad.Refs[0].Symref)
	})

	t.Run("v0 first line missing NUL is malformed", func(t *testing.T) {
		t.Parallel()

		r := buildAdvertisement(t,
			packet{data: []byte(oidMain + " refs/heads/main\n")},
			packet{kind: pktline.Flush},
		)
		_, err := ParseAdvertisement(r, nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "first ref")
	})

	t.Run("v0 first line missing space is malformed", func(t *testing.T) {
		t.Parallel()

		r := buildAdvertisement(t,
			packet{data: []byte("nospace\x00caps\n")},
			packet{kind: pktline.Flush},
		)
		_, err := ParseAdvertisement(r, nil)
		require.Error(t, err)
	})

	t.Run("v0 peel without preceding ref errors", func(t *testing.T) {
		t.Parallel()

		// First line is a peel — no preceding ref, malformed.
		r := buildAdvertisement(t,
			packet{data: []byte(oidTag + " refs/tags/v1^{}\x00\n")},
			packet{data: []byte(oidMain + " refs/heads/main\n")},
			packet{kind: pktline.Flush},
		)
		_, err := ParseAdvertisement(r, nil)
		require.Error(t, err)
	})

	t.Run("v0 peel paired with mismatched preceding ref errors", func(t *testing.T) {
		t.Parallel()

		r := buildAdvertisement(t,
			packet{data: []byte(oidMain + " refs/heads/main\x00\n")},
			packet{data: []byte(oidTagDef + " refs/tags/v1^{}\n")},
			packet{kind: pktline.Flush},
		)
		_, err := ParseAdvertisement(r, nil)
		require.Error(t, err)
	})

	t.Run("v0 other-tip line missing space is malformed", func(t *testing.T) {
		t.Parallel()

		r := buildAdvertisement(t,
			packet{data: []byte(oidMain + " refs/heads/main\x00\n")},
			packet{data: []byte("nospaceline\n")},
			packet{kind: pktline.Flush},
		)
		_, err := ParseAdvertisement(r, nil)
		require.Error(t, err)
	})

	t.Run("v0 unexpected delim is a wire violation", func(t *testing.T) {
		t.Parallel()

		r := buildAdvertisement(t,
			packet{data: []byte(oidMain + " refs/heads/main\x00\n")},
			packet{kind: pktline.Delim},
		)
		_, err := ParseAdvertisement(r, nil)
		require.Error(t, err)
	})

	t.Run("v0 truncated stream surfaces unexpected EOF", func(t *testing.T) {
		t.Parallel()

		// First ref line, no flush — pktline.Reader returns io.EOF on
		// the next read; parser must convert to ErrUnexpectedEOF.
		r := buildAdvertisement(t,
			packet{data: []byte(oidMain + " refs/heads/main\x00\n")},
		)
		_, err := ParseAdvertisement(r, nil)
		require.Error(t, err)
	})

	t.Run("v1 shape matches v0 with version 1 prefix", func(t *testing.T) {
		t.Parallel()

		r := buildAdvertisement(t,
			packet{data: []byte("version 1\n")},
			packet{data: []byte(oidHEAD + " HEAD\x00symref=HEAD:refs/heads/main agent=git\n")},
			packet{data: []byte(oidMain + " refs/heads/main\n")},
			packet{data: []byte(oidTag + " refs/tags/v1\n")},
			packet{data: []byte(oidTagDef + " refs/tags/v1^{}\n")},
			packet{kind: pktline.Flush},
		)
		ad, err := ParseAdvertisement(r, nil)
		require.NoError(t, err)
		assert.Equal(t, transport.ProtocolV1, ad.Version)
		require.Len(t, ad.Caps, 2)
		require.Len(t, ad.Refs, 3)
		assert.Equal(t, RawRef{OID: oidHEAD, Name: "HEAD", Symref: "refs/heads/main"}, ad.Refs[0])
		assert.Equal(t, RawRef{OID: oidMain, Name: "refs/heads/main"}, ad.Refs[1])
		assert.Equal(t, RawRef{OID: oidTag, Name: "refs/tags/v1", Peeled: oidTagDef}, ad.Refs[2])
	})

	t.Run("v0 empty repo with no caps", func(t *testing.T) {
		t.Parallel()

		// Edge case: caps block is empty after the NUL.
		r := buildAdvertisement(t,
			packet{data: []byte(oidZero + " capabilities^{}\x00\n")},
			packet{kind: pktline.Flush},
		)
		ad, err := ParseAdvertisement(r, nil)
		require.NoError(t, err)
		assert.Empty(t, ad.Refs)
		assert.Empty(t, ad.Caps)
	})

	t.Run("v0 cap list trims trailing LF only", func(t *testing.T) {
		t.Parallel()

		// Confirm the parser does not strip CR (CR is not in the
		// canonical whitespace set) — a payload ending in "\r\n" keeps
		// the CR as part of the last cap token.
		r := buildAdvertisement(t,
			packet{data: []byte(oidMain + " refs/heads/main\x00agent=git\r\n")},
			packet{kind: pktline.Flush},
		)
		ad, err := ParseAdvertisement(r, nil)
		require.NoError(t, err)
		require.Len(t, ad.Caps, 1)
		// The CR should remain attached to the value because it is not
		// LF and not in the cap-list whitespace alphabet.
		assert.True(t, strings.HasSuffix(ad.Caps[0].Value, "\r"),
			"expected trailing CR preserved on cap value, got %q",
			ad.Caps[0].Value)
	})
}

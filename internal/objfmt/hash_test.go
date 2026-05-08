package objfmt

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHash_IsZero(t *testing.T) {
	t.Run("zero value is zero", func(t *testing.T) {
		assert.True(t, Hash{}.IsZero())
	})
	t.Run("any non-zero byte makes it non-zero", func(t *testing.T) {
		var h Hash
		h[0] = 0x01
		assert.False(t, h.IsZero())
	})
	t.Run("trailing non-zero byte still non-zero", func(t *testing.T) {
		var h Hash
		h[31] = 0x01
		assert.False(t, h.IsZero())
	})
}

func TestHash_Hex(t *testing.T) {
	t.Run("SHA1 hex is 40 lowercase chars of low 20 bytes", func(t *testing.T) {
		var h Hash
		for i := 0; i < 20; i++ {
			h[i] = 0xab
		}
		// High 12 bytes intentionally garbage to prove they are ignored.
		for i := 20; i < 32; i++ {
			h[i] = 0xff
		}
		got := h.Hex(SHA1)
		assert.Len(t, got, 40)
		assert.Equal(t, strings.Repeat("ab", 20), got)
		assert.Equal(t, strings.ToLower(got), got)
	})
	t.Run("SHA256 hex is 64 chars of all 32 bytes", func(t *testing.T) {
		var h Hash
		for i := range h {
			h[i] = 0xcd
		}
		got := h.Hex(SHA256)
		assert.Len(t, got, 64)
		assert.Equal(t, strings.Repeat("cd", 32), got)
	})
	t.Run("unknown algo returns empty string", func(t *testing.T) {
		assert.Equal(t, "", Hash{}.Hex(Algo(0)))
	})
}

func TestParseHex(t *testing.T) {
	t.Run("SHA1 round-trips with Hex", func(t *testing.T) {
		in := "0123456789abcdef0123456789abcdef01234567"
		h, err := ParseHex(in, SHA1)
		require.NoError(t, err)
		assert.Equal(t, in, h.Hex(SHA1))
		// High 12 bytes must be zero so SHA-1 hashes compare as keys.
		for i := 20; i < 32; i++ {
			assert.Equal(t, byte(0), h[i])
		}
	})
	t.Run("SHA256 round-trips with Hex", func(t *testing.T) {
		in := strings.Repeat("ab", 32)
		h, err := ParseHex(in, SHA256)
		require.NoError(t, err)
		assert.Equal(t, in, h.Hex(SHA256))
	})
	t.Run("rejects 40 chars with SHA256 algo", func(t *testing.T) {
		_, err := ParseHex(strings.Repeat("a", 40), SHA256)
		assert.Error(t, err)
	})
	t.Run("rejects 64 chars with SHA1 algo", func(t *testing.T) {
		_, err := ParseHex(strings.Repeat("a", 64), SHA1)
		assert.Error(t, err)
	})
	t.Run("rejects empty string", func(t *testing.T) {
		_, err := ParseHex("", SHA1)
		assert.Error(t, err)
	})
	t.Run("rejects non-hex input", func(t *testing.T) {
		_, err := ParseHex("zz"+strings.Repeat("a", 38), SHA1)
		assert.Error(t, err)
	})
	t.Run("rejects unknown algo", func(t *testing.T) {
		_, err := ParseHex(strings.Repeat("a", 40), Algo(0))
		assert.Error(t, err)
	})
}

func TestHash_comparable(t *testing.T) {
	t.Run("equal SHA-1 hashes are usable as map keys", func(t *testing.T) {
		a, err := ParseHex(strings.Repeat("0a", 20), SHA1)
		require.NoError(t, err)
		b, err := ParseHex(strings.Repeat("0a", 20), SHA1)
		require.NoError(t, err)
		assert.True(t, a == b)
		m := map[Hash]int{a: 1}
		assert.Equal(t, 1, m[b])
	})
}

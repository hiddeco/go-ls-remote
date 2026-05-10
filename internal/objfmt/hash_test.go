package objfmt

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSHA1Hash_Hex(t *testing.T) {
	t.Run("zero value hex is 40 zero chars", func(t *testing.T) {
		assert.Equal(t, strings.Repeat("0", 40), SHA1Hash{}.Hex())
	})
	t.Run("known input hex is 40 lowercase chars", func(t *testing.T) {
		var h SHA1Hash
		for i := range h {
			h[i] = byte(i)
		}
		got := h.Hex()
		assert.Len(t, got, 40)
		assert.Equal(t, "000102030405060708090a0b0c0d0e0f10111213", got)
		assert.Equal(t, strings.ToLower(got), got)
	})
}

func TestSHA1Hash_AppendHex(t *testing.T) {
	t.Run("appends 40 hex chars to existing dst", func(t *testing.T) {
		var h SHA1Hash
		for i := range h {
			h[i] = byte(i)
		}
		got := h.AppendHex([]byte("prefix:"))
		assert.Equal(t, "prefix:000102030405060708090a0b0c0d0e0f10111213", string(got))
	})
	t.Run("agrees byte-for-byte with Hex", func(t *testing.T) {
		var h SHA1Hash
		for i := range h {
			h[i] = byte(i*7 + 3)
		}
		assert.Equal(t, h.Hex(), string(h.AppendHex(nil)))
	})
}

func TestSHA1Hash_IsZero(t *testing.T) {
	t.Run("zero value is zero", func(t *testing.T) {
		assert.True(t, SHA1Hash{}.IsZero())
	})
	t.Run("first byte set is non-zero", func(t *testing.T) {
		var h SHA1Hash
		h[0] = 0x01
		assert.False(t, h.IsZero())
	})
	t.Run("trailing byte set is non-zero", func(t *testing.T) {
		var h SHA1Hash
		h[19] = 0x01
		assert.False(t, h.IsZero())
	})
}

func TestSHA1Hash_Bytes(t *testing.T) {
	t.Run("returns a 20-byte slice with the same contents", func(t *testing.T) {
		var h SHA1Hash
		for i := range h {
			h[i] = byte(i)
		}
		b := h.Bytes()
		assert.Len(t, b, 20)
		for i, v := range b {
			assert.Equal(t, byte(i), v)
		}
	})
	t.Run("returned slice does not alias the receiver", func(t *testing.T) {
		// Bytes is defined on a value receiver, so the slice references
		// the copy's storage, not the caller's array. Mutating the
		// returned slice must therefore leave the receiver untouched.
		var h SHA1Hash
		b := h.Bytes()
		b[0] = 0xff
		assert.Equal(t, byte(0x00), h[0])
	})
}

func TestSHA256Hash_Hex(t *testing.T) {
	t.Run("zero value hex is 64 zero chars", func(t *testing.T) {
		assert.Equal(t, strings.Repeat("0", 64), SHA256Hash{}.Hex())
	})
	t.Run("known input hex is 64 lowercase chars", func(t *testing.T) {
		var h SHA256Hash
		for i := range h {
			h[i] = 0xcd
		}
		got := h.Hex()
		assert.Len(t, got, 64)
		assert.Equal(t, strings.Repeat("cd", 32), got)
		assert.Equal(t, strings.ToLower(got), got)
	})
}

func TestSHA256Hash_AppendHex(t *testing.T) {
	t.Run("appends 64 hex chars", func(t *testing.T) {
		var h SHA256Hash
		for i := range h {
			h[i] = byte(i)
		}
		got := h.AppendHex(nil)
		want := "000102030405060708090a0b0c0d0e0f" +
			"101112131415161718191a1b1c1d1e1f"
		assert.Equal(t, want, string(got))
	})
	t.Run("agrees byte-for-byte with Hex", func(t *testing.T) {
		var h SHA256Hash
		for i := range h {
			h[i] = byte(i*7 + 3)
		}
		assert.Equal(t, h.Hex(), string(h.AppendHex(nil)))
	})
}

func TestSHA256Hash_IsZero(t *testing.T) {
	t.Run("zero value is zero", func(t *testing.T) {
		assert.True(t, SHA256Hash{}.IsZero())
	})
	t.Run("first byte set is non-zero", func(t *testing.T) {
		var h SHA256Hash
		h[0] = 0x01
		assert.False(t, h.IsZero())
	})
	t.Run("trailing byte set is non-zero", func(t *testing.T) {
		var h SHA256Hash
		h[31] = 0x01
		assert.False(t, h.IsZero())
	})
}

func TestSHA256Hash_Bytes(t *testing.T) {
	t.Run("returns a 32-byte slice with the same contents", func(t *testing.T) {
		var h SHA256Hash
		for i := range h {
			h[i] = byte(i)
		}
		b := h.Bytes()
		assert.Len(t, b, 32)
		for i, v := range b {
			assert.Equal(t, byte(i), v)
		}
	})
	t.Run("returned slice does not alias the receiver", func(t *testing.T) {
		// See [TestSHA1Hash_Bytes] for the rationale.
		var h SHA256Hash
		b := h.Bytes()
		b[0] = 0xff
		assert.Equal(t, byte(0x00), h[0])
	})
}

func TestParseSHA1Hex(t *testing.T) {
	t.Run("round-trips with Hex", func(t *testing.T) {
		in := "0123456789abcdef0123456789abcdef01234567"
		h, err := ParseSHA1Hex(in)
		require.NoError(t, err)
		assert.Equal(t, in, h.Hex())
	})
	t.Run("rejects 39-char input", func(t *testing.T) {
		_, err := ParseSHA1Hex(strings.Repeat("a", 39))
		assert.Error(t, err)
	})
	t.Run("rejects 41-char input", func(t *testing.T) {
		_, err := ParseSHA1Hex(strings.Repeat("a", 41))
		assert.Error(t, err)
	})
	t.Run("rejects 64-char input", func(t *testing.T) {
		_, err := ParseSHA1Hex(strings.Repeat("a", 64))
		assert.Error(t, err)
	})
	t.Run("rejects empty input", func(t *testing.T) {
		_, err := ParseSHA1Hex("")
		assert.Error(t, err)
	})
	t.Run("rejects non-hex input", func(t *testing.T) {
		_, err := ParseSHA1Hex("zz" + strings.Repeat("a", 38))
		assert.Error(t, err)
	})
}

func TestParseSHA256Hex(t *testing.T) {
	t.Run("round-trips with Hex", func(t *testing.T) {
		in := strings.Repeat("ab", 32)
		h, err := ParseSHA256Hex(in)
		require.NoError(t, err)
		assert.Equal(t, in, h.Hex())
	})
	t.Run("rejects 63-char input", func(t *testing.T) {
		_, err := ParseSHA256Hex(strings.Repeat("a", 63))
		assert.Error(t, err)
	})
	t.Run("rejects 65-char input", func(t *testing.T) {
		_, err := ParseSHA256Hex(strings.Repeat("a", 65))
		assert.Error(t, err)
	})
	t.Run("rejects 40-char input", func(t *testing.T) {
		_, err := ParseSHA256Hex(strings.Repeat("a", 40))
		assert.Error(t, err)
	})
	t.Run("rejects non-hex input", func(t *testing.T) {
		_, err := ParseSHA256Hex("zz" + strings.Repeat("a", 62))
		assert.Error(t, err)
	})
}

func Test_sha1Algo_ParseHex(t *testing.T) {
	t.Run("returns SHA1Hash with round-tripping hex", func(t *testing.T) {
		in := "0123456789abcdef0123456789abcdef01234567"
		h, err := sha1Algo{}.ParseHex(in)
		require.NoError(t, err)
		assert.Equal(t, in, h.Hex())
	})
	t.Run("rejects wrong-length input", func(t *testing.T) {
		_, err := sha1Algo{}.ParseHex(strings.Repeat("a", 64))
		assert.Error(t, err)
	})
}

func Test_sha256Algo_ParseHex(t *testing.T) {
	t.Run("returns SHA256Hash with round-tripping hex", func(t *testing.T) {
		in := strings.Repeat("ab", 32)
		h, err := sha256Algo{}.ParseHex(in)
		require.NoError(t, err)
		assert.Equal(t, in, h.Hex())
	})
	t.Run("rejects wrong-length input", func(t *testing.T) {
		_, err := sha256Algo{}.ParseHex(strings.Repeat("a", 40))
		assert.Error(t, err)
	})
}

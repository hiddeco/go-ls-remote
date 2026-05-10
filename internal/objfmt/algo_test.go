package objfmt

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAlgo_Size(t *testing.T) {
	t.Run("SHA1 is 20 bytes", func(t *testing.T) {
		assert.Equal(t, 20, SHA1.Size())
	})
	t.Run("SHA256 is 32 bytes", func(t *testing.T) {
		assert.Equal(t, 32, SHA256.Size())
	})
}

func TestAlgo_String(t *testing.T) {
	t.Run("SHA1 prints sha1", func(t *testing.T) {
		assert.Equal(t, "sha1", SHA1.String())
	})
	t.Run("SHA256 prints sha256", func(t *testing.T) {
		assert.Equal(t, "sha256", SHA256.String())
	})
}

func TestAlgo_comparable(t *testing.T) {
	t.Run("SHA1 equals itself", func(t *testing.T) {
		assert.Equal(t, SHA1, SHA1)
		assert.True(t, SHA1 == SHA1)
	})
	t.Run("SHA256 equals itself", func(t *testing.T) {
		assert.Equal(t, SHA256, SHA256)
		assert.True(t, SHA256 == SHA256)
	})
	t.Run("SHA1 differs from SHA256", func(t *testing.T) {
		assert.NotEqual(t, SHA1, SHA256)
		assert.False(t, SHA1 == SHA256)
	})
}

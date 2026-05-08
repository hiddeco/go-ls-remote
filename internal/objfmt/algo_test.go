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
	t.Run("zero value is unknown", func(t *testing.T) {
		assert.Equal(t, 0, Algo(0).Size())
	})
	t.Run("out-of-range is unknown", func(t *testing.T) {
		assert.Equal(t, 0, Algo(99).Size())
	})
}

func TestAlgo_String(t *testing.T) {
	t.Run("SHA1 prints sha1", func(t *testing.T) {
		assert.Equal(t, "sha1", SHA1.String())
	})
	t.Run("SHA256 prints sha256", func(t *testing.T) {
		assert.Equal(t, "sha256", SHA256.String())
	})
	t.Run("zero value prints unknown", func(t *testing.T) {
		assert.Equal(t, "unknown", Algo(0).String())
	})
	t.Run("out-of-range prints unknown", func(t *testing.T) {
		assert.Equal(t, "unknown", Algo(99).String())
	})
}

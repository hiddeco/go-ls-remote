package objfmt

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAlgo_Size(t *testing.T) {
	t.Parallel()

	t.Run("SHA1 is 20 bytes", func(t *testing.T) {
		t.Parallel()

		assert.Equal(t, 20, SHA1.Size())
	})
	t.Run("SHA256 is 32 bytes", func(t *testing.T) {
		t.Parallel()

		assert.Equal(t, 32, SHA256.Size())
	})
}

func TestAlgo_String(t *testing.T) {
	t.Parallel()

	t.Run("SHA1 prints sha1", func(t *testing.T) {
		t.Parallel()

		assert.Equal(t, "sha1", SHA1.String())
	})
	t.Run("SHA256 prints sha256", func(t *testing.T) {
		t.Parallel()

		assert.Equal(t, "sha256", SHA256.String())
	})
}

func TestAlgo_comparable(t *testing.T) {
	t.Parallel()

	t.Run("SHA1 equals itself", func(t *testing.T) {
		t.Parallel()

		assert.True(t, SHA1 == SHA1, //nolint:gocritic,testifylint // exercises the == operator the type promises
			"SHA1 must be comparable to itself via the == operator")
	})
	t.Run("SHA256 equals itself", func(t *testing.T) {
		t.Parallel()

		assert.True(t, SHA256 == SHA256, //nolint:gocritic,testifylint // exercises the == operator the type promises
			"SHA256 must be comparable to itself via the == operator")
	})
	t.Run("SHA1 differs from SHA256", func(t *testing.T) {
		t.Parallel()

		assert.NotEqual(t, SHA1, SHA256)
	})
}

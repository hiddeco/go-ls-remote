package gitt_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	gitt "github.com/hiddeco/go-ls-remote/transport/git"
)

func TestTransport_Schemes(t *testing.T) {
	t.Parallel()

	got := gitt.New().Schemes()
	assert.Equal(t, []string{"git"}, got)
}

func TestNew(t *testing.T) {
	t.Parallel()

	t.Run("no options", func(t *testing.T) {
		t.Parallel()

		tr := gitt.New()
		assert.NotNil(t, tr)
	})

	t.Run("nil option skipped", func(t *testing.T) {
		t.Parallel()

		tr := gitt.New(nil)
		assert.NotNil(t, tr)
		// Must not panic; Schemes must still work.
		assert.Equal(t, []string{"git"}, tr.Schemes())
	})
}

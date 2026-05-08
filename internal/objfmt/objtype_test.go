package objfmt

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestObjectType_numericValues(t *testing.T) {
	t.Run("matches canonical Git constants", func(t *testing.T) {
		assert.Equal(t, ObjectType(1), TypeCommit)
		assert.Equal(t, ObjectType(2), TypeTree)
		assert.Equal(t, ObjectType(3), TypeBlob)
		assert.Equal(t, ObjectType(4), TypeTag)
		assert.Equal(t, ObjectType(6), TypeOfsDelta)
		assert.Equal(t, ObjectType(7), TypeRefDelta)
	})
}

func TestObjectType_String(t *testing.T) {
	t.Run("commit", func(t *testing.T) {
		assert.Equal(t, "commit", TypeCommit.String())
	})
	t.Run("tree", func(t *testing.T) {
		assert.Equal(t, "tree", TypeTree.String())
	})
	t.Run("blob", func(t *testing.T) {
		assert.Equal(t, "blob", TypeBlob.String())
	})
	t.Run("tag", func(t *testing.T) {
		assert.Equal(t, "tag", TypeTag.String())
	})
	t.Run("ofs_delta returns empty string", func(t *testing.T) {
		assert.Equal(t, "", TypeOfsDelta.String())
	})
	t.Run("ref_delta returns empty string", func(t *testing.T) {
		assert.Equal(t, "", TypeRefDelta.String())
	})
	t.Run("unknown returns empty string", func(t *testing.T) {
		assert.Equal(t, "", ObjectType(0).String())
		assert.Equal(t, "", ObjectType(5).String())
		assert.Equal(t, "", ObjectType(8).String())
	})
}

func TestObjectType_IsDelta(t *testing.T) {
	t.Run("ofs_delta is delta", func(t *testing.T) {
		assert.True(t, TypeOfsDelta.IsDelta())
	})
	t.Run("ref_delta is delta", func(t *testing.T) {
		assert.True(t, TypeRefDelta.IsDelta())
	})
	t.Run("commit is not delta", func(t *testing.T) {
		assert.False(t, TypeCommit.IsDelta())
	})
	t.Run("tree is not delta", func(t *testing.T) {
		assert.False(t, TypeTree.IsDelta())
	})
	t.Run("blob is not delta", func(t *testing.T) {
		assert.False(t, TypeBlob.IsDelta())
	})
	t.Run("tag is not delta", func(t *testing.T) {
		assert.False(t, TypeTag.IsDelta())
	})
	t.Run("zero value is not delta", func(t *testing.T) {
		assert.False(t, ObjectType(0).IsDelta())
	})
	t.Run("reserved 5 is not delta", func(t *testing.T) {
		assert.False(t, ObjectType(5).IsDelta())
	})
}

package objfmt

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestObjectType_numericValues(t *testing.T) {
	t.Parallel()
	t.Run("matches canonical Git constants", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, TypeCommit, ObjectType(1))
		assert.Equal(t, TypeTree, ObjectType(2))
		assert.Equal(t, TypeBlob, ObjectType(3))
		assert.Equal(t, TypeTag, ObjectType(4))
		assert.Equal(t, TypeOfsDelta, ObjectType(6))
		assert.Equal(t, TypeRefDelta, ObjectType(7))
	})
}

func TestObjectType_String(t *testing.T) {
	t.Parallel()
	t.Run("commit", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, "commit", TypeCommit.String())
	})
	t.Run("tree", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, "tree", TypeTree.String())
	})
	t.Run("blob", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, "blob", TypeBlob.String())
	})
	t.Run("tag", func(t *testing.T) {
		t.Parallel()
		assert.Equal(t, "tag", TypeTag.String())
	})
	t.Run("ofs_delta returns empty string", func(t *testing.T) {
		t.Parallel()
		assert.Empty(t, TypeOfsDelta.String())
	})
	t.Run("ref_delta returns empty string", func(t *testing.T) {
		t.Parallel()
		assert.Empty(t, TypeRefDelta.String())
	})
	t.Run("unknown returns empty string", func(t *testing.T) {
		t.Parallel()
		assert.Empty(t, ObjectType(0).String())
		assert.Empty(t, ObjectType(5).String())
		assert.Empty(t, ObjectType(8).String())
	})
}

func TestObjectType_IsDelta(t *testing.T) {
	t.Parallel()
	t.Run("ofs_delta is delta", func(t *testing.T) {
		t.Parallel()
		assert.True(t, TypeOfsDelta.IsDelta())
	})
	t.Run("ref_delta is delta", func(t *testing.T) {
		t.Parallel()
		assert.True(t, TypeRefDelta.IsDelta())
	})
	t.Run("commit is not delta", func(t *testing.T) {
		t.Parallel()
		assert.False(t, TypeCommit.IsDelta())
	})
	t.Run("tree is not delta", func(t *testing.T) {
		t.Parallel()
		assert.False(t, TypeTree.IsDelta())
	})
	t.Run("blob is not delta", func(t *testing.T) {
		t.Parallel()
		assert.False(t, TypeBlob.IsDelta())
	})
	t.Run("tag is not delta", func(t *testing.T) {
		t.Parallel()
		assert.False(t, TypeTag.IsDelta())
	})
	t.Run("zero value is not delta", func(t *testing.T) {
		t.Parallel()
		assert.False(t, ObjectType(0).IsDelta())
	})
	t.Run("reserved 5 is not delta", func(t *testing.T) {
		t.Parallel()
		assert.False(t, ObjectType(5).IsDelta())
	})
}

package lsremote

import (
	"reflect"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRefsRequest(t *testing.T) {
	t.Run("zero value", func(t *testing.T) {
		var a RefsRequest
		assert.Nil(t, a.Prefixes,
			"zero-value Prefixes is the nil slice, not an empty allocation")
		assert.False(t, a.Peel)
		assert.False(t, a.Symrefs)
		assert.False(t, a.Unborn)
	})

	t.Run("populated values survive round trip", func(t *testing.T) {
		a := RefsRequest{
			Prefixes: []string{"refs/heads/", "refs/tags/"},
			Peel:     true,
			Symrefs:  true,
			Unborn:   true,
		}
		require.Len(t, a.Prefixes, 2)
		assert.Equal(t, "refs/heads/", a.Prefixes[0])
		assert.Equal(t, "refs/tags/", a.Prefixes[1])
		assert.True(t, a.Peel)
		assert.True(t, a.Symrefs)
		assert.True(t, a.Unborn)
	})

	t.Run("exported field set is exactly the documented one", func(t *testing.T) {
		want := []string{"Peel", "Prefixes", "Symrefs", "Unborn"}
		got := exportedFieldNames(reflect.TypeOf(RefsRequest{}))
		assert.Equal(t, want, got)
	})

	t.Run("field types are as documented", func(t *testing.T) {
		typ := reflect.TypeOf(RefsRequest{})
		assertFieldType(t, typ, "Prefixes", reflect.SliceOf(reflect.TypeOf("")))
		assertFieldType(t, typ, "Peel", reflect.TypeOf(false))
		assertFieldType(t, typ, "Symrefs", reflect.TypeOf(false))
		assertFieldType(t, typ, "Unborn", reflect.TypeOf(false))
	})

	t.Run("no methods on the type", func(t *testing.T) {
		// RefsRequest is a plain data carrier — no methods on the
		// value or pointer receiver.
		assert.Equal(t, 0, reflect.TypeOf(RefsRequest{}).NumMethod())
		assert.Equal(t, 0, reflect.PointerTo(reflect.TypeOf(RefsRequest{})).NumMethod())
	})
}

func TestObjectInfoRequest(t *testing.T) {
	t.Run("zero value", func(t *testing.T) {
		var a ObjectInfoRequest
		assert.False(t, a.Size)
	})

	t.Run("populated values survive round trip", func(t *testing.T) {
		a := ObjectInfoRequest{Size: true}
		assert.True(t, a.Size)
	})

	t.Run("exported field set is exactly the documented one", func(t *testing.T) {
		want := []string{"Size"}
		got := exportedFieldNames(reflect.TypeOf(ObjectInfoRequest{}))
		assert.Equal(t, want, got)
	})

	t.Run("field types are as documented", func(t *testing.T) {
		typ := reflect.TypeOf(ObjectInfoRequest{})
		assertFieldType(t, typ, "Size", reflect.TypeOf(false))
	})

	t.Run("no methods on the type", func(t *testing.T) {
		assert.Equal(t, 0, reflect.TypeOf(ObjectInfoRequest{}).NumMethod())
		assert.Equal(t, 0, reflect.PointerTo(reflect.TypeOf(ObjectInfoRequest{})).NumMethod())
	})
}

// exportedFieldNames returns the names of the exported fields of a
// struct type in sorted order so callers can compare against a stable
// expectation.
func exportedFieldNames(typ reflect.Type) []string {
	var names []string
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.IsExported() {
			names = append(names, f.Name)
		}
	}
	sort.Strings(names)
	return names
}

func assertFieldType(t *testing.T, typ reflect.Type, name string, want reflect.Type) {
	t.Helper()
	f, ok := typ.FieldByName(name)
	require.Truef(t, ok, "field %q is missing from %s", name, typ.Name())
	assert.Equalf(t, want, f.Type,
		"field %q on %s has type %s, want %s", name, typ.Name(), f.Type, want)
}

package objfmt

// ObjectType is the 3-bit type field encoded in pack object headers.
//
// Numeric values match canonical Git's `enum object_type` in
// [object.h:97-107]:
//
//	OBJ_COMMIT    = 1
//	OBJ_TREE      = 2
//	OBJ_BLOB      = 3
//	OBJ_TAG       = 4
//	OBJ_OFS_DELTA = 6
//	OBJ_REF_DELTA = 7
//
// Value 5 is reserved for future expansion. The zero value is not a
// valid pack type and is reported as the empty string by [String].
//
// [object.h:97-107]: https://github.com/git/git/blob/v2.54.0/object.h#L97-L107
type ObjectType uint8

// Pack object types as encoded in the 3-bit type field of a pack
// object header. See [object.h:97-107] in canonical Git for the
// authoritative numeric values.
//
// [object.h:97-107]: https://github.com/git/git/blob/v2.54.0/object.h#L97-L107
const (
	TypeCommit   ObjectType = 1
	TypeTree     ObjectType = 2
	TypeBlob     ObjectType = 3
	TypeTag      ObjectType = 4
	TypeOfsDelta ObjectType = 6
	TypeRefDelta ObjectType = 7
)

// String returns the canonical lowercase name for a non-delta object
// type — `commit`, `tree`, `blob`, or `tag` — and the empty string for
// delta types or any unknown value. Delta types resolve to one of the
// non-delta types after the pack reader applies the delta chain, so a
// delta has no user-visible type name of its own.
func (t ObjectType) String() string {
	switch t {
	case TypeCommit:
		return "commit"
	case TypeTree:
		return "tree"
	case TypeBlob:
		return "blob"
	case TypeTag:
		return "tag"
	default:
		return ""
	}
}

// IsDelta reports whether t is [TypeOfsDelta] or [TypeRefDelta].
func (t ObjectType) IsDelta() bool {
	return t == TypeOfsDelta || t == TypeRefDelta
}

package lsremote

// UnbornRef represents the reference to an "unborn" state in Git, indicating
// the initial state of a newly created branch without any commits.
const UnbornRef = ObjectID("unborn")

// ObjectID represents the hash of a Git object within the repository. It is
// stored as a hexadecimal string, and serves as a unique identifier for
// various objects like commits and tags.
type ObjectID string

// Equals returns true if the ObjectID is equal to the other ObjectID.
func (oid ObjectID) Equals(other ObjectID) bool {
	return oid == other
}

// IsUnborn returns true if the ObjectID is equal to the UnbornRef.
func (oid ObjectID) IsUnborn() bool {
	return oid.Equals(UnbornRef)
}

// String returns the string representation of an ObjectID.
func (oid ObjectID) String() string {
	return string(oid)
}

// SymRefTarget is the target of a symbolic reference.
type SymRefTarget string

// Equals returns true if the SymRefTarget is equal to the other string.
func (s SymRefTarget) Equals(other string) bool {
	return s.String() == other
}

// String returns the string representation of a SymRefTarget.
func (s SymRefTarget) String() string {
	return string(s)
}

// Reference represents a reference within the repository, which can be a HEAD,
// a branch, an (annotated) tag, or a symbolic reference.
//
// This struct is primarily designed to capture the output of the `ls-refs`
// command from Git Protocol v2, while remaining compatible with the outputs of
// both Git Protocol v1 and the dumb protocol.
//
// In the context of Git Protocol v2, the reference format is as follows:
//
//	obj-id-or-unborn = (obj-id | "unborn")
//	ref = PKT-LINE(obj-id-or-unborn SP refname *(SP ref-attribute) LF)
//	ref-attribute = (symref | peeled)
//	symref = "symref-target:" symref-target
//	peeled = "peeled:" obj-id
//
// For further information, consult: https://git-scm.com/docs/protocol-v2#_ls_refs
type Reference struct {
	// ID is the identifier of the referenced object (commit, tag, etc.).
	ID ObjectID

	// Name is the name of the reference (e.g., branch name, tag name).
	Name string

	// Attributes holds optional attributes associated with the reference.
	Attributes *Attributes
}

// Attributes holds the attributes related to a reference.
type Attributes struct {
	// SymRef is the symbolic reference target, if applicable.
	SymRef SymRefTarget

	// Peeled is the ID of the referenced object after peeling, which represents the target
	// commit of an annotated tag, if applicable.
	Peeled ObjectID
}

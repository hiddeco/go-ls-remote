package objstore

// looseObjects reads objects stored under
// `<commonDir>/objects/<aa>/<rest-of-hash>` — the canonical loose
// object layout. Always present alongside whichever pack backend the
// store opener selects, since loose objects can coexist with packs.
//
// This file carries the type and constructor only. Hash lookup,
// fanout-prefix matching, and zlib framing land in a follow-up.
type looseObjects struct {
	commonDir string
}

// openLoose constructs a [looseObjects] backed by commonDir. It must
// succeed on a well-formed empty repository — `objects/` may be empty
// or missing entries entirely; per-object errors surface from
// [looseObjects.Find] once that method exists.
func openLoose(commonDir string) (*looseObjects, error) {
	return &looseObjects{commonDir: commonDir}, nil
}

// Close releases the backend. The skeleton holds no resources.
func (l *looseObjects) Close() error { return nil }

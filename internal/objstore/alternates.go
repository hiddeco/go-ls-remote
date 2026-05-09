package objstore

// openAlternates resolves the chain of alternate object stores reachable
// from `<commonDir>/objects/info/alternates` and returns each as a
// fully-constructed [Store]. Every alternate is opened recursively, so
// transitive alternates are flattened into one slice.
//
// This file carries the entry point only; the real implementation —
// path canonicalization, relative-path resolution, cycle detection
// (callers will eventually pass a `seen` set keyed by canonical paths),
// per-alternate algo inheritance — lands in a follow-up. The skeleton
// returns an empty chain so the store opener compiles and behaves
// correctly on the common case of no alternates.
func openAlternates(commonDir string, cfg storeConfig) ([]*Store, error) {
	return nil, nil
}

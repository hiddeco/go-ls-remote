package filet

// Option configures a [Transport] at construction time. Construct an
// Option via the package's `With*` helpers; the type is intentionally
// sealed so the option set cannot grow outside this package.
//
// No concrete options exist yet — the local-filesystem transport
// accepts a `file://` URL and nothing else. The type is exported now
// so [New]'s variadic signature is stable across follow-up commits
// that may introduce per-Transport tuning (for example a hook that
// swaps in a test object-store backend).
type Option interface {
	apply(*Transport)
}

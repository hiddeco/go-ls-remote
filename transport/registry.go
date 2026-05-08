package transport

import "strings"

// Registry is an explicit map of URL scheme to [Transport]. The library
// never registers transports via `init()` side effects; callers compose
// a Registry by constructing one with [NewRegistry] and (optionally)
// adding more transports via [Registry.Register].
//
// A nil *Registry is not usable; pass a non-nil instance even when
// empty.
//
// Registry is safe for concurrent reads after construction. Concurrent
// [Registry.Register] calls require external synchronisation.
type Registry struct {
	byScheme map[string]Transport
}

// NewRegistry returns a [Registry] populated with the given transports.
// Each transport contributes its [Transport.Schemes]; if two transports
// claim the same scheme the later one wins.
//
// Schemes are stored case-insensitively (lower-cased on entry); lookups
// are likewise case-insensitive.
func NewRegistry(ts ...Transport) *Registry {
	r := &Registry{byScheme: make(map[string]Transport, len(ts))}
	for _, t := range ts {
		r.Register(t)
	}
	return r
}

// Register adds t to the registry. For each scheme t claims, the entry
// replaces any prior binding (last writer wins).
func (r *Registry) Register(t Transport) {
	for _, s := range t.Schemes() {
		r.byScheme[strings.ToLower(s)] = t
	}
}

// Lookup returns the [Transport] bound to scheme, matched
// case-insensitively. The boolean is false when no transport is bound.
func (r *Registry) Lookup(scheme string) (Transport, bool) {
	t, ok := r.byScheme[strings.ToLower(scheme)]
	return t, ok
}

// Schemes returns all registered scheme names, in lowercase. The
// returned slice is freshly allocated; callers may sort or append to
// it without affecting the Registry. Order of the returned slice is
// unspecified.
func (r *Registry) Schemes() []string {
	out := make([]string, 0, len(r.byScheme))
	for s := range r.byScheme {
		out = append(out, s)
	}
	return out
}

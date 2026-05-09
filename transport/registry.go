package transport

import (
	"maps"
	"slices"
	"strings"
)

// Registry is an explicit map of URL scheme to [Transport]. The library
// never registers transports via `init()` side effects; callers compose
// a Registry by constructing one with [NewRegistry] and (optionally)
// adding more transports via [Registry.Register].
//
// A nil *Registry is not usable; pass a non-nil instance even when
// empty.
//
// # Concurrency
//
// Registry is safe for concurrent reads after construction:
// [Registry.Lookup] and [Registry.Schemes] may be called from any
// number of goroutines. [Registry.Register] writes the underlying map,
// so concurrent Register calls — or any Register concurrent with
// Lookup or Schemes — require external synchronisation. The intended
// pattern is to populate the Registry once at start-up and treat it
// as read-only thereafter.
type Registry struct {
	byScheme map[string]Transport
}

// NewRegistry returns a [Registry] populated with the given transports.
// Each transport contributes its [Transport.Schemes]; if two transports
// claim the same scheme the later one wins.
//
// Every Transport in ts must be non-nil. Passing nil panics — the
// Registry is constructed once at start-up and a nil entry would
// poison every subsequent [Registry.Lookup] for the schemes it
// would have claimed, so the failure is forced to be loud.
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
//
// Register panics if t is nil; see [NewRegistry] for the rationale.
func (r *Registry) Register(t Transport) {
	if t == nil {
		panic("transport: Register called with nil Transport")
	}
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
// unspecified — Go's map-iteration order is randomised on each call.
func (r *Registry) Schemes() []string {
	return slices.Collect(maps.Keys(r.byScheme))
}

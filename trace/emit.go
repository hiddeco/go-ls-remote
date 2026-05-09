package trace

// IsEnabled reports whether t is a non-nil Tracer that wants events.
//
// Code on hot paths (pkt-line read/write, per-byte HTTP body events)
// gates expensive event construction — `time.Now`, slice copies,
// fmt.Sprintf — behind an IsEnabled check so the nil-tracer case
// stays allocation-free:
//
//	if trace.IsEnabled(t) {
//	    t.OnEvent(trace.PacketEvent{Time: time.Now(), ...})
//	}
//
// Cold-path emitters that build trivial events should prefer [Emit].
func IsEnabled(t Tracer) bool { return t != nil }

// Emit reports e to t if t is non-nil and is otherwise a no-op.
//
// The event value is constructed by the caller before the call, so
// Emit does not avoid construction cost — gate cold-path sites with
// the helper to centralise the nil semantics, and gate hot-path sites
// with [IsEnabled] so the event struct (and any `time.Now` lookup it
// embeds) is skipped entirely when no tracer is wired up.
func Emit(t Tracer, e Event) {
	if t != nil {
		t.OnEvent(e)
	}
}

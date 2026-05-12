//go:build race

package server

// raceEnabled reports whether the test binary was built with `-race`.
// Race instrumentation perturbs `sync.Pool`'s steady state: the
// extra heap pressure from the race runtime triggers GCs more
// frequently, and `sync.Pool` discards items from its per-P caches
// on every GC cycle (see `sync/pool.go` and
// `runtime/mgc.go::poolCleanup`). The pack-resolution path under
// `Store.ObjectInfo` uses pooled scratch buffers, so periodic
// cold-path reallocation inflates the per-OID alloc average. The
// race-mode budget in
// [TestEmitObjectInfoLine_AllocBudget] carries that inflation; the
// non-race budget does not.
const raceEnabled = true

//go:build race

package objfmt

// raceEnabled reports whether the test binary was built with `-race`.
// Race instrumentation perturbs `sync.Pool`'s steady state: the extra
// heap pressure from the race runtime triggers GCs more frequently,
// and `sync.Pool` discards items from its per-P caches on every GC
// cycle (see `sync/pool.go` and `runtime/mgc.go::poolCleanup`). With
// the pool draining mid-loop, the cold path in [Pack.ReadDeltaHeader]
// (`zlib.NewReader` instead of `zlib.Resetter.Reset`) fires a small
// percentage of the time, inflating the per-call alloc average by
// roughly one. The race-mode budget in
// [TestPack_ReadDeltaHeader_AllocsAfterWarmup] carries that
// inflation; the non-race budget does not.
const raceEnabled = true

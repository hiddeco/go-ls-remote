package reftable

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests in this file pin the concurrency contract documented on
// [Reader] and [Stack]: read methods are safe for concurrent use by
// any number of goroutines after construction, while Close is the
// caller's responsibility to serialize. Run under `go test -race` to
// let the race detector flag any unsynchronized writes.
//
// Each goroutine inserts a [runtime.Gosched] after every hammer
// iteration so the Go scheduler rotates fairly across all 32 goroutines
// inside the 100ms time budget. The allocation-free [Stack.IterRefs]
// path is otherwise tight enough that with goroutines >> CPUs the
// sysmon's 10ms preemption cadence can leave some goroutines unrun
// before the time budget elapses, even though the contract under test
// (concurrent reads remain safe and produce correct results) is met.
// The yield does not change what is being tested; it only ensures the
// per-goroutine progress assertion is observed deterministically.

const concurrentGoroutines = 32

// concurrentNames lists ref names that exist in the with-index-sha1
// fixture; FindRef must return ok=true for each. Mixing in HEAD and
// refs/heads/main exercises both the symref and value-record code
// paths.
var concurrentNames = []string{
	"HEAD",
	"refs/heads/main",
	"refs/heads/branch-1",
	"refs/heads/branch-50",
	"refs/heads/branch-120",
}

func TestReader_concurrent_IterRefs_and_FindRef(t *testing.T) {
	r, err := OpenReader[objfmt.SHA1Hash](fixturePath(t, "with-index-sha1/0001-0001-aaaaaaaa.ref"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	var (
		wg       sync.WaitGroup
		stop     atomic.Bool
		findHits = make([]int, concurrentGoroutines)
		iterRuns = make([]int, concurrentGoroutines)
	)

	for id := range concurrentGoroutines {
		wg.Go(func() {
			for !stop.Load() {
				// Walk the iterator end-to-end at least once per round.
				for _, err := range r.IterRefs() {
					if err != nil {
						t.Errorf("goroutine %d: IterRefs error: %v", id, err)
						return
					}
				}
				iterRuns[id]++

				for _, name := range concurrentNames {
					rec, ok, err := r.FindRef(name)
					if err != nil {
						t.Errorf("goroutine %d: FindRef(%q) error: %v", id, name, err)
						return
					}
					if !ok {
						t.Errorf("goroutine %d: FindRef(%q) miss", id, name)
						return
					}
					if rec.Name != name {
						t.Errorf("goroutine %d: FindRef(%q) returned name %q", id, name, rec.Name)
						return
					}
					findHits[id]++
				}
				runtime.Gosched()
			}
		})
	}

	// Let the goroutines hammer the reader for a short, fixed window.
	// The race detector observes every read; the duration is just a
	// heuristic for "enough work to surface a race if one exists".
	time.Sleep(100 * time.Millisecond)
	stop.Store(true)
	wg.Wait()

	for i := range concurrentGoroutines {
		assert.Greater(t, findHits[i], 0, "goroutine %d completed no FindRef hits", i)
		assert.Greater(t, iterRuns[i], 0, "goroutine %d completed no IterRefs walks", i)
	}
}

func TestReader_close_idempotent_after_concurrent_reads(t *testing.T) {
	r, err := OpenReader[objfmt.SHA1Hash](fixturePath(t, "single-sha1/0001-0001-aaaaaaaa.ref"))
	require.NoError(t, err)

	// Spawn a handful of readers, let each finish a fixed amount of
	// work, then Close twice from the main goroutine after they have
	// returned. This is the supported shape: Close serializes against
	// in-flight reads, and idempotency is a property of "Close called
	// twice" rather than "Close called twice concurrently".
	const workers = 8
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for range 50 {
				rec, ok, err := r.FindRef("refs/heads/main")
				if err != nil {
					t.Errorf("FindRef error: %v", err)
					return
				}
				if !ok {
					t.Errorf("FindRef miss for refs/heads/main")
					return
				}
				if rec.Name != "refs/heads/main" {
					t.Errorf("FindRef returned name %q", rec.Name)
					return
				}
			}
		})
	}
	wg.Wait()

	require.NoError(t, r.Close())
	require.NoError(t, r.Close(), "second Close on a closed Reader must return nil")
}

func TestStack_concurrent_IterRefs_and_FindRef(t *testing.T) {
	s, err := OpenStack[objfmt.SHA1Hash](stackDir(t, "with-index-sha1"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	var (
		wg       sync.WaitGroup
		stop     atomic.Bool
		findHits = make([]int, concurrentGoroutines)
		iterRuns = make([]int, concurrentGoroutines)
	)

	for id := range concurrentGoroutines {
		wg.Go(func() {
			for !stop.Load() {
				for _, err := range s.IterRefs() {
					if err != nil {
						t.Errorf("goroutine %d: IterRefs error: %v", id, err)
						return
					}
				}
				iterRuns[id]++

				for _, name := range concurrentNames {
					rec, ok, err := s.FindRef(name)
					if err != nil {
						t.Errorf("goroutine %d: FindRef(%q) error: %v", id, name, err)
						return
					}
					if !ok {
						t.Errorf("goroutine %d: FindRef(%q) miss", id, name)
						return
					}
					if rec.Name != name {
						t.Errorf("goroutine %d: FindRef(%q) returned name %q", id, name, rec.Name)
						return
					}
					findHits[id]++
				}
				runtime.Gosched()
			}
		})
	}

	time.Sleep(100 * time.Millisecond)
	stop.Store(true)
	wg.Wait()

	for i := range concurrentGoroutines {
		assert.Greater(t, findHits[i], 0, "goroutine %d completed no FindRef hits", i)
		assert.Greater(t, iterRuns[i], 0, "goroutine %d completed no IterRefs walks", i)
	}
}

package lsremote_test

import (
	"context"
	"sort"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	lsremote "github.com/hiddeco/go-ls-remote"
	"github.com/hiddeco/go-ls-remote/internal/inttest"
)

// TestSession_ConcurrentHTTP exercises the public contract that an
// HTTP-backed [lsremote.Session] is safe for concurrent use without
// external synchronisation. Eight goroutines share one Session and
// each issues [Session.ListRefs] and [Session.ObjectInfo] in a loop;
// the underlying transport.Conn must multiplex the commands across
// independent POSTs. Under `-race` this also pins the absence of a
// data race in the Session-to-transport plumbing.
//
// The test runs once per HTTP variant (`http` and `https`) because
// the TLS-aware harness reaches the HTTP transport through a
// configured [http.Client], and a regression that serialises the
// transport-level Conn would surface on both paths.
func TestSession_ConcurrentHTTP(t *testing.T) {
	for _, name := range []string{"http", "https"} {
		t.Run(name, func(t *testing.T) {
			runHTTPConcurrent(t, lookupTransport(t, name))
		})
	}
}

// TestSession_SerialisedNonHTTP exercises the public contract that the
// non-HTTP transports require external serialisation. Eight goroutines
// share one Session through an external mutex; each grabs the lock and
// performs [Session.ListRefs] plus [Session.ObjectInfo]. The mutex
// satisfies the documented requirement, so the test asserts only that
// every goroutine observes the same wire-derived results — a
// regression that broke the serial path would surface here.
func TestSession_SerialisedNonHTTP(t *testing.T) {
	for _, name := range []string{"ssh", "git", "file"} {
		t.Run(name, func(t *testing.T) {
			runSerialisedConcurrent(t, lookupTransport(t, name))
		})
	}
}

// lookupTransport returns the [transportSetup] whose `name` matches
// the requested transport. The cross-transport equivalence suite owns
// the canonical list, so resolving by name here keeps the two suites
// in lockstep without duplicating the harness boilerplate.
func lookupTransport(t *testing.T, name string) transportSetup {
	t.Helper()
	for _, tp := range transports() {
		if tp.name == name {
			return tp
		}
	}
	t.Fatalf("unknown transport %q", name)
	return transportSetup{}
}

// concurrentEntry is the fixture every concurrency case runs against.
// `loose-objects` carries a fixed set of real on-disk OIDs the matrix
// declares, which the test exercises through [Session.ObjectInfo].
func concurrentEntry(t *testing.T) inttest.Entry {
	t.Helper()
	for _, e := range inttest.Entries() {
		if e.Name == "loose-objects" {
			return e
		}
	}
	t.Fatalf("matrix is missing the `loose-objects` fixture")
	return inttest.Entry{}
}

// runHTTPConcurrent drives 8 goroutines through one HTTP-backed
// Session, asserting that every iteration's ListRefs and ObjectInfo
// observe results consistent with a serial baseline captured before
// the goroutines start.
func runHTTPConcurrent(t *testing.T, tp transportSetup) {
	t.Helper()

	entry := concurrentEntry(t)
	gitdir := entry.Materialize(t)
	ep := tp.start(t, entry, gitdir)

	ctx := context.Background()
	opts := []lsremote.Option{
		lsremote.WithTransports(ep.registry),
		lsremote.WithProtocol(lsremote.ProtocolV2),
	}

	session, err := lsremote.Dial(ctx, ep.url, opts...)
	require.NoError(t, err, "Dial(%s)", ep.url)
	t.Cleanup(func() { _ = session.Close() })

	// Capture the serial baseline once. The concurrency assertions
	// reduce to "every goroutine's result matches this" without
	// needing per-iteration agreement on a moving target.
	args := lsremote.RefsRequest{
		Symrefs: true,
		Peel:    true,
		Unborn:  entry.Unborn,
	}
	wantRefs, err := session.ListRefs(ctx, args)
	require.NoError(t, err)

	oids := sortedOIDs(entry.ExpectedObjectInfo)
	wantInfos, err := session.ObjectInfo(ctx, oids,
		lsremote.ObjectInfoRequest{Size: true})
	require.NoError(t, err)

	const goroutines = 8
	const iterations = 4

	var wg sync.WaitGroup
	for range goroutines {
		wg.Go(func() {
			for range iterations {
				gotRefs, err := session.ListRefs(ctx, args)
				assert.NoError(t, err)
				assert.Equal(t, wantRefs, gotRefs,
					"ListRefs result must be stable across concurrent calls")

				gotInfos, err := session.ObjectInfo(ctx, oids,
					lsremote.ObjectInfoRequest{Size: true})
				assert.NoError(t, err)
				assert.Equal(t, wantInfos, gotInfos,
					"ObjectInfo result must be stable across concurrent calls")
			}
		})
	}
	wg.Wait()
}

// runSerialisedConcurrent drives 8 goroutines through one Session over
// a non-HTTP transport, with each goroutine acquiring an external
// mutex around every Session call. The transport contract requires
// callers to serialise; the mutex satisfies that. The assertion is
// the same shape as the HTTP variant: every goroutine observes
// results consistent with a serial baseline.
func runSerialisedConcurrent(t *testing.T, tp transportSetup) {
	t.Helper()

	entry := concurrentEntry(t)
	gitdir := entry.Materialize(t)
	ep := tp.start(t, entry, gitdir)

	ctx := context.Background()
	opts := []lsremote.Option{
		lsremote.WithTransports(ep.registry),
		lsremote.WithProtocol(lsremote.ProtocolV2),
	}

	session, err := lsremote.Dial(ctx, ep.url, opts...)
	require.NoError(t, err, "Dial(%s)", ep.url)
	t.Cleanup(func() { _ = session.Close() })

	args := lsremote.RefsRequest{
		Symrefs: true,
		Peel:    true,
		Unborn:  entry.Unborn,
	}
	wantRefs, err := session.ListRefs(ctx, args)
	require.NoError(t, err)

	oids := sortedOIDs(entry.ExpectedObjectInfo)
	wantInfos, err := session.ObjectInfo(ctx, oids,
		lsremote.ObjectInfoRequest{Size: true})
	require.NoError(t, err)

	const goroutines = 8
	const iterations = 4

	var mu sync.Mutex
	var wg sync.WaitGroup
	for range goroutines {
		wg.Go(func() {
			for range iterations {
				mu.Lock()
				gotRefs, refsErr := session.ListRefs(ctx, args)
				gotInfos, infosErr := session.ObjectInfo(ctx, oids,
					lsremote.ObjectInfoRequest{Size: true})
				mu.Unlock()

				assert.NoError(t, refsErr)
				assert.Equal(t, wantRefs, gotRefs,
					"serialised ListRefs must match the baseline")
				assert.NoError(t, infosErr)
				assert.Equal(t, wantInfos, gotInfos,
					"serialised ObjectInfo must match the baseline")
			}
		})
	}
	wg.Wait()
}

// sortedOIDs returns the keys of want as a sorted slice so the
// per-call `oids` argument is stable across baseline and per-goroutine
// invocations; ObjectInfo preserves caller order on the returned
// slice, and a stable input order keeps the equality assertion
// meaningful.
func sortedOIDs(want map[string]int64) []string {
	out := make([]string, 0, len(want))
	for k := range want {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

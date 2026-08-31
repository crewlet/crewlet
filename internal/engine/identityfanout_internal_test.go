package engine

import (
	"sync"
	"testing"
	"time"
)

// THE FAN-OUT IS BOUNDED, and nothing bounded it.
//
// Three resolvers — GitHub, GitLab, Jira — each open one HTTPS call per
// distinct seat credential, on the config-apply path. A company of thirty
// seats therefore opened thirty simultaneous connections to one vendor at
// every boot and every apply. github.go's comment asserted a bound that was
// not a bound on anything the host controls.
func TestIdentityLookupsRunAtMostTheCapAtOnce(t *testing.T) {
	t.Parallel()
	const work = 64

	var (
		mu       sync.Mutex
		inFlight int
		peak     int
		done     int
	)
	resolveConcurrently(work, func(int) {
		mu.Lock()
		inFlight++
		peak = max(peak, inFlight)
		mu.Unlock()

		// HELD, so callers actually overlap. A vendor lookup is an HTTPS
		// round trip; a body that returns immediately lets each goroutine
		// finish before the next starts, and the peak then stays at one
		// whether or not anything bounds it — which is a test that cannot
		// fail. 20ms is enough to make 64 unbounded starts visible and
		// keeps the bounded run to eight waves.
		time.Sleep(20 * time.Millisecond)

		mu.Lock()
		inFlight--
		done++
		mu.Unlock()
	})

	if done != work {
		t.Errorf("ran %d of %d lookups: the bound must not drop work", done, work)
	}
	if peak > identityLookups {
		t.Errorf("peak in-flight = %d, want at most %d", peak, identityLookups)
	}
}

// AND EVERY INDEX IS VISITED EXACTLY ONCE, which is what the callers rely on:
// each writes its own slot in a shared slice, so a repeated or skipped index
// would silently give a seat another seat's identity or none at all.
func TestEveryIdentityIndexIsVisitedOnce(t *testing.T) {
	t.Parallel()
	const work = 50
	seen := make([]int, work)
	var mu sync.Mutex

	resolveConcurrently(work, func(i int) {
		mu.Lock()
		defer mu.Unlock()
		seen[i]++
	})

	for i, n := range seen {
		if n != 1 {
			t.Fatalf("index %d visited %d times, want exactly once", i, n)
		}
	}
}

// An empty credential set does nothing and does not block, which is the
// overwhelmingly common case: credentials change rarely, so almost every
// apply resolves nothing at all.
func TestNoCredentialsResolvesNothing(t *testing.T) {
	t.Parallel()
	called := false
	resolveConcurrently(0, func(int) { called = true })
	if called {
		t.Error("a resolve with no missing credentials still ran a lookup")
	}
}

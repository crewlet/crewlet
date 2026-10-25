package coord_test

import (
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
)

// A CLAIM ANSWERED AS HELD KEEPS EVERY PEER OUT THROUGH ITS EXCLUSIVE WINDOW.
//
// The window is what a holder may count on without asking again: work it does
// inside the window is work no other node can be doing under the same lease,
// because no other node can have claimed it yet. So at the window's last
// instant a peer's claim is still refused, and the window ends before the
// lease does — by the margin the holder named — while a peer is let in only
// once the lease itself has run out. A margin as long as the TTL leaves no
// window at all.
func TestAConfirmedClaimKeepsAPeerOutThroughItsExclusiveWindow(t *testing.T) {
	t.Parallel()
	var (
		mu  sync.Mutex
		now = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	)
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	set := func(at time.Time) {
		mu.Lock()
		defer mu.Unlock()
		now = at
	}
	store := &coordmemory.Backend{Clock: clock}
	resource := coord.WorkerResource("object-collector")
	const ttl, margin = 3 * time.Minute, time.Minute
	claim := func(owner string) bool {
		t.Helper()
		lease, _, err := store.TryAcquire(t.Context(), resource,
			coord.AcquireOptions{Owner: owner, TTL: ttl, Ungated: true})
		if err != nil {
			t.Fatal(err)
		}
		return lease != nil
	}

	asked := clock()
	if !claim("a:1") {
		t.Fatal("the premise: the first claim was refused")
	}
	through := coord.ExclusiveThrough(asked, ttl, margin)
	if want := asked.Add(ttl - margin); !through.Equal(want) {
		t.Fatalf("a claim asked at %v with a %v lease is exclusive through %v, want %v",
			asked, ttl, through, want)
	}
	set(through)
	if claim("b:1") {
		t.Fatal("a peer claimed the lease inside the holder's exclusive window")
	}
	set(asked.Add(ttl + time.Nanosecond))
	if !claim("b:1") {
		t.Fatal("a peer was kept out after the lease ran out: the premise of the window")
	}
	if got := coord.ExclusiveThrough(asked, ttl, ttl); got.After(asked) {
		t.Fatalf("a margin as long as the TTL leaves a window through %v, after the claim was asked", got)
	}
}

package storetest

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// The notification-valve cases. Here rather than package-local for the same
// reason the delivery claims are: the check-and-increment is a conditional
// upsert — ON CONFLICT … DO UPDATE … WHERE — which is exactly the shape one
// driver accepts and the other may not.

func testRateLimitAllowsUpToTheLimit(t *testing.T, db *store.DB) {
	valve := db.RateLimits()
	bucket := store.NotifyBucket("agent-1")

	for i := range 3 {
		ok, err := valve.Allow(t.Context(), bucket, 3, base)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if !ok {
			t.Fatalf("call %d of 3 was refused inside the limit", i+1)
		}
	}
	ok, err := valve.Allow(t.Context(), bucket, 3, base)
	if err != nil {
		t.Fatalf("fourth call: %v", err)
	}
	if ok {
		t.Fatal("the fourth call passed a limit of 3")
	}
	// And it stays shut for the rest of the window, rather than the
	// counter wrapping or the row being re-inserted.
	if ok, _ := valve.Allow(t.Context(), bucket, 3, base.Add(100*time.Millisecond)); ok {
		t.Fatal("the valve reopened inside the same window")
	}
}

// A limit of 1 is the tightest setting an operator can write, and it is the
// one the INSERT branch decides on its own — the WHERE never sees it.
func testRateLimitOfOne(t *testing.T, db *store.DB) {
	valve := db.RateLimits()
	bucket := store.NotifyBucket("agent-1")

	if ok, err := valve.Allow(t.Context(), bucket, 1, base); err != nil || !ok {
		t.Fatalf("the first call under a limit of 1 was refused: %v, %v", ok, err)
	}
	if ok, _ := valve.Allow(t.Context(), bucket, 1, base); ok {
		t.Fatal("a second call passed a limit of 1")
	}
}

func testRateLimitWindowsAreIndependent(t *testing.T, db *store.DB) {
	valve := db.RateLimits()
	bucket := store.NotifyBucket("agent-1")

	for range 2 {
		if ok, _ := valve.Allow(t.Context(), bucket, 2, base); !ok {
			t.Fatal("a call inside the limit was refused")
		}
	}
	if ok, _ := valve.Allow(t.Context(), bucket, 2, base); ok {
		t.Fatal("the window did not close")
	}
	// The next window starts fresh: the valve limits a rate, not a total.
	next := base.Add(store.NotifyWindow)
	if ok, err := valve.Allow(t.Context(), bucket, 2, next); err != nil || !ok {
		t.Fatalf("the next window did not open: %v, %v", ok, err)
	}
	// Instants inside one window share its counter, whatever their offset.
	mid := store.WindowStart(next, store.NotifyWindow).Add(store.NotifyWindow - time.Nanosecond)
	if ok, _ := valve.Allow(t.Context(), bucket, 2, mid); !ok {
		t.Fatal("the second call of the new window was refused")
	}
	if ok, _ := valve.Allow(t.Context(), bucket, 2, mid); ok {
		t.Fatal("an instant late in the window got its own counter")
	}
}

// One seat exhausting its allowance must not close the valve on another —
// the limit is per seat, which is what makes a loop between two of them
// trip both rather than starving the whole company.
func testRateLimitIsPerBucket(t *testing.T, db *store.DB) {
	valve := db.RateLimits()

	for range 2 {
		if ok, _ := valve.Allow(t.Context(), store.NotifyBucket("a"), 2, base); !ok {
			t.Fatal("a call inside the limit was refused")
		}
	}
	if ok, _ := valve.Allow(t.Context(), store.NotifyBucket("a"), 2, base); ok {
		t.Fatal("seat a's valve did not close")
	}
	if ok, err := valve.Allow(t.Context(), store.NotifyBucket("b"), 2, base); err != nil || !ok {
		t.Fatalf("seat b was refused because seat a was busy: %v, %v", ok, err)
	}
}

// Off by default: an operator who never asked for the valve pays nothing
// and is never refused.
func testRateLimitDisabled(t *testing.T, db *store.DB) {
	valve := db.RateLimits()
	for _, limit := range []int{0, -1} {
		for range 50 {
			ok, err := valve.Allow(t.Context(), store.NotifyBucket("a"), limit, base)
			if err != nil || !ok {
				t.Fatalf("limit %d refused a call: %v, %v", limit, ok, err)
			}
		}
	}
	// And nothing was written, so a company that never enables the valve
	// never grows the table.
	n, err := valve.Purge(t.Context(), base.Add(time.Hour))
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 0 {
		t.Fatalf("a disabled valve wrote %d rows", n)
	}
	// An empty bucket is likewise never limited: there is no seat to
	// attribute the count to, and refusing on one would drop real work.
	if ok, err := valve.Allow(t.Context(), "", 1, base); err != nil || !ok {
		t.Fatalf("an unattributed call was refused: %v, %v", ok, err)
	}
}

// The sweep must honour its cutoff. Clearing the table would reset the LIVE
// window too, and let a full limit's worth through again the instant
// housekeeping ran — a periodic hole in the rate limit.
func testRateLimitPurgeKeepsTheLiveWindow(t *testing.T, db *store.DB) {
	valve := db.RateLimits()
	bucket := store.NotifyBucket("agent-1")
	old := base.Add(-time.Hour)

	if _, err := valve.Allow(t.Context(), bucket, 1, old); err != nil {
		t.Fatalf("old window: %v", err)
	}
	if ok, _ := valve.Allow(t.Context(), bucket, 1, base); !ok {
		t.Fatal("the live window did not open")
	}

	n, err := valve.Purge(t.Context(), store.WindowStart(base, store.NotifyWindow))
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 1 {
		t.Fatalf("purge removed %d rows, want the one stale window", n)
	}
	if ok, _ := valve.Allow(t.Context(), bucket, 1, base); ok {
		t.Fatal("the sweep reset the live window")
	}
}

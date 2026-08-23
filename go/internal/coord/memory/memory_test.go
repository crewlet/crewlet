package memory_test

import (
	"context"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/coordtest"
	"github.com/crewlet/crewlet/internal/coord/memory"
)

// The twin is certified by the same suite as every other backend. A
// divergence between it and the store a fleet actually runs on is a failing
// test here rather than a production-only surprise on the one node that
// happens to run the other one.
func TestContract(t *testing.T) {
	coordtest.Run(t, func(t *testing.T) coord.Backend { return memory.New() })
}

// The zero value has to work: callers construct plain data without
// constructors, and a store that only functions when built by New would fail
// on a struct field somebody forgot to initialise.
func TestZeroValueIsUsable(t *testing.T) {
	t.Parallel()
	coordtest.Run(t, func(t *testing.T) coord.Backend { return new(memory.Backend) })
}

// Clock is what makes a caller's OWN test deterministic: it can put the
// store's clock wherever it needs a lease to lapse instead of waiting for one.
func TestInjectedClockDecidesExpiry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	b := &memory.Backend{Clock: func() time.Time { return now }}

	lease, err := b.TryAcquire(ctx, coord.SeatResource("ceo"), coord.AcquireOptions{
		Owner: "node-a:1", TTL: 30 * time.Second,
	})
	if err != nil || lease == nil {
		t.Fatalf("TryAcquire = (%v, %v)", lease, err)
	}
	if want := now.Add(30 * time.Second); !lease.ExpiresAt.Equal(want) {
		t.Fatalf("ExpiresAt = %v, want the store's clock plus the TTL (%v)", lease.ExpiresAt, want)
	}

	// Time does not pass on its own here — the store's clock is the only
	// clock, so the lease is still held however long the test takes.
	held, err := b.Get(ctx, coord.SeatResource("ceo"))
	if err != nil || held == nil {
		t.Fatalf("Get = (%v, %v), want the lease still held", held, err)
	}

	now = now.Add(31 * time.Second)
	if unheld, err := b.Get(ctx, coord.SeatResource("ceo")); err != nil || unheld != nil {
		t.Fatalf("Get = (%v, %v) past the deadline, want (nil, nil)", unheld, err)
	}
}

// A dead context is the one way this store cannot answer. Callers branch on
// unknown-versus-lost, and that branch must be reachable in a memory-backed
// test or it is only ever exercised in production.
func TestDeadContextIsUnknownNotRefusal(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b := memory.New()

	lease, err := b.TryAcquire(ctx, coord.SeatResource("ceo"), coord.AcquireOptions{
		Owner: "node-a:1", TTL: time.Minute,
	})
	if lease != nil {
		t.Fatal("TryAcquire granted a lease on a cancelled context")
	}
	if err == nil {
		t.Fatal("TryAcquire answered (nil, nil) — a call that never reached the store " +
			"reads as a peer holding the resource")
	}
	if _, err := b.ListLive(ctx, coord.NodePrefix); err == nil {
		t.Fatal("ListLive answered an empty fleet on a cancelled context")
	}
}

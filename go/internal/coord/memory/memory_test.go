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

	// EVERY verb, because a dead context is a point in this store's life and
	// the suite's job is to send each verb at each such point. Three of the
	// eight used to be sent here, and a mutation dropping the guard from any
	// of the other five passed the whole package — the contract suite could
	// not cover it either, since its fault injector short-circuits ABOVE the
	// backend and so exercises the wrapper rather than this code.
	//
	// What each of them must not do is answer definitely: nil, false or an
	// empty slice with no error all read as facts about ownership, and a call
	// that never reached the store knows no facts about ownership.
	verbs := []struct {
		name string
		call func() error
	}{
		{"TryAcquire", func() error {
			lease, err := b.TryAcquire(ctx, coord.SeatResource("ceo"), coord.AcquireOptions{
				Owner: "node-a:1", TTL: time.Minute,
			})
			if lease != nil {
				t.Error("TryAcquire granted a lease on a cancelled context")
			}
			return err
		}},
		{"Renew", func() error {
			ok, err := b.Renew(ctx, coord.SeatResource("ceo"), "node-a:1", 1, time.Minute)
			if ok {
				t.Error("Renew reported success on a cancelled context")
			}
			return err
		}},
		{"Release", func() error {
			ok, err := b.Release(ctx, coord.SeatResource("ceo"), "node-a:1", 1)
			if ok {
				t.Error("Release reported success on a cancelled context")
			}
			return err
		}},
		{"Get", func() error { _, err := b.Get(ctx, coord.SeatResource("ceo")); return err }},
		{"ListOwned", func() error { _, err := b.ListOwned(ctx, "node-a:1"); return err }},
		{"ListLive", func() error { _, err := b.ListLive(ctx, coord.NodePrefix); return err }},
		{"PreferredResources", func() error {
			_, err := b.PreferredResources(ctx, coord.SeatPrefix, "node-a")
			return err
		}},
		{"FleetProtocolFloor", func() error {
			floor, any, err := b.FleetProtocolFloor(ctx)
			if any {
				t.Errorf("FleetProtocolFloor reported a floor of %d on a cancelled context", floor)
			}
			return err
		}},
	}
	for _, v := range verbs {
		if err := v.call(); err == nil {
			t.Errorf("%s answered definitely on a cancelled context — an unreachable store "+
				"says nothing about ownership, and a caller reading that as fact sheds "+
				"seats it still holds", v.name)
		}
	}
}

// A meta payload that cannot be encoded is refused, and refused BEFORE the
// record is written.
//
// This is twin-only behaviour that the contract suite structurally cannot
// reach: every meta payload in coordtest is JSON-native by deliberate design,
// so no case there can hand the store something a codec would reject. A
// portable suite cannot hold a backend-specific path, which is exactly the
// gap where a defect sits unnoticed — measured, a silent drop here passes both
// packages.
//
// Pinned on the OBSERVABLE rather than on the error text: what a caller needs
// is that the claim did not appear to succeed with its profile quietly
// missing, because a node whose roles were dropped is one peers make placement
// decisions about against a profile it never published.
func TestUnencodableMetaIsRefusedNotDropped(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := memory.New()
	resource := coord.NodeResource("n1")

	lease, err := b.TryAcquire(ctx, resource, coord.AcquireOptions{
		Owner: "n1:a", TTL: time.Minute, Ungated: true,
		Meta: map[string]any{"roles": []any{"seats"}, "ch": make(chan int)},
	})
	if err == nil {
		t.Fatalf("TryAcquire = (%v, nil) for a payload no store could encode — a real "+
			"backend's write would fail on it, and answering success with the profile "+
			"silently gone is the one outcome a caller cannot detect", lease)
	}
	if lease != nil {
		t.Fatal("TryAcquire granted a lease it could not record the meta for")
	}
	// And nothing was written on the way to refusing.
	held, err := b.Get(ctx, resource)
	if err != nil {
		t.Fatalf("Get after a refused claim: %v", err)
	}
	if held != nil {
		t.Fatalf("the resource is held by %q after a claim that was refused", held.Owner)
	}
}

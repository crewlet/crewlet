package schedule_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/schedule"
)

func TestTheDutyIsHeldByExactlyOneNode(t *testing.T) {
	t.Parallel()
	backend := memory.New()
	a := schedule.ClaimDuty(backend, "node-a#1", "node-a", schedule.DefaultTick)
	b := schedule.ClaimDuty(backend, "node-b#1", "node-b", schedule.DefaultTick)
	ctx := t.Context()

	held, err := a(ctx)
	if err != nil || !held {
		t.Fatalf("a's first claim = %v, %v; want true, nil", held, err)
	}
	held, err = b(ctx)
	if err != nil {
		t.Fatalf("b's claim = %v — a peer holding the duty is a definite refusal, not an "+
			"unknown; conflating them makes a busy fleet look like a broken store", err)
	}
	if held {
		t.Fatal("two nodes hold the duty at once")
	}
	// The holder re-claims every tick, and TryAcquire doubles as a renew for
	// the current owner — so holding it costs one round trip and the holder
	// stays the holder while it keeps ticking.
	for range 3 {
		if held, err = a(ctx); err != nil || !held {
			t.Fatalf("a's re-claim = %v, %v; want true, nil", held, err)
		}
	}
}

func TestTheDutyMovesWhenItsHolderStopsTicking(t *testing.T) {
	t.Parallel()
	// The whole reason it is a claim per tick rather than a leader election:
	// a node that dies between ticks releases the duty by letting its lease
	// lapse. Nothing has to notice the death, because nothing was waiting
	// for the dead node to say anything.
	backend := memory.New()
	// A tick this short makes the TTL the floor (30s), which is what the
	// clock is then advanced past.
	tick := time.Second
	a := schedule.ClaimDuty(backend, "node-a#1", "node-a", tick)
	b := schedule.ClaimDuty(backend, "node-b#1", "node-b", tick)
	ctx := t.Context()

	if held, err := a(ctx); err != nil || !held {
		t.Fatalf("a's claim = %v, %v; want true, nil", held, err)
	}
	if held, _ := b(ctx); held {
		t.Fatal("b took a live duty")
	}

	backend.Advance(schedule.DutyTTL(tick) + time.Second)

	if held, err := b(ctx); err != nil || !held {
		t.Fatalf("b's claim after the TTL lapsed = %v, %v; want true, nil", held, err)
	}
}

func TestDutyTTLScalesWithTheTickAndHasAFloor(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		tick time.Duration
		want time.Duration
	}{
		// Three ticks of tolerance: the holder re-claims every tick, so the
		// duty survives two consecutive slow or failed claims without
		// moving. Moving it is not dangerous, but it is churn on a lease
		// the whole fleet reads.
		{"the shipped tick", 10 * time.Second, 30 * time.Second},
		// The floor. A very short tick would otherwise mint a very short
		// lease and re-claim it constantly, which is store traffic bought
		// for nothing.
		{"a very short tick", time.Second, 30 * time.Second},
		{"zero takes the default tick", 0, 30 * time.Second},
		// At the configured ceiling the TTL is 3 minutes, and that is the
		// number an operator is accepting when they raise the tick: a dead
		// holder's duty is unclaimable for up to one TTL, and the
		// successor's catchup pass replays only the MOST RECENT missed
		// fire. A per-minute schedule can therefore lose fires. Anyone
		// setting a tick near MaxTick has already accepted minute-scale
		// dispatch latency.
		{"the configured ceiling", 59 * time.Second, 177 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := schedule.DutyTTL(tc.tick); got != tc.want {
				t.Fatalf("DutyTTL(%v) = %v, want %v", tc.tick, got, tc.want)
			}
		})
	}
}

func TestAnUnreachableStorePassesUnknownThrough(t *testing.T) {
	t.Parallel()
	// The tri-state, unflattened. Collapsing an unreachable store into
	// "false" would hide from the log the difference between "a peer holds
	// it" and "the store could not be reached" — the same silence, and very
	// different situations.
	want := errors.New("store unreachable")
	duty := schedule.ClaimDuty(faultyBackend{err: want}, "node-a#1", "node-a", schedule.DefaultTick)
	held, err := duty(t.Context())
	if !errors.Is(err, want) {
		t.Fatalf("duty error = %v, want the backend's own %v", err, want)
	}
	if held {
		t.Fatal("an unreachable store granted the duty")
	}
}

func TestNoBackendIsNoDuty(t *testing.T) {
	t.Parallel()
	// Nil rather than a function that always says yes: the scheduler logs
	// whether it is a fleet singleton, and an always-true wrapper would make
	// a single node report itself as one.
	if duty := schedule.ClaimDuty(nil, "node-a#1", "node-a", schedule.DefaultTick); duty != nil {
		t.Fatal("ClaimDuty with no backend returned a DutyFunc")
	}
}

func TestTheDutyClaimIsUngated(t *testing.T) {
	t.Parallel()
	// A duty record left at an older protocol by a build that predates the
	// gate would block every seat claim fleet-wide the moment the version
	// moved, so a duty claim never gates on the fleet protocol floor. It
	// still carries THIS build's protocol, so it never becomes the thing
	// that blocks either.
	backend := memory.New()
	ctx := t.Context()

	// A peer holding an unrelated resource at protocol 1 — the situation the
	// gate exists for.
	if _, err := backend.TryAcquire(ctx, coord.SeatResource("legacy"), coord.AcquireOptions{
		Owner: "old-node#1", TTL: time.Minute, Protocol: 1,
	}); err != nil {
		t.Fatalf("seed the old lease: %v", err)
	}

	duty := schedule.ClaimDuty(backend, "node-a#1", "node-a", schedule.DefaultTick)
	if held, err := duty(ctx); err != nil || !held {
		t.Fatalf("duty claim beside a protocol-1 lease = %v, %v; want true, nil", held, err)
	}

	lease, err := backend.Get(ctx, coord.WorkerResource(schedule.DutyName))
	if err != nil || lease == nil {
		t.Fatalf("Get(worker:scheduler) = %v, %v", lease, err)
	}
	if lease.Protocol != coord.ProtocolVersion {
		t.Fatalf("the duty was claimed at protocol %d, want this build's %d — a duty record "+
			"below the floor blocks every seat claim in the fleet",
			lease.Protocol, coord.ProtocolVersion)
	}
	if lease.Preferred != "node-a" {
		t.Fatalf("stickiness hint = %q, want the stable node id so a restarted node tends to "+
			"get its own duty back", lease.Preferred)
	}
}

// faultyBackend answers every call with one error. Only TryAcquire is reached
// by ClaimDuty; the rest exist to satisfy the interface and say so if that
// ever changes.
type faultyBackend struct{ err error }

func (f faultyBackend) TryAcquire(context.Context, string, coord.AcquireOptions) (*coord.Lease, error) {
	return nil, f.err
}
func (f faultyBackend) Renew(context.Context, string, string, int64, time.Duration) (bool, error) {
	return false, f.err
}
func (f faultyBackend) Release(context.Context, string, string, int64) (bool, error) {
	return false, f.err
}
func (f faultyBackend) Get(context.Context, string) (*coord.Lease, error) { return nil, f.err }
func (f faultyBackend) ListOwned(context.Context, string) ([]coord.Lease, error) {
	return nil, f.err
}
func (f faultyBackend) ListLive(context.Context, string) ([]coord.Lease, error) {
	return nil, f.err
}
func (f faultyBackend) PreferredResources(context.Context, string, string) (map[string]struct{}, error) {
	return nil, f.err
}
func (f faultyBackend) FleetProtocolFloor(context.Context) (int, bool, error) {
	return 0, false, f.err
}

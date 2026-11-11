package observe_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/eventfan"
	"github.com/crewlet/crewlet/internal/observe"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tokens"
)

// bootBudget stands in for the handover's wait in these cases; nothing they
// claim depends on its length, only on a read outlasting it.
const bootBudget = 5 * time.Second

// A SEED THAT MISSES THE BOOT'S BUDGET STILL LANDS, and the projection says the
// seed is complete once it does.
//
// The boot gave the history seed one attempt under one deadline, so on a store
// whose reads outlasted it the feed, the spend window and every seat's last
// turn started at the boot for the life of the process, and `/health` answered
// `seeded_from.complete: false` until the next restart.
//
// Mutation: cancel the seeds still being read when Seeding.Wait's budget runs
// out, as the boot's deadline did, and the projection is never seeded.
func TestASeedThatMissesTheBootBudgetStillLands(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		live := livestate.New()
		history := &heldHistory{release: make(chan struct{})}
		seeding := observe.StartSeeding(t.Context(), observe.HistorySeed(history, noOrg, live))
		defer seeding.Stop()

		pending := seeding.Wait(t.Context(), bootBudget)
		if !slices.Equal(pending, []string{"the live projection's history"}) {
			t.Fatalf("pending at the handover = %v, want the history seed", pending)
		}
		if _, seeded := live.SeededFrom(); seeded {
			t.Fatal("the projection records a seed whose read has not returned")
		}

		close(history.release)
		if pending := seeding.Wait(t.Context(), time.Hour); len(pending) > 0 {
			t.Fatalf("still pending after its read returned: %v", pending)
		}
		coverage, seeded := live.SeededFrom()
		if !seeded || !coverage.Complete {
			t.Errorf("seeded from %+v (recorded %v), want the late seed complete", coverage, seeded)
		}
		if rows := live.RecentEvents(0); len(rows) != 1 {
			t.Errorf("the feed holds %d rows, want the one the late seed read", len(rows))
		}
	})
}

// A SEED THAT DID NOT COMPLETE IS READ AGAIN UNTIL IT DOES: a read that failed,
// and a read a node did not answer, which is incomplete without any error.
//
// Mutation: return after the first attempt whatever it answered, and both
// seeds stay incomplete.
func TestASeedIsReadAgainUntilItCompletes(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		for _, c := range []struct {
			name    string
			history *flakyHistory
		}{
			{"a read that failed", &flakyHistory{fail: 3}},
			{"a node that did not answer", &flakyHistory{silent: 3}},
		} {
			live := livestate.New()
			seeding := observe.StartSeeding(t.Context(), observe.HistorySeed(c.history, noOrg, live))
			if pending := seeding.Wait(t.Context(), time.Hour); len(pending) > 0 {
				t.Errorf("%s: still pending after an hour: %v", c.name, pending)
			}
			seeding.Stop()
			if got := c.history.attempts(); got != 4 {
				t.Errorf("%s: %d attempts, want three that did not complete and the one that did",
					c.name, got)
			}
			if coverage, _ := live.SeededFrom(); !coverage.Complete {
				t.Errorf("%s: seeded from %+v, want the last attempt's complete coverage", c.name, coverage)
			}
		}
	})
}

// A SEED THAT NEVER COMPLETES IS READ AT A BOUNDED RATE: doubling from the fleet
// read budget, and never more often than the reconcile interval once there.
//
// Mutation: retry at a fixed short wait, and the attempts crowd together.
func TestASeedThatNeverCompletesIsReadAtABoundedRate(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		history := &flakyHistory{silent: -1}
		seeding := observe.StartSeeding(t.Context(), observe.HistorySeed(history, noOrg, livestate.New()))
		if pending := seeding.Wait(t.Context(), time.Hour); len(pending) != 1 {
			t.Fatalf("pending after an hour = %v, want the seed that cannot complete", pending)
		}
		seeding.Stop()
		at := history.instants()
		if len(at) < 10 {
			t.Fatalf("%d attempts in an hour, want it still being read", len(at))
		}
		// Jitter spreads a wait by a fifth either way.
		floor := func(d time.Duration) time.Duration { return d * 4 / 5 }
		for i := 1; i < len(at); i++ {
			gap := at[i].Sub(at[i-1])
			want := floor(eventfan.FleetReadBudget)
			if i >= 6 {
				want = floor(livestate.ReconcileInterval)
			}
			if gap < want {
				t.Errorf("attempt %d came %s after the one before, want at least %s", i+1, gap, want)
			}
		}
	})
}

// THE SEEDS ARE READ SIDE BY SIDE: a slow history seed does not hold the
// pauses and the executors behind it, which read the coordination store in
// milliseconds and failed every boot on the deadline the history seed spent.
//
// Mutation: attempt the seeds one after another, and the pause seed is still
// pending at the handover.
func TestASlowSeedDoesNotHoldTheOthers(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		history := &heldHistory{release: make(chan struct{})}
		var paused atomic.Bool
		seeding := observe.StartSeeding(t.Context(),
			observe.HistorySeed(history, noOrg, livestate.New()),
			observe.BootSeed{Name: "the paused seats", NotSeeded: "seat_pauses_not_seeded",
				Seeded: "seat_pauses_seeded", Attempt: func(ctx context.Context) error {
					paused.Store(ctx.Err() == nil)
					return ctx.Err()
				}})
		defer seeding.Stop()
		pending := seeding.Wait(t.Context(), bootBudget)
		if !slices.Equal(pending, []string{"the live projection's history"}) {
			t.Errorf("pending at the handover = %v, want the slow history seed alone", pending)
		}
		if !paused.Load() {
			t.Error("the pause seed did not land beside the slow history seed")
		}
		close(history.release)
	})
}

// STOPPING THE SEEDING ENDS A READ IN FLIGHT AND READS NOTHING AFTER IT.
//
// Mutation: run the seeds on a context Stop does not cancel, and Stop never
// returns.
func TestStoppingTheSeedingEndsItsReads(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		history := &heldHistory{release: make(chan struct{})}
		seeding := observe.StartSeeding(t.Context(), observe.HistorySeed(history, noOrg, livestate.New()))
		synctest.Wait()
		seeding.Stop()
		if got := history.asked(); got != 1 {
			t.Errorf("%d attempts after Stop, want the one in flight and no more", got)
		}
	})
}

// noOrg is a node serving no company, so the seed asks no seat's turns.
func noOrg() *org.Organization { return nil }

// heldHistory is a fleet whose feed read answers only once released, or
// gives up when its caller does.
type heldHistory struct {
	release chan struct{}
	mu      sync.Mutex
	n       int
}

func (h *heldHistory) asked() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.n
}

func (h *heldHistory) List(ctx context.Context, _ store.ListQuery) (eventfan.Listing, eventfan.Coverage, error) {
	h.mu.Lock()
	h.n++
	h.mu.Unlock()
	select {
	case <-h.release:
	case <-ctx.Done():
		return eventfan.Listing{}, eventfan.Coverage{}, ctx.Err()
	}
	return eventfan.Listing{Rows: []store.EventRecord{{
		ID: "e1", Type: "agent_turn_completed", Category: "agent", Time: time.Now().UTC(),
	}}}, answered("node-1"), nil
}

func (h *heldHistory) PhaseTokens(context.Context, store.PhaseTokenQuery) ([]tokens.Record, eventfan.Coverage, error) {
	return nil, answered("node-1"), nil
}

func (h *heldHistory) Turns(context.Context, store.TurnQuery) (eventfan.TurnPage, eventfan.Coverage, error) {
	return eventfan.TurnPage{}, answered("node-1"), nil
}

// flakyHistory is a fleet whose feed read fails for its first `fail` attempts,
// or is not answered by node-2 for its first `silent` (every attempt, when
// negative), and is answered by both after.
type flakyHistory struct {
	fail, silent int
	mu           sync.Mutex
	at           []time.Time
}

func (h *flakyHistory) attempts() int { return len(h.instants()) }

func (h *flakyHistory) instants() []time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.at)
}

func (h *flakyHistory) List(context.Context, store.ListQuery) (eventfan.Listing, eventfan.Coverage, error) {
	h.mu.Lock()
	h.at = append(h.at, time.Now())
	n := len(h.at)
	h.mu.Unlock()
	switch {
	case n <= h.fail:
		return eventfan.Listing{}, eventfan.Coverage{}, errors.New("the store is busy")
	case h.silent < 0 || n <= h.silent:
		return eventfan.Listing{}, eventfan.Coverage{Nodes: []eventfan.NodeCoverage{
			{ID: "node-1", Answered: true},
			{ID: "node-2", Error: "no answer within the 2s fleet read budget"},
		}}, nil
	}
	return eventfan.Listing{}, answered("node-1", "node-2"), nil
}

func (h *flakyHistory) PhaseTokens(context.Context, store.PhaseTokenQuery) ([]tokens.Record, eventfan.Coverage, error) {
	return nil, answered("node-1", "node-2"), nil
}

func (h *flakyHistory) Turns(context.Context, store.TurnQuery) (eventfan.TurnPage, eventfan.Coverage, error) {
	return eventfan.TurnPage{}, answered("node-1", "node-2"), nil
}

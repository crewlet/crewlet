package seat

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/memory"
)

// remaining is how long a step begun now on ctx may run.
func remaining(ctx context.Context, t *testing.T) time.Duration {
	t.Helper()
	step, done := StopStep(ctx)
	defer done()
	deadline, ok := step.Deadline()
	if !ok {
		t.Fatal("a step of a stop carries no deadline")
	}
	return time.Until(deadline)
}

// A STOP'S STEPS SHARE ONE ALLOWANCE, so a store that cannot be reached costs
// the stop one allowance in total rather than one per step: the step that
// spent it all leaves the next one a context that has already ended.
func TestAStopsStepsShareOneAllowance(t *testing.T) {
	t.Parallel()
	ctx := WithStopBudget(t.Context(), NewStopBudget(200*time.Millisecond))
	first, done := StopStep(ctx)
	select {
	case <-first.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("a step was never ended by the stop's allowance")
	}
	done()
	next, done := StopStep(ctx)
	defer done()
	if next.Err() == nil {
		t.Fatalf("a step begun after the allowance was spent may still run %v",
			remaining(ctx, t))
	}
}

// THE ALLOWANCE IS CHARGED ONLY WHILE A STEP RUNS. Between a stop's first steps
// and its last sits the drain's unbounded wait for running turns, which must
// spend none of it; and steps in flight together are charged once for the
// time they overlap, since a node's seats are released together.
//
// Driven on instants of its own rather than the wall clock, so what a step
// was charged is an exact figure: read across a sleep it is bounded only from
// below, and on a loaded runner neither a fixed tolerance nor any bound read
// off clocks around the steps tells one charge for an overlap from one per
// step every time. [StopStep] is the same two calls on time.Now, which
// [TestAStopsStepsShareOneAllowance] holds it to.
func TestTheAllowanceIsChargedOnlyWhileAStepRuns(t *testing.T) {
	t.Parallel()
	const total = 2 * time.Second
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	at := func(ms int) time.Time { return start.Add(time.Duration(ms) * time.Millisecond) }
	budget := NewStopBudget(total)

	// A step of 100ms, then the drain's wait of 300ms before the next one.
	budget.begin(at(0))
	budget.end(at(100))
	deadline := budget.begin(at(400))
	if got := spent(budget); got != 100*time.Millisecond {
		t.Fatalf("%v charged after a 100ms step and a 300ms wait between steps, "+
			"want 100ms: the wait was charged", got)
	}
	if want := at(400).Add(total - 100*time.Millisecond); !deadline.Equal(want) {
		t.Fatalf("a step begun after 100ms was spent may run until %v, want %v",
			deadline.Sub(start), want.Sub(start))
	}
	budget.end(at(400))

	// Two steps that overlap: one runs 500..900, the other 600..1000. Both
	// together run for 500ms; charged per step it would be 800ms.
	budget.begin(at(500))
	deadline = budget.begin(at(600))
	if want := at(600).Add(total - 200*time.Millisecond); !deadline.Equal(want) {
		t.Fatalf("a step begun while another ran may run until %v, want %v: "+
			"the running step's time so far is what is left",
			deadline.Sub(start), want.Sub(start))
	}
	budget.end(at(900))
	budget.end(at(1000))
	if got := spent(budget); got != 600*time.Millisecond {
		t.Fatalf("%v charged in all after a 100ms step and two steps overlapping across "+
			"500ms, want 600ms: an overlap is charged once, not once per step", got)
	}
}

// spent is what b has charged so far.
func spent(b *StopBudget) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.spent
}

// A STEP OUTSIDE A STOP KEEPS WHATEVER BOUND ITS CALLER GAVE IT: a seat handed
// back by a sweep or a fence is not racing a stop's allowance.
func TestAStepOutsideAStopKeepsItsCallersBound(t *testing.T) {
	t.Parallel()
	step, done := StopStep(t.Context())
	defer done()
	if step != t.Context() {
		t.Fatal("a step outside a stop was handed a context of its own")
	}
}

// A LAYER JOINS THE STOP IT IS PART OF, OR BEGINS ONE INSIDE ITS LEASE. A
// context carrying a stop's allowance keeps it, so the layer's give-backs draw
// on what the stop's other steps draw on; one carrying none is handed
// [StopAllowance] of the TTL, so a give-back made outside any stop is still
// bounded — and bounded inside the lease it is racing.
func TestWithinStopJoinsTheStopOrBeginsOneInsideTheLease(t *testing.T) {
	t.Parallel()
	const carried = 300 * time.Millisecond
	joined := WithinStop(WithStopBudget(t.Context(), NewStopBudget(carried)), time.Hour)
	if left := remaining(joined, t); left > carried {
		t.Errorf("a step of a stop carrying %v may run %v: the layer began an "+
			"allowance of its own beside the stop's", carried, left)
	}

	const ttl = 3 * time.Second
	begun := remaining(WithinStop(t.Context(), ttl), t)
	if want := StopAllowance(ttl); begun > want || begun < want-200*time.Millisecond {
		t.Errorf("a stop begun at a %v lease allows %v, want one heartbeat "+
			"interval (%v)", ttl, begun, want)
	}
}

// A HOST STOPPED ON AN ENDED CONTEXT STILL GIVES ITS LEASES BACK. Stop is
// reached on a shutdown path whose context has routinely ended, and the twin
// here refuses a call made on one, as every backend does: a give-back that
// inherited it would release nothing, so every seat would sit dark for a full
// TTL and peers would keep reserving capacity for a node that is gone.
//
// Read straight off the store the moment Stop returns, on a clock that never
// moves, so no lease can have lapsed into looking given back.
func TestAStopOnAnEndedContextGivesItsLeasesBack(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	h := f.newHost("node-a", Config{
		Seats: seatsNamed("ceo", "eng"), SweepInterval: time.Hour, HeartbeatInterval: time.Hour,
	})
	h.Start(f.ctx)
	wantHeld(t, h, "ceo", "eng")

	ended, cancel := context.WithCancel(f.ctx)
	cancel()
	h.Stop(ended)
	for _, resource := range []string{
		coord.SeatResource("ceo"), coord.SeatResource("eng"), coord.NodeResource("node-a"),
	} {
		if lease := f.leaseOf(resource); lease != nil {
			t.Errorf("%s is still held by %s after a stop on an ended context: "+
				"the give-back inherited the caller's context", resource, lease.Owner)
		}
	}
}

// A HOST STOPPED ON AN ENDED CONTEXT SPENDS ONE ALLOWANCE OF ITS OWN LEASE on
// giving its leases back, never none and never longer: the store here answers
// a release only when the context it was asked on ends — a member that has
// lost quorum — so nothing but the allowance decides how long the stop spends
// learning that every give-back failed.
func TestAStopOnAnEndedContextSpendsOneAllowanceOfItsLease(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	stuck := &stuckReleases{Backend: f.store}
	const ttl = 3 * time.Second
	h := f.newHost("node-a", Config{
		Backend: stuck, Seats: seatsNamed("ceo", "eng"), TTL: ttl,
		SweepInterval: time.Hour, HeartbeatInterval: time.Hour,
	})
	h.Start(f.ctx)
	wantHeld(t, h, "ceo", "eng")

	ended, cancel := context.WithCancel(f.ctx)
	cancel()
	stuck.block()
	started := time.Now()
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		h.Stop(ended)
	}()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("a stop against a store that never answers did not return: " +
			"its give-backs are bounded by no allowance")
	}
	if took, allowance := time.Since(started), StopAllowance(ttl); took > allowance+2*time.Second {
		t.Fatalf("the stop took %v against an allowance of %v", took, allowance)
	}
	if got := stuck.releases(); got != 3 {
		t.Errorf("%d release(s) asked for, want both seats and the presence", got)
	}
}

// A HOST THAT CANNOT REACH ITS STORE GIVES ITS LEASES BACK IN ONE ALLOWANCE.
//
// The store here answers a release only when the context it was asked on
// ends, which is a member that has lost quorum: every release fails, and only
// the stop's allowance decides how long the drain spends learning so. The
// presence release spends it, and the seats' releases after it are handed
// contexts that have already ended.
func TestADrainAgainstAnUnreachableStoreSpendsOneAllowance(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	stuck := &stuckReleases{Backend: f.store}
	h := f.newHost("node-a", Config{
		Backend: stuck, Seats: seatsNamed("ceo", "eng"),
		SweepInterval: time.Hour, HeartbeatInterval: time.Hour,
	})
	h.Start(f.ctx)
	wantHeld(t, h, "ceo", "eng")

	const allowance = 300 * time.Millisecond
	ctx := WithStopBudget(f.ctx, NewStopBudget(allowance))
	stuck.block()
	started := time.Now()
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		h.BeginDrain(ctx)
		h.ReleaseAll(ctx, ReasonDrain)
	}()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("a drain against a store that never answers did not finish: " +
			"its releases are not bounded by the stop's allowance")
	}
	if took := time.Since(started); took > allowance+2*time.Second {
		t.Fatalf("the drain took %v against an allowance of %v: its steps each "+
			"spent a bound of their own", took, allowance)
	}
	if got := stuck.releases(); got != 3 {
		t.Errorf("%d release(s) asked for, want the presence and both seats", got)
	}
}

// stuckReleases is a store whose releases, once blocked, answer only when the
// context they were asked on ends — a member without quorum.
type stuckReleases struct {
	*memory.Backend
	mu      sync.Mutex
	blocked bool
	asked   int
}

func (s *stuckReleases) block() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blocked = true
}

func (s *stuckReleases) releases() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.asked
}

func (s *stuckReleases) Release(ctx context.Context, resource, owner string, epoch int64) (bool, error) {
	s.mu.Lock()
	blocked := s.blocked
	if blocked {
		s.asked++
	}
	s.mu.Unlock()
	if !blocked {
		return s.Backend.Release(ctx, resource, owner, epoch)
	}
	<-ctx.Done()
	return false, coord.ErrUnavailable
}

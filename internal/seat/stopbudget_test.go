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
func remaining(t *testing.T, ctx context.Context) time.Duration {
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
			remaining(t, ctx))
	}
}

// THE ALLOWANCE IS CHARGED ONLY WHILE A STEP RUNS. Between a stop's first steps
// and its last sits the drain's unbounded wait for running turns, which must
// spend none of it; and steps in flight together are charged once for the
// time they overlap, since a node's seats are released together.
func TestTheAllowanceIsChargedOnlyWhileAStepRuns(t *testing.T) {
	t.Parallel()
	const total = 2 * time.Second
	ctx := WithStopBudget(t.Context(), NewStopBudget(total))

	_, quick := StopStep(ctx)
	quick()
	time.Sleep(300 * time.Millisecond) // the drain's wait, between steps
	if left := remaining(t, ctx); left < total-200*time.Millisecond {
		t.Fatalf("%v left after a step that took nothing and a wait between "+
			"steps, want nearly all of %v: the wait was charged", left, total)
	}

	_, a := StopStep(ctx)
	_, b := StopStep(ctx)
	time.Sleep(300 * time.Millisecond)
	a()
	b()
	// About 300ms charged for the overlap, never 600ms for two steps — and
	// never less than the overlap itself, which a timer cannot shorten.
	left := remaining(t, ctx)
	if left > total-300*time.Millisecond || left < total-550*time.Millisecond {
		t.Fatalf("%v left after two steps overlapping for 300ms, want about %v: "+
			"the overlap was charged once per step", left, total-300*time.Millisecond)
	}
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
	if left := remaining(t, joined); left > carried {
		t.Errorf("a step of a stop carrying %v may run %v: the layer began an "+
			"allowance of its own beside the stop's", carried, left)
	}

	const ttl = 3 * time.Second
	begun := remaining(t, WithinStop(t.Context(), ttl))
	if want := StopAllowance(ttl); begun > want || begun < want-200*time.Millisecond {
		t.Errorf("a stop begun at a %v lease allows %v, want one heartbeat "+
			"interval (%v)", ttl, begun, want)
	}
}

// A HOST STOPPED ON AN ENDED CONTEXT STILL GIVES ITS LEASES BACK, on an
// allowance inside its own lease rather than none: Stop is reached on a
// shutdown path whose context has routinely ended, and the store here answers
// a release only when the context it was asked on ends.
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

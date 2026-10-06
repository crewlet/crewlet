package sandbox

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
)

// A BOX THAT DOES NOT ANSWER DELAYS NO OTHER BOX. The tick polled its boxes one
// after another, so a box whose envd held a request open for its ten-minute
// silence bound held every box behind it — their completions unannounced and
// their keepalives unsent, toward a fifteen-minute TTL. Now each box is
// polled beside the others: a stuck box's neighbour is polled, found done and
// announced while the stuck one is still being waited on.
//
// Mutation: poll the boxes one after another again, and the done box is not
// announced until the stuck one is released.
func TestABoxThatDoesNotAnswerDelaysNoOtherBox(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	// Listed first — oldest, then by turn — so a tick that polled in turn
	// would reach the done box only after the stuck one.
	stuck := rig.launch("t-1-stuck")
	rig.launch("t-2-done")

	release := make(chan struct{})
	var stuckPolls atomic.Int32
	rig.runner.PollFunc = func(ctx context.Context, box Sandbox) (bool, error) {
		if box.ID() == stuck.SandboxID {
			stuckPolls.Add(1)
			select {
			case <-release:
			case <-ctx.Done():
				return false, ctx.Err()
			}
			return false, nil
		}
		return true, nil
	}

	ticked := make(chan int, 1)
	go func() {
		fired, err := rig.waiter.Tick(t.Context())
		if err != nil {
			t.Errorf("Tick: %v", err)
		}
		ticked <- fired
	}()
	if !announcedWithin(rig, "t-2-done", 10*time.Second) {
		t.Fatal("the done box was not announced while its neighbour's poll was held")
	}
	select {
	case <-ticked:
		t.Fatal("the tick ended while a poll it started was still running")
	default:
	}
	close(release)
	if fired := <-ticked; fired != 1 {
		t.Errorf("the tick fired %d completions; want the done box's", fired)
	}
	if n := stuckPolls.Load(); n != 1 {
		t.Errorf("the stuck box was polled %d times in one tick; want once", n)
	}
}

// A BOX'S POLL IS BOUNDED, however long the box takes to answer: it lasted as
// long as the box's slowest request, which for a command is ten minutes of
// silence. Abandoned at the bound, the poll is that box's loss alone, and a
// later pass asks again.
func TestABoxsPollIsBounded(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	rig.waiter.pollBound = 50 * time.Millisecond
	rig.launch("t-stuck")
	var bounded atomic.Bool
	rig.runner.PollFunc = func(ctx context.Context, _ Sandbox) (bool, error) {
		<-ctx.Done()
		bounded.Store(true)
		return false, ctx.Err()
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := rig.waiter.Tick(t.Context()); err != nil {
			t.Errorf("Tick: %v", err)
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the tick did not end at its box's bound")
	}
	if !bounded.Load() {
		t.Error("the box's poll was not ended by its bound")
	}
}

// A BOX IS POLLED BY ONE POLL AT A TIME, whoever drives the tick: a second tick
// that lands while a box's poll is still running passes that box over, so its
// reconnects, keepalives and liveness probes never overlap — while every other
// box is polled as usual.
func TestABoxIsPolledByOnePollAtATime(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	stuck := rig.launch("t-stuck")
	rig.launch("t-other")
	release := make(chan struct{})
	entered := make(chan struct{}, 4)
	var mu sync.Mutex
	polls := map[string]int{}
	rig.runner.PollFunc = func(ctx context.Context, box Sandbox) (bool, error) {
		mu.Lock()
		polls[box.ID()]++
		again := polls[box.ID()] > 1
		mu.Unlock()
		// Only its first poll is held, so a second one — the overlap
		// this forbids — is counted rather than hung.
		if box.ID() == stuck.SandboxID && !again {
			entered <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
		return false, nil
	}
	first := make(chan struct{})
	go func() {
		defer close(first)
		_, _ = rig.waiter.Tick(t.Context())
	}()
	<-entered // the stuck box's first poll is running
	// AND THE OTHER BOX'S HAS ENDED: a second tick that landed while it was
	// still running would pass it over too, which is the rule working, not
	// the case this asserts.
	other := rig.get("t-other")
	deadline := time.Now().Add(10 * time.Second)
	for {
		mu.Lock()
		polled := polls[other.SandboxID]
		mu.Unlock()
		if polled == 1 && !rig.waiter.isBusy(other.TurnID) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the other box's first poll did not end (polled %d times)", polled)
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := rig.waiter.Tick(t.Context()); err != nil {
		t.Fatalf("the second tick: %v", err)
	}
	close(release)
	<-first
	mu.Lock()
	defer mu.Unlock()
	if polls[stuck.SandboxID] != 1 {
		t.Errorf("the stuck box was polled %d times by two overlapping ticks; want once", polls[stuck.SandboxID])
	}
	if polls[other.SandboxID] != 2 {
		t.Errorf("the other box was polled %d times by two ticks; want twice", polls[other.SandboxID])
	}
}

// THE LOOP KEEPS EVERY OTHER BOX'S CADENCE WHILE ONE BOX IS HELD. A loop that
// waited for each pass's slowest poll would set every box's cadence to a held
// box's bound rather than the interval, and stretch the pass past the duty's
// own TTL. A pass returns once its walk has started: the next one comes on
// schedule, passes the held box over and polls the rest.
//
// Mutation: wait for the pass's tasks in the loop, and the neighbour is polled
// about once per bound — four times here, against about seventy.
func TestTheLoopKeepsEveryOtherBoxsCadenceWhileOneIsHeld(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	stuck := rig.launch("t-1-stuck")
	rig.launch("t-2-healthy")
	waiter, err := NewWaiter(WaiterOptions{
		Queue: rig.queue, Pending: rig.pending, Manager: rig.managers,
		Interval: 20 * time.Millisecond, Now: func() time.Time { return rig.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	waiter.pollBound = 400 * time.Millisecond

	var (
		healthy        atomic.Int32
		stuckInFlight  atomic.Int32
		stuckOverlaps  atomic.Int32
		stuckPollCount atomic.Int32
	)
	rig.runner.PollFunc = func(ctx context.Context, box Sandbox) (bool, error) {
		if box.ID() != stuck.SandboxID {
			healthy.Add(1)
			return false, nil
		}
		stuckPollCount.Add(1)
		if stuckInFlight.Add(1) > 1 {
			stuckOverlaps.Add(1)
		}
		defer stuckInFlight.Add(-1)
		<-ctx.Done()
		return false, ctx.Err()
	}
	waiter.Start(t.Context())
	time.Sleep(1500 * time.Millisecond)
	waiter.Stop()

	// About seventy passes at this cadence; the old loop managed four.
	if n := healthy.Load(); n < 20 {
		t.Errorf("the healthy box was polled %d times in 1.5 s at a 20 ms interval; "+
			"want its cadence kept while its neighbour sat at a 400 ms bound", n)
	}
	if n := stuckPollCount.Load(); n == 0 {
		t.Error("the held box was never polled, so this case tested nothing")
	}
	if n := stuckOverlaps.Load(); n != 0 {
		t.Errorf("the held box was polled by two polls at once %d times; want one at a time", n)
	}
}

// A POLL ENDS WITH THE DUTY THAT AUTHORISED IT. A poll outlives the pass that
// started it, and the duty it was started under lapses a TTL after its claim:
// past that a peer may hold the duty and poll the same box, so the poll is
// bounded by the claim — its TTL less one interval — wherever that comes
// before the poll's own bound.
//
// Mutation: bound a poll by [pollBound] alone, and it runs on for the ten
// seconds this case gives it.
func TestAPollEndsWithTheDutyThatAuthorisedIt(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	rig.launch("t-stuck")
	waiter, err := NewWaiter(WaiterOptions{
		Queue: rig.queue, Pending: rig.pending, Manager: rig.managers,
		Interval: 20 * time.Millisecond, DutyTTL: 200 * time.Millisecond,
		ClaimDuty: func(context.Context) (bool, error) { return true, nil },
		Now:       func() time.Time { return rig.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	waiter.pollBound = 10 * time.Second
	ended := make(chan error, 1)
	rig.runner.PollFunc = func(ctx context.Context, _ Sandbox) (bool, error) {
		<-ctx.Done()
		ended <- ctx.Err()
		return false, ctx.Err()
	}
	started := time.Now()
	if _, err := waiter.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(started); took > 5*time.Second {
		t.Fatalf("the poll ran %v under a duty of 200 ms; want it ended with the duty", took)
	}
	if err := <-ended; !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("the poll ended with %v; want the duty's deadline", err)
	}
}

// A DUTY LOST STOPS EVERY POLL IT AUTHORISED, at once rather than at the
// claim's deadline: a peer holds the duty now and works the same boxes. And a
// box whose reach was STOPPED is not counted as one that did not answer — the
// stop says nothing about the box — so a run is never given up on because its
// node lost the duty.
//
// Mutation: leave in-flight polls running when a claim is refused, and the
// poll runs to the ten-second bound; count a stopped reach as a failure, and
// the streak shows it.
func TestADutyLostStopsEveryPollItAuthorised(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	stuck := rig.launch("t-stuck")
	var claims atomic.Int32
	waiter, err := NewWaiter(WaiterOptions{
		Queue: rig.queue, Pending: rig.pending, Manager: rig.managers,
		Interval: 20 * time.Millisecond, DutyTTL: time.Minute,
		// Held for the first pass, and a peer's from the second on.
		ClaimDuty: func(context.Context) (bool, error) { return claims.Add(1) == 1, nil },
		Now:       func() time.Time { return rig.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	waiter.pollBound = 10 * time.Second
	reaching := make(chan struct{}, 1)
	ended := make(chan error, 1)
	rig.provider.AttachFunc = func(ctx context.Context, id string) error {
		if id != stuck.SandboxID {
			return nil
		}
		reaching <- struct{}{}
		<-ctx.Done()
		ended <- ctx.Err()
		return ctx.Err()
	}
	waiter.Start(t.Context())
	defer waiter.Stop()
	<-reaching
	select {
	case err := <-ended:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("the poll ended with %v; want it stopped when the duty went to a peer", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a poll the lost duty authorised was still running 5 s later")
	}
	waiter.Stop()
	waiter.mu.Lock()
	defer waiter.mu.Unlock()
	if streak, counted := waiter.failures[stuck.TurnID]; counted {
		t.Errorf("a reach stopped by the lost duty was counted against the box: %+v", streak)
	}
}

// A RECLAIM THAT WON ITS FLIP KILLS THE BOX, whatever ends its task: the flip
// released the row, so nothing else names the box any more, and a kill
// abandoned with the task — at its deadline, or by a stop — left a paused
// snapshot billed with nobody to reclaim it.
//
// Mutation: kill under the task's own context, and the box survives.
func TestAReclaimThatWonItsFlipKillsTheBoxWhateverEndsItsTask(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	run := rig.launch("t1")
	rig.park("t1")
	rig.now = rig.now.Add(DefaultPauseTTL + time.Second)
	ctx, stop := context.WithCancel(t.Context())
	rig.waiter.pending = stopAfterFlip{PendingStore: rig.pending, stop: stop}
	if !rig.waiter.reapOne(ctx, rig.manager, rig.get("t1")) {
		t.Fatal("the reclaim did not win its flip, so this case tested nothing")
	}
	if killed := rig.provider.KilledIDs(); !slices.Equal(killed, []string{run.SandboxID}) {
		t.Fatalf("killed %v after the task was stopped past its flip; want %q", killed, run.SandboxID)
	}
}

// stopAfterFlip ends the reclaim's task the moment its flip lands.
type stopAfterFlip struct {
	PendingStore
	stop context.CancelFunc
}

func (s stopAfterFlip) ExpirePause(ctx context.Context, turnID string) (bool, error) {
	won, err := s.PendingStore.ExpirePause(ctx, turnID)
	s.stop()
	return won, err
}

// A WALK WHOSE AUTHORITY HAS ENDED STARTS NOTHING. It takes no slot, even
// where a slot is free at the same moment: both cases of the wait are ready
// then, and Go picks between them at random, so a walk that took the slot
// would start a task it no longer has the authority for — one whose deadline
// may already have passed, which then counts a box it never asked as one that
// did not answer. And it registers no task: asked under the lock a stand-down
// stops everything under, a task is either stopped by a stand-down or refused
// after it, never started between the two with nothing left to stop it.
//
// Mutation: drop the second look after the slot is taken, and about half of
// these attempts take one; drop the walk's check from the registration, and a
// task is registered after the stand-down.
func TestAWalkWhoseAuthorityHasEndedStartsNothing(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	ended, stop := context.WithCancel(t.Context())
	stop()
	for range 200 {
		if rig.waiter.acquireSlot(ended) {
			t.Fatal("a walk whose authority had ended took a slot")
		}
	}
	if held := len(rig.waiter.slots); held != 0 {
		t.Fatalf("%d slots are held after every attempt was refused; want none", held)
	}
	if rig.waiter.begin(ended, "t1", func() {}) || rig.waiter.isBusy("t1") {
		t.Fatal("a walk whose authority had ended registered a task")
	}
}

// announcedWithin waits for a completion of turnID to be published.
func announcedWithin(rig *waiterRig, turnID string, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		rig.queue.mu.Lock()
		for _, p := range rig.queue.published {
			if c, ok := p.event.Data.(*types.SandboxRunCompleted); ok && c.TurnID == turnID {
				rig.queue.mu.Unlock()
				return true
			}
		}
		rig.queue.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

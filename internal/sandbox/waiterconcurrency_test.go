package sandbox

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
)

// A BOX THAT DOES NOT ANSWER DELAYS NO OTHER BOX: each box is polled beside the
// others, so one whose runtime holds an exec open to the poll's bound holds
// back no neighbour's completion or keepalive.
//
// READ IN A BUBBLE, once every goroutine is blocked: the stuck box's poll waits
// on its release, and the done box has been polled and announced beside it with
// no time passed.
//
// Mutation: poll the boxes one after another, and the done box is not
// announced until the stuck one is released.
func TestABoxThatDoesNotAnswerDelaysNoOtherBox(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig := newWaiterRig(t)
		// Listed first — oldest, then by turn — so a tick that polled in turn
		// would reach the done box only after the stuck one.
		stuck := rig.launch("t-1-stuck")
		rig.launch("t-2-done")

		release := make(chan struct{})
		var stuckPolls atomic.Int32
		rig.runner.PollFunc = func(ctx context.Context, box Sandbox) (JobState, error) {
			if box.ID() == stuck.SandboxID {
				stuckPolls.Add(1)
				select {
				case <-release:
				case <-ctx.Done():
					return JobRunning, ctx.Err()
				}
				return JobRunning, nil
			}
			return JobEnded, nil
		}

		ticked := make(chan int, 1)
		go func() {
			fired, err := rig.waiter.Tick(t.Context())
			if err != nil {
				t.Errorf("Tick: %v", err)
			}
			ticked <- fired
		}()
		synctest.Wait()
		if !completionPublished(rig, "t-2-done") {
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
	})
}

// A BOX'S POLL IS BOUNDED, however long the box takes to answer (a command's
// request may sit ten minutes silent): abandoned at the bound, the poll is that
// box's loss alone, and a later pass asks again.
func TestABoxsPollIsBounded(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	rig.waiter.pollBound = 50 * time.Millisecond
	rig.launch("t-stuck")
	var bounded atomic.Bool
	rig.runner.PollFunc = func(ctx context.Context, _ Sandbox) (JobState, error) {
		<-ctx.Done()
		bounded.Store(true)
		return JobRunning, ctx.Err()
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
	case <-time.After(10 * time.Second): //fixedwait:liveness
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
	rig.runner.PollFunc = func(ctx context.Context, box Sandbox) (JobState, error) {
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
		return JobRunning, nil
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
		if time.Now().After(deadline) { //fixedwait:measure a poll's own deadline, whose firing only fails the case
			t.Fatalf("the other box's first poll did not end (polled %d times)", polled)
		}
		time.Sleep(time.Millisecond) //fixedwait:liveness
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

// THE LOOP KEEPS EVERY OTHER BOX'S CADENCE WHILE ONE BOX IS HELD: a pass
// returns once its walk has started, so the next comes on schedule, passes the
// held box over and polls the rest. Waiting for each pass's slowest poll would
// set every box's cadence to the held box's bound and stretch a pass past the
// duty's own TTL.
//
// Counted and ended by EVENTS: the held poll is released once the healthy box
// has been polled three times during it, three passes inside the ten-second
// bound (five hundred intervals), missed only if the machine stalls for seconds.
//
// Mutation: wait for the pass's tasks in the loop, and the neighbour is polled
// at most once during the hold, which runs to its bound and goes red.
func TestTheLoopKeepsEveryOtherBoxsCadenceWhileOneIsHeld(t *testing.T) {
	t.Parallel()
	const want = 3
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
	// The backstop, five hundred intervals: reached by a loop that ties the
	// healthy box to the held one, or by a machine stalled for seconds.
	waiter.pollBound = 10 * time.Second

	var (
		duringHold     atomic.Int32
		stuckInFlight  atomic.Int32
		stuckOverlaps  atomic.Int32
		stuckPollCount atomic.Int32
		firstHeld      = make(chan struct{})
		firstEnded     = make(chan struct{})
		released       = make(chan struct{})
		release        sync.Once
	)
	rig.runner.PollFunc = func(ctx context.Context, box Sandbox) (JobState, error) {
		if box.ID() != stuck.SandboxID {
			// Counted only inside the held box's first poll: its count is
			// one while that poll runs, and two or more once it has ended.
			if stuckPollCount.Load() == 1 && stuckInFlight.Load() == 1 && duringHold.Add(1) == want {
				release.Do(func() { close(released) })
			}
			return JobRunning, nil
		}
		n := stuckPollCount.Add(1)
		if stuckInFlight.Add(1) > 1 {
			stuckOverlaps.Add(1)
		}
		if n == 1 {
			close(firstHeld)
		}
		defer func() {
			stuckInFlight.Add(-1)
			if n == 1 {
				close(firstEnded)
			}
		}()
		select {
		case <-released:
			return JobRunning, nil
		case <-ctx.Done():
			return JobRunning, ctx.Err()
		}
	}
	waiter.Start(t.Context())
	defer waiter.Stop()
	for _, step := range []struct {
		name string
		ch   <-chan struct{}
	}{{"began", firstHeld}, {"ended", firstEnded}} {
		select {
		case <-step.ch:
		case <-time.After(30 * time.Second): //fixedwait:liveness
			t.Fatalf("the held box's first poll never %s", step.name)
		}
	}
	waiter.Stop()

	if n := duringHold.Load(); n < want {
		t.Errorf("the healthy box was polled %d times while its neighbour's first poll was held, which "+
			"its %s bound then ended; want %d, which a loop that keeps every box's cadence reaches "+
			"in a few 20 ms intervals unless the machine stalled for seconds", n, waiter.pollBound, want)
	}
	if n := stuckOverlaps.Load(); n != 0 {
		t.Errorf("the held box was polled by two polls at once %d times; want one at a time", n)
	}
}

// A RECLAIM THAT WON ITS FLIP KILLS THE BOX, whatever ends its task: the flip
// released the row, so nothing else names the box, and a kill abandoned with
// the task (at its deadline, or by a stop) would leave a paused box held with
// nobody to reclaim it.
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

// A WALK THE NEXT PASS REPLACED TAKES NO SLOT, even where one is free: both
// cases of the wait are ready, Go picks at random, and a task started from the
// listing the next pass superseded polls a run that may have moved on since.
//
// Mutation: drop the second look after the slot is taken, and about half of
// these attempts take one.
func TestAReplacedWalkTakesNoSlot(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	replaced, stop := context.WithCancel(t.Context())
	stop()
	for range 200 {
		if rig.waiter.acquireSlot(replaced) {
			t.Fatal("a replaced walk took a slot")
		}
	}
	if held := len(rig.waiter.slots); held != 0 {
		t.Fatalf("%d slots are held after every attempt was refused; want none", held)
	}
}

// completionPublished reports whether a completion of turnID has been
// published.
func completionPublished(rig *waiterRig, turnID string) bool {
	rig.queue.mu.Lock()
	defer rig.queue.mu.Unlock()
	for _, p := range rig.queue.published {
		if c, ok := p.event.Data.(*types.ExecutorFinished); ok && c.TurnID == turnID {
			return true
		}
	}
	return false
}

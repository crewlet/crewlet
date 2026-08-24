package seat

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// fakePulse is a duty whose beat and liveness the test sets directly.
type fakePulse struct {
	mu   sync.Mutex
	last time.Time
	live bool
}

func (p *fakePulse) Beat() (time.Time, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.last, p.live
}

func (p *fakePulse) set(last time.Time, live bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.last, p.live = last, live
}

func caught() (chan Stall, func(Stall)) {
	fired := make(chan Stall, 4)
	return fired, func(s Stall) { fired <- s }
}

// --- the threshold is not a knob -------------------------------------------

func TestTheThresholdIsTheSeatLeaseTTL(t *testing.T) {
	t.Parallel()
	// Not a config knob and not a number of its own. Past it the node is
	// provably not the owner, and letting the two drift is how a process
	// gets to be simultaneously "not the owner" and "still holding the
	// mail". The public constructor takes no threshold at all, which is the
	// strongest form that rule can take.
	if got := NewWatchdog().Threshold(); got != SeatLeaseTTL {
		t.Fatalf("threshold = %s, want the seat lease TTL (%s)", got, SeatLeaseTTL)
	}
}

func TestPollIsACeilingScaledToTheThreshold(t *testing.T) {
	t.Parallel()
	// A poll slower than the threshold turns the deadline into a suggestion.
	// At production scale the ceiling applies instead — no point polling
	// nine times a second because the TTL is generous.
	cases := []struct {
		threshold time.Duration
		want      time.Duration
	}{
		{SeatLeaseTTL, WatchdogPollInterval},
		{45 * time.Second, time.Second},
		{500 * time.Millisecond, 100 * time.Millisecond},
		{50 * time.Millisecond, 10 * time.Millisecond},
	}
	for _, tc := range cases {
		w := newWatchdog(tc.threshold, nil, func(Stall) {})
		if w.poll != tc.want {
			t.Errorf("threshold %s: poll = %s, want %s", tc.threshold, w.poll, tc.want)
		}
		if w.poll > tc.threshold/WatchdogBeatsPerThreshold {
			t.Errorf("threshold %s: poll %s does not fit %d times inside it",
				tc.threshold, w.poll, WatchdogBeatsPerThreshold)
		}
	}
}

func TestTheHostBeatFitsFiveTimesInTheThreshold(t *testing.T) {
	t.Parallel()
	// The invariant a healthy node's life depends on. A beat slow relative
	// to the threshold makes a perfectly healthy process report itself
	// stalled — invisible at the shipped values (45 s vs 1 s) and lethal to
	// anyone who lowers the lease TTL, so it is enforced by construction
	// rather than documented.
	f := newFleet(t)
	cases := []struct {
		ttl       time.Duration
		heartbeat time.Duration
	}{
		{SeatLeaseTTL, HeartbeatInterval},
		{45 * time.Second, 15 * time.Second},
		{3 * time.Second, time.Second},
		{500 * time.Millisecond, 200 * time.Millisecond},
		{50 * time.Millisecond, 10 * time.Millisecond},
	}
	for _, tc := range cases {
		h := f.newHost("node-a", Config{TTL: tc.ttl, HeartbeatInterval: tc.heartbeat})
		beat := h.beatInterval()
		if beat > tc.ttl/WatchdogBeatsPerThreshold {
			t.Errorf("ttl %s: beat %s does not fit %d times inside the threshold",
				tc.ttl, beat, WatchdogBeatsPerThreshold)
		}
		if beat > tc.heartbeat {
			t.Errorf("ttl %s: beat %s is slower than the pass it proves (%s)",
				tc.ttl, beat, tc.heartbeat)
		}
		if beat <= 0 {
			t.Errorf("ttl %s: beat %s must be positive", tc.ttl, beat)
		}
	}
}

func TestTheExitCodeIsDistinct(t *testing.T) {
	t.Parallel()
	// So an orchestrator's restart log says what happened rather than
	// blending into every other non-zero exit.
	switch WatchdogExitCode {
	case 0, 1, 2:
		t.Fatalf("exit code %d is indistinguishable from an ordinary failure", WatchdogExitCode)
	}
}

// --- what fires and what does not ------------------------------------------

func TestAHealthyDutyIsNeverKilled(t *testing.T) {
	t.Parallel()
	// The failure that would matter most: a watchdog that fires on a
	// perfectly healthy process.
	clock := newClock()
	fired, onStall := caught()
	w := newWatchdog(time.Minute, clock.Now, onStall)
	pulse := &fakePulse{}
	w.Watch("seat-heartbeat", pulse)

	for range 20 {
		pulse.set(clock.Now(), true)
		clock.Advance(WatchdogBeatInterval)
		if !w.check() {
			t.Fatal("the watchdog stopped watching a healthy duty")
		}
	}
	if len(fired) != 0 {
		t.Fatalf("fired %d times on a healthy duty", len(fired))
	}
}

func TestAStalledDutyEndsTheProcess(t *testing.T) {
	t.Parallel()
	// The whole remedy: a wedged-but-alive node keeps its broker session
	// open, so the broker holds its prefetch of seats a peer now owns for a
	// full unacked-message timeout — roughly 30 minutes. Exiting collapses
	// that to the 9 ms a closed session takes to release.
	clock := newClock()
	fired, onStall := caught()
	w := newWatchdog(time.Minute, clock.Now, onStall)
	pulse := &fakePulse{}
	pulse.set(clock.Now(), true)
	w.Watch("seat-heartbeat", pulse)

	clock.Advance(time.Minute + time.Second)
	if w.check() {
		t.Fatal("the watchdog kept watching after firing")
	}
	stall := <-fired
	if stall.Duty != "seat-heartbeat" || stall.Lag != time.Minute+time.Second || stall.Threshold != time.Minute {
		t.Fatalf("stall = %+v, want the seat heartbeat 61s past a 60s threshold", stall)
	}
}

func TestTheWatchdogFiresOnlyOnce(t *testing.T) {
	t.Parallel()
	// It ends the process, so "again" is meaningless — but the loop must
	// not spin re-firing while a test seam keeps the process alive.
	clock := newClock()
	fired, onStall := caught()
	w := newWatchdog(time.Minute, clock.Now, onStall)
	pulse := &fakePulse{}
	pulse.set(clock.Now(), true)
	w.Watch("seat-heartbeat", pulse)

	clock.Advance(2 * time.Minute)
	for range 4 {
		w.check()
	}
	if len(fired) != 1 {
		t.Fatalf("fired %d times, want once", len(fired))
	}
}

func TestALiveDutyThatHasNeverBeatenIsNotShot(t *testing.T) {
	t.Parallel()
	// Registered but not yet running is not wedged. Reading an unset beat
	// as "stalled since the epoch" would kill a process for starting up.
	clock := newClock()
	fired, onStall := caught()
	w := newWatchdog(time.Minute, clock.Now, onStall)
	w.Watch("seat-heartbeat", &fakePulse{live: true})

	clock.Advance(time.Hour)
	if !w.check() {
		t.Fatal("the watchdog gave up on a duty that had simply not started yet")
	}
	if len(fired) != 0 {
		t.Fatal("a duty that never beat was shot for it")
	}
}

func TestAWatchdogWithNothingToWatchStandsDown(t *testing.T) {
	t.Parallel()
	// There is nothing whose stall could hold a peer's mail, so there is
	// nothing to exit for. The safe direction is never an unprovoked exit.
	fired, onStall := caught()
	w := newWatchdog(time.Minute, newClock().Now, onStall)
	if w.check() {
		t.Fatal("a watchdog with no duties kept watching")
	}
	if len(fired) != 0 {
		t.Fatal("a watchdog with no duties fired")
	}
}

// --- a GONE duty is not a WEDGED one ---------------------------------------

func TestAStoppedHostStandsTheWatchdogDown(t *testing.T) {
	t.Parallel()
	// The distinction the whole mechanism turns on. From the watcher, "the
	// duty is wedged" and "the duty is gone" look identical — the beat
	// simply stops refreshing — and they are opposite situations. A wedged
	// duty is still alive inside a live process holding a peer's mail,
	// which is the entire justification for exiting; a duty that was shut
	// down took its work with it.
	//
	// Getting this wrong is not a subtle bug: every engine that is
	// abandoned rather than stopped arms a suicide timer that fires one
	// lease TTL later on a perfectly healthy process. In the Python engine
	// it killed this repo's own test suite at 63%, exit code 75.
	f := newFleet(t)
	clock := newClock()
	fired, onStall := caught()
	h := f.newHost("node-a", Config{
		Seats: seatsNamed("ceo"), Clock: clock.Now,
		SweepInterval: time.Hour, HeartbeatInterval: time.Hour,
	})
	w := newWatchdog(time.Minute, clock.Now, onStall)
	w.Watch("seat-heartbeat", h)

	h.Start(f.ctx)
	if !w.check() {
		t.Fatal("the watchdog stood down on a running host")
	}

	h.Stop(f.ctx)
	clock.Advance(time.Hour) // the beat can never refresh again

	if w.check() {
		t.Fatal("a stopped host was mistaken for a wedged one")
	}
	if len(fired) != 0 {
		t.Fatal("the watchdog shot a process whose host had been shut down cleanly")
	}
}

func TestAHostThatNeverStartedIsNotWatched(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	clock := newClock()
	fired, onStall := caught()
	h := f.newHost("node-a", Config{Clock: clock.Now})
	w := newWatchdog(time.Minute, clock.Now, onStall)
	w.Watch("seat-heartbeat", h)

	clock.Advance(time.Hour)
	if w.check() {
		t.Fatal("a host that was never started kept the watchdog armed")
	}
	if len(fired) != 0 {
		t.Fatal("a host that was never started was shot for not beating")
	}
}

// --- the goroutine wiring --------------------------------------------------

func TestTheWatcherGoroutineFiresOnItsOwn(t *testing.T) {
	t.Parallel()
	// Everything above drives check() directly. This is the wiring: a real
	// ticker, on real time, with nothing prompting it.
	fired, onStall := caught()
	w := newWatchdog(50*time.Millisecond, nil, onStall)
	pulse := &fakePulse{}
	pulse.set(time.Now().Add(-time.Hour), true)
	w.Watch("seat-heartbeat", pulse)

	w.Start(context.Background())
	defer w.Stop()

	select {
	case stall := <-fired:
		if stall.Duty != "seat-heartbeat" {
			t.Fatalf("stall named %q", stall.Duty)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the watcher slept through a duty that had not beaten in an hour")
	}
}

func TestWatchdogStartAndStopAreIdempotent(t *testing.T) {
	t.Parallel()
	_, onStall := caught()
	w := newWatchdog(time.Minute, nil, onStall)
	w.Watch("seat-heartbeat", &fakePulse{last: time.Now(), live: true})

	w.Start(context.Background())
	w.Start(context.Background()) // a second start is a no-op, not a second goroutine
	w.Stop()
	w.Stop()
}

func TestTheWatcherStopsWithItsContext(t *testing.T) {
	t.Parallel()
	_, onStall := caught()
	w := newWatchdog(time.Minute, nil, onStall)
	w.Watch("seat-heartbeat", &fakePulse{last: time.Now(), live: true})

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	cancel()
	w.Stop() // returns only once the watcher goroutine has exited
}

// --- what the host's beat actually proves ----------------------------------

func TestAHealthyHostKeepsBeating(t *testing.T) {
	t.Parallel()
	// The beat ticks far faster than the pass it guards, so a healthy node
	// is never more than one tick behind.
	f := newFleet(t)
	h := f.newHost("node-a", Config{
		Seats: seatsNamed("ceo"), Clock: time.Now,
		HeartbeatInterval: 20 * time.Millisecond, SweepInterval: time.Hour, TTL: time.Minute,
	})
	h.Start(f.ctx)
	defer h.Stop(f.ctx)

	first, live := h.Beat()
	if !live {
		t.Fatal("a started host reported itself not running")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		next, _ := h.Beat()
		if next.After(first) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the heartbeat goroutine never proved it was turning")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// blockingRenew hangs the heartbeat pass the way a wedged store call, a
// deadlocked lock or a hook that never returns does.
type blockingRenew struct {
	coord.Backend
	release <-chan struct{}
}

func (b blockingRenew) Renew(ctx context.Context, resource, owner string, epoch int64, ttl time.Duration) (bool, error) {
	if coord.IsSeatResource(resource) {
		select {
		case <-b.release:
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	return b.Backend.Renew(ctx, resource, owner, epoch, ttl)
}

func TestAWedgedHeartbeatPassIsWhatTheWatchdogSees(t *testing.T) {
	t.Parallel()
	// The Go-specific half of the design. A stalled duty does not stall the
	// runtime here, so a watchdog that only proved "the scheduler still
	// runs Go code" would sleep through the one failure that matters: the
	// renewals stop, this node's seats move to a peer, and its queue client
	// — whose goroutines the stall never touched — stays attached and holds
	// their mail for the broker's full ack timeout.
	f := newFleet(t)
	release := make(chan struct{})
	defer close(release)

	h := f.newHost("node-a", Config{
		Backend: blockingRenew{Backend: f.store, release: release},
		Seats:   seatsNamed("ceo"), Clock: time.Now,
		HeartbeatInterval: 10 * time.Millisecond, SweepInterval: time.Hour, TTL: time.Minute,
	})
	fired, onStall := caught()
	w := newWatchdog(150*time.Millisecond, nil, onStall)
	w.Watch("seat-heartbeat", h)

	h.Start(f.ctx)
	w.Start(f.ctx)
	defer func() {
		w.Stop()
		h.Stop(f.ctx) // the blocked renew returns on the loop's cancellation
	}()

	select {
	case stall := <-fired:
		if stall.Duty != "seat-heartbeat" || stall.Lag < 150*time.Millisecond {
			t.Fatalf("stall = %+v, want the seat heartbeat past its threshold", stall)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the watchdog slept through a heartbeat pass that never returned, so this " +
			"node's seats moved to a peer while it kept holding their mail")
	}
}

func TestLagReportsHowFarBehindTheWorstDutyIs(t *testing.T) {
	t.Parallel()
	clock := newClock()
	_, onStall := caught()
	w := newWatchdog(time.Minute, clock.Now, onStall)

	if lag := w.Lag(); lag != 0 {
		t.Fatalf("lag with nothing watched = %s, want 0", lag)
	}
	slow, quick := &fakePulse{}, &fakePulse{}
	slow.set(clock.Now(), true)
	clock.Advance(30 * time.Second)
	quick.set(clock.Now(), true)
	w.Watch("slow", slow)
	w.Watch("quick", quick)

	if lag := w.Lag(); lag != 30*time.Second {
		t.Fatalf("lag = %s, want the worst duty's 30s", lag)
	}
	// A duty that stopped is not part of the answer.
	slow.set(clock.Now().Add(-time.Hour), false)
	if lag := w.Lag(); lag != 0 {
		t.Fatalf("lag = %s, want 0 once the stale duty stopped", lag)
	}
}

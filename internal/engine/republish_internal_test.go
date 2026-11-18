package engine

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
)

// as is a party named name, for a request the republisher credits.
func as(name string) iam.Actor { return iam.Actor{Name: name, Kind: iam.ActorOperator} }

// A recorder for what the republisher actually ran.
type republishLog struct {
	mu        sync.Mutex
	operators []string
	done      chan struct{}
}

func newRepublishLog() *republishLog {
	return &republishLog{done: make(chan struct{}, 16)}
}

func (l *republishLog) run(_ context.Context, by iam.Actor) {
	l.mu.Lock()
	l.operators = append(l.operators, by.Name)
	l.mu.Unlock()
	select {
	case l.done <- struct{}{}:
	default:
	}
}

func (l *republishLog) calls() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.operators...)
}

// waitFor blocks until the recorder has seen n runs, or the case gives up.
func (l *republishLog) waitFor(t *testing.T, n int) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for len(l.calls()) < n {
		select {
		case <-l.done:
		case <-deadline:
			t.Fatalf("saw %d re-activation(s), want %d", len(l.calls()), n)
		}
	}
}

// THE FIRST REQUEST RUNS AT ONCE, because somebody is watching: a credential
// a pass just minted goes live without the operator doing anything else.
//
// At once is not on the caller's goroutine ([republisher.request]), so the
// run is awaited rather than asserted synchronously.
func TestTheFirstRepublishRunsImmediately(t *testing.T) {
	t.Parallel()
	log := newRepublishLog()
	r := &republisher{now: func() time.Time { return time.Unix(0, 0) }}
	t.Cleanup(r.stop)

	r.request(as("founder@example.com"), log.run)
	log.waitFor(t, 1)

	if got := log.calls(); len(got) != 1 || got[0] != "founder@example.com" {
		t.Fatalf("calls = %v, want one credited to the operator", got)
	}
}

// AND THE CALLER DOES NOT WAIT FOR IT: the caller is a provisioning pass's
// Flush inside the surface lease, and a reload run inline spends the margin
// the pass's status write needs, landing that write outside the lease
// ([republisher.request]).
func TestTheCallerDoesNotWaitForTheRebuild(t *testing.T) {
	t.Parallel()
	log := newRepublishLog()
	r := &republisher{now: func() time.Time { return time.Unix(0, 0) }}
	t.Cleanup(r.stop)

	// A run that does not return: a coordination store having a bad
	// afternoon, or a config apply waiting on a peer.
	blocked := make(chan struct{})
	t.Cleanup(func() { close(blocked) })
	done := make(chan struct{})
	go func() {
		r.request(as("founder@example.com"), func(context.Context, iam.Actor) { <-blocked })
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("request blocked on the rebuild: a pass that sealed a " +
			"credential spends the lease margin its own status write needs, " +
			"and the write then lands outside the lease")
	}
	_ = log
}

// A BURST IS ONE APPLY, AND NOTHING IN IT IS DROPPED: one Connect writes a
// request per surface, and those inside the window are coalesced, not
// skipped, since a skipped rebuild leaves a sealed credential nothing running
// reads.
//
// On the bubble's clock, so "during the window" is a state the case holds
// rather than a race with a real timer: the read follows [synctest.Wait] and
// precedes any clock advance.
//
// Mutation: never arm the window's timer, and the second call never comes;
// fire it at once, and two calls are read during the window; credit the
// burst's first request, and the operator is wrong.
func TestABurstOfSealsBecomesOneRepublish(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		log := newRepublishLog()
		clock := time.Unix(0, 0)
		var mu sync.Mutex
		r := &republisher{
			window: 50 * time.Millisecond,
			now: func() time.Time {
				mu.Lock()
				defer mu.Unlock()
				return clock
			},
		}
		t.Cleanup(r.stop)

		// Six seals inside one window, which is one operator pressing Connect.
		r.request(as("atlassian"), log.run)
		for _, who := range []string{"jira", "confluence", "datadog", "gitlab", "founder"} {
			r.request(as(who), log.run)
		}

		// One ran immediately; five are owed. Read once everything due now
		// has run and before the clock moves, so a coalesced run fired early
		// is among the calls read.
		log.waitFor(t, 1)
		synctest.Wait()
		if got := log.calls(); len(got) != 1 {
			t.Fatalf("calls = %v during the window, want exactly the first", got)
		}
		log.waitFor(t, 2)

		got := log.calls()
		if len(got) != 2 {
			t.Fatalf("calls = %v, want the immediate one and one for the burst", got)
		}
		// The latest operator: the burst's one revision is credited to
		// whoever caused it last.
		if got[1] != "founder" {
			t.Errorf("the coalesced re-activation was credited to %q, want the "+
				"last request's operator", got[1])
		}
	})
}

// A PASS THAT CANNOT CONVERGE IS BOUNDED: an apply brings the reconcile tick
// forward, so a pass sealing on every tick would seal as fast as an apply
// completes. Certifying reconcilers against "a converged pass writes nothing"
// says nothing of a broken vendor pass; this makes the next one survivable.
func TestARunawayPassCannotOutrunTheWindow(t *testing.T) {
	t.Parallel()
	log := newRepublishLog()
	r := &republisher{now: func() time.Time { return time.Unix(0, 0) }}
	t.Cleanup(r.stop)

	// A hundred seals at one instant, which is the loop with the clock
	// held still: nothing may run but the first.
	for range 100 {
		r.request(as("runaway"), log.run)
	}
	log.waitFor(t, 1)

	if got := log.calls(); len(got) != 1 {
		t.Errorf("%d re-activations from 100 seals inside one window, want 1: "+
			"an apply storm is what the inline rebuild allowed", len(got))
	}
}

// A NODE THAT IS LEAVING DOES NOT RE-ACTIVATE ON ITS WAY OUT: an owed timer
// left armed through shutdown would write a config revision from a node that
// has released its seats.
func TestStoppingDisarmsAnOwedRepublish(t *testing.T) {
	t.Parallel()
	log := newRepublishLog()
	r := &republisher{
		window: 20 * time.Millisecond,
		now:    func() time.Time { return time.Unix(0, 0) },
	}

	r.request(as("first"), log.run)
	// Awaited before the second, so "the one that ran before the stop" is a
	// fact rather than a race with the asynchronous immediate run.
	log.waitFor(t, 1)
	r.request(as("owed"), log.run)
	r.stop()

	// Well past the window it would have fired in.
	time.Sleep(80 * time.Millisecond)

	if got := log.calls(); len(got) != 1 {
		t.Errorf("calls = %v after stop, want only the one that ran before it", got)
	}
	// AND A REQUEST AFTER STOP IS REFUSED RATHER THAN RE-ARMING.
	r.request(as("late"), log.run)
	if got := log.calls(); len(got) != 1 {
		t.Errorf("calls = %v, want a request after stop to do nothing", got)
	}
}

// THE WINDOW REOPENS, so a later credential is not held behind an earlier
// burst for ever.
func TestTheWindowReopensForALaterSeal(t *testing.T) {
	t.Parallel()
	log := newRepublishLog()
	var mu sync.Mutex
	clock := time.Unix(0, 0)
	r := &republisher{
		window: time.Hour,
		now: func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return clock
		},
	}
	t.Cleanup(r.stop)

	r.request(as("first"), log.run)
	// Awaited, so the clock moves only after the asynchronous first run.
	log.waitFor(t, 1)
	mu.Lock()
	clock = clock.Add(time.Hour + time.Second)
	mu.Unlock()
	r.request(as("much-later"), log.run)
	log.waitFor(t, 2)

	got := log.calls()
	if len(got) != 2 || got[1] != "much-later" {
		t.Errorf("calls = %v, want a seal past the window to run at once", got)
	}
}

// STOP WAITS FOR A RE-ACTIVATION THAT HAS ALREADY BEGUN, AND CANCELS IT.
//
// Disarming does nothing for a fired timer, already past the `stopped`
// check. Both halves are asserted, since either alone leaves a leaving node
// writing beside [Engine.Stop]: the run's context is CANCELLED, so an apply
// in flight gives up, and stop does not RETURN until the run has unwound, so
// a caller can treat it as "nothing is writing any more".
func TestStoppingWaitsForARepublishAlreadyRunning(t *testing.T) {
	t.Parallel()
	var (
		entered   = make(chan struct{})
		release   = make(chan struct{})
		returned  = make(chan struct{})
		cancelled bool
	)
	r := &republisher{now: func() time.Time { return time.Unix(0, 0) }}

	r.request(as("founder@example.com"), func(ctx context.Context, _ iam.Actor) {
		close(entered)
		// Held inside the run: stop is called between its guard and its
		// return.
		<-release
		cancelled = ctx.Err() != nil
		close(returned)
	})

	// The run is in flight and past every check it makes.
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the immediate re-activation never started")
	}

	stopped := make(chan struct{})
	go func() { r.stop(); close(stopped) }()

	// STOP MUST NOT HAVE RETURNED, because the run has not.
	select {
	case <-stopped:
		t.Fatal("stop returned while a re-activation was still writing: a " +
			"caller reads that as nothing is writing any more, and closes " +
			"the store underneath it")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("the run never returned")
	}
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("stop never returned after the run finished")
	}
	if !cancelled {
		t.Error("the run's context was not cancelled, so an apply in flight " +
			"finishes rather than giving up: a leaving node still writes a " +
			"revision and wakes every peer")
	}
}

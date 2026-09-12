package engine

import (
	"context"
	"sync"
	"testing"
	"time"
)

// A recorder for what the republisher actually ran.
type republishLog struct {
	mu        sync.Mutex
	operators []string
	done      chan struct{}
}

func newRepublishLog() *republishLog {
	return &republishLog{done: make(chan struct{}, 16)}
}

func (l *republishLog) run(_ context.Context, operator string) {
	l.mu.Lock()
	l.operators = append(l.operators, operator)
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

// THE FIRST REQUEST RUNS AT ONCE, because somebody is watching.
//
// The whole point of the rebuild is that a credential a pass just minted goes
// live without the operator doing anything else. Making them wait out a
// coalescing window for the first one would trade the incident this bounds
// for the one it was built to fix.
//
// AT ONCE IS NOT ON THE CALLER'S GOROUTINE. It used to be both, and the
// second half was spending a lease margin that belongs to the status write —
// see [republisher.request]. So the run is awaited rather than asserted
// synchronously, which is the same claim about latency and a different one
// about who blocks.
func TestTheFirstRepublishRunsImmediately(t *testing.T) {
	t.Parallel()
	log := newRepublishLog()
	r := &republisher{now: func() time.Time { return time.Unix(0, 0) }}
	t.Cleanup(r.stop)

	r.request("founder@example.com", log.run)
	log.waitFor(t, 1)

	if got := log.calls(); len(got) != 1 || got[0] != "founder@example.com" {
		t.Fatalf("calls = %v, want one credited to the operator", got)
	}
}

// AND THE CALLER DOES NOT WAIT FOR IT.
//
// The caller is a provisioning pass's Flush, inside the surface lease, and
// the minute [setup.PassDeadline] leaves inside [setup.LeaseTTL] is what the
// status write that records the pass has to complete in. Running the reload
// inline spent that minute here and left the record asking for another, so it
// landed with the lease already lapsed and a peer free to hold the surface.
func TestTheCallerDoesNotWaitForTheRebuild(t *testing.T) {
	t.Parallel()
	log := newRepublishLog()
	r := &republisher{now: func() time.Time { return time.Unix(0, 0) }}
	t.Cleanup(r.stop)

	// A RUN THAT DOES NOT RETURN, which is the shape that matters: a
	// coordination store having a bad afternoon, or a config apply waiting
	// on a peer.
	blocked := make(chan struct{})
	t.Cleanup(func() { close(blocked) })
	done := make(chan struct{})
	go func() {
		r.request("founder@example.com", func(context.Context, string) { <-blocked })
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

// A BURST IS ONE APPLY, AND NOTHING IN IT IS DROPPED.
//
// Connecting a third-party app from the dashboard writes one request per
// surface — Atlassian's alone applies three revisions in a row — and each seal
// used to be a whole-company rebuild and a permanent config revision. Twenty
// of one deployment's fifty-six revisions were these.
//
// The requests inside the window are COALESCED rather than skipped, which is
// the distinction that matters: a skipped rebuild is exactly the state this
// mechanism exists to prevent — a credential sealed, resolvable, and read by
// nothing that is running.
func TestABurstOfSealsBecomesOneRepublish(t *testing.T) {
	t.Parallel()
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
	r.request("atlassian", log.run)
	for _, who := range []string{"jira", "confluence", "datadog", "gitlab", "founder"} {
		r.request(who, log.run)
	}

	// One ran immediately; the other five are owed, not gone.
	log.waitFor(t, 1)
	if got := log.calls(); len(got) != 1 {
		t.Fatalf("calls = %v during the window, want exactly the first", got)
	}
	log.waitFor(t, 2)

	got := log.calls()
	if len(got) != 2 {
		t.Fatalf("calls = %v, want the immediate one and one for the burst", got)
	}
	// THE LATEST OPERATOR, because the burst collapses to one revision and
	// it is credited to whoever most recently caused it.
	if got[1] != "founder" {
		t.Errorf("the coalesced re-activation was credited to %q, want the "+
			"last request's operator", got[1])
	}
}

// A PASS THAT CANNOT CONVERGE IS BOUNDED, which the inline rebuild was not.
//
// An apply marks every surface stale and brings the reconcile loop's next tick
// forward, so a pass that seals on every tick seals as fast as an apply
// completes. The inline version argued this could not happen because every
// reconciler is certified against "a converged pass writes nothing" — an
// argument about a CONVERGED world, which is not the world a broken vendor
// pass is in. GitLab's minted a year-long `api`-scoped token every five
// seconds, 144 of them from one connect.
//
// This does not make that pass correct — it is fixed where it lives — it makes
// the next one survivable.
func TestARunawayPassCannotOutrunTheWindow(t *testing.T) {
	t.Parallel()
	log := newRepublishLog()
	r := &republisher{now: func() time.Time { return time.Unix(0, 0) }}
	t.Cleanup(r.stop)

	// A hundred seals at one instant, which is the loop with the clock
	// held still: nothing may run but the first.
	for range 100 {
		r.request("runaway", log.run)
	}
	log.waitFor(t, 1)

	if got := log.calls(); len(got) != 1 {
		t.Errorf("%d re-activations from 100 seals inside one window, want 1: "+
			"an apply storm is what the inline rebuild allowed", len(got))
	}
}

// A NODE THAT IS LEAVING DOES NOT RE-ACTIVATE ON ITS WAY OUT.
//
// The timer is a goroutine's lifetime and it belongs to the engine. Left armed
// through a shutdown it writes a config revision from a node that has already
// released its seats — crediting an operator and waking every peer on behalf
// of a process that is going away.
func TestStoppingDisarmsAnOwedRepublish(t *testing.T) {
	t.Parallel()
	log := newRepublishLog()
	r := &republisher{
		window: 20 * time.Millisecond,
		now:    func() time.Time { return time.Unix(0, 0) },
	}

	r.request("first", log.run)
	// AWAITED BEFORE THE SECOND, so "the one that ran before the stop" is a
	// fact rather than a race: the immediate run is no longer complete when
	// request returns.
	log.waitFor(t, 1)
	r.request("owed", log.run)
	r.stop()

	// Well past the window it would have fired in.
	time.Sleep(80 * time.Millisecond)

	if got := log.calls(); len(got) != 1 {
		t.Errorf("calls = %v after stop, want only the one that ran before it", got)
	}
	// AND A REQUEST AFTER STOP IS REFUSED RATHER THAN RE-ARMING.
	r.request("late", log.run)
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

	r.request("first", log.run)
	// AWAITED, so the clock only moves once the first run has happened: the
	// immediate run is asynchronous now, and moving the clock under it would
	// be asserting the reopened window against a race.
	log.waitFor(t, 1)
	mu.Lock()
	clock = clock.Add(time.Hour + time.Second)
	mu.Unlock()
	r.request("much-later", log.run)
	log.waitFor(t, 2)

	got := log.calls()
	if len(got) != 2 || got[1] != "much-later" {
		t.Errorf("calls = %v, want a seal past the window to run at once", got)
	}
}

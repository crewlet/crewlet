package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
)

// reconcileAt is the i-th activation instant of a reconciler case.
func reconcileAt(i int) time.Time {
	return time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Minute)
}

// nextBegun is the activation the next reconcile began for, or a failure
// after a while.
func nextBegun(t *testing.T, began <-chan time.Time, what string) time.Time {
	t.Helper()
	select {
	case at := <-began:
		return at
	case <-time.After(10 * time.Second):
		t.Fatalf("no reconcile began: %s", what)
		return time.Time{}
	}
}

// THE NEWEST ACTIVATION'S RECONCILE WINS.
//
// One reconcile runs at a time. Asked for a newer activation while one runs,
// the reconciler ENDS the running one, with [code.ErrSuperseded] as its cause
// — the newer reconcile decides every repository again, and its fence refuses
// what the older would still write — and of the requests made meanwhile runs
// only the newest. A request for the activation running, or an older one, is
// dropped: nothing runs after the newest but what is asked for later. And the
// ended reconcile is never run again, though it answers its ending as what it
// left undone: the newer one decides every repository it would have.
//
// Mutation: let the older reconcile run to its end and the newer never begins
// while it is held; keep every request and an older activation is reconciled
// after the newest; retry a reconcile its context ended and the older one runs
// again.
func TestTheNewestActivationsReconcileWins(t *testing.T) {
	t.Parallel()
	type ran struct {
		at    time.Time
		cause error
	}
	var (
		mu   sync.Mutex
		runs []ran
	)
	began := make(chan time.Time, 8)
	release := make(chan struct{})
	// asked holds an ended reconcile from returning until the case has made
	// both its newer requests, so the loop cannot begin the first of them
	// between the two.
	asked := make(chan struct{})
	r := newCodeReconciler(func(ctx context.Context, w codeReposWanted) error {
		began <- w.at
		select {
		case <-ctx.Done():
		case <-release:
		}
		<-asked
		mu.Lock()
		runs = append(runs, ran{w.at, context.Cause(ctx)})
		mu.Unlock()
		return context.Cause(ctx)
	})
	r.retryAfter = func(int) time.Duration { return time.Millisecond }
	t.Cleanup(r.stop)

	r.ask(t.Context(), nil, reconcileAt(1))
	if got := nextBegun(t, began, "the first request"); !got.Equal(reconcileAt(1)) {
		t.Fatalf("the reconcile of %v began, want the one asked for, %v", got, reconcileAt(1))
	}
	r.ask(t.Context(), nil, reconcileAt(2))
	r.ask(t.Context(), nil, reconcileAt(3))
	close(asked)
	if got := nextBegun(t, began, "two newer requests while the first was held"); !got.Equal(reconcileAt(3)) {
		t.Fatalf("after the first, the reconcile of %v began, want the newest asked for, %v",
			got, reconcileAt(3))
	}
	mu.Lock()
	first := runs[0]
	mu.Unlock()
	if !errors.Is(first.cause, code.ErrSuperseded) {
		t.Errorf("the older reconcile was ended by %v, want a newer activation's request "+
			"(code.ErrSuperseded)", first.cause)
	}

	r.ask(t.Context(), nil, reconcileAt(2))
	r.ask(t.Context(), nil, reconcileAt(3))
	close(release)
	waitUntil(t, 10*time.Second, "the reconciler to go idle", r.idle)
	mu.Lock()
	defer mu.Unlock()
	if len(runs) != 2 || !runs[1].at.Equal(reconcileAt(3)) || runs[1].cause != nil {
		var got []string
		for _, r := range runs {
			got = append(got, fmt.Sprintf("%s ended by %v", r.at.Format(time.TimeOnly), r.cause))
		}
		t.Fatalf("the reconciles run were %q, want the first, ended, and the newest (%s), "+
			"run to its end — an older or the same activation asked for after it is dropped",
			got, reconcileAt(3).Format(time.TimeOnly))
	}
}

// STOPPING ENDS THE RECONCILE IN FLIGHT, WAITS FOR IT, AND BEGINS NO MORE: the
// engine stops it before withdrawing its admission, so stop returns only once
// the reconcile has — which was ended with the engine's stop as its cause —
// and a request after it is dropped. A reconciler never asked runs nothing, and
// stops at once.
//
// Mutation: return from stop without waiting for the loop and the reconcile
// is still running when it does.
func TestStoppingEndsTheReconcileInFlightAndBeginsNoMore(t *testing.T) {
	t.Parallel()
	newCodeReconciler(func(context.Context, codeReposWanted) error {
		t.Error("a reconciler never asked ran a reconcile")
		return nil
	}).stop()

	began := make(chan time.Time, 4)
	var (
		returned atomic.Bool
		cause    atomic.Value
	)
	r := newCodeReconciler(func(ctx context.Context, w codeReposWanted) error {
		began <- w.at
		<-ctx.Done()
		// A reconcile ending takes a moment: the decisions in flight
		// return first.
		time.Sleep(50 * time.Millisecond)
		cause.Store(context.Cause(ctx))
		returned.Store(true)
		return context.Cause(ctx)
	})
	r.retryAfter = func(int) time.Duration { return time.Millisecond }
	r.ask(t.Context(), nil, reconcileAt(1))
	nextBegun(t, began, "the request before the stop")
	r.stop()
	if !returned.Load() {
		t.Fatal("stop returned with a reconcile still running: it would publish past the " +
			"admission's withdrawal")
	}
	if got, _ := cause.Load().(error); !errors.Is(got, errCodeReconcileStopped) {
		t.Errorf("the reconcile in flight was ended by %v, want the engine's stop", got)
	}
	r.ask(t.Context(), nil, reconcileAt(2))
	select {
	case at := <-began:
		t.Fatalf("a reconcile of %v began after the stop", at)
	case <-time.After(100 * time.Millisecond):
	}
	if !r.idle() {
		t.Error("a request after the stop is waiting to run")
	}
}

// A RECONCILE THAT LEFT SOMETHING UNDONE IS RUN AGAIN, WAITING LONGER EACH
// TIME, UNTIL IT LEAVES NOTHING.
//
// The apply that asked has already reported its revision applied, so nothing
// else runs it again: a node whose decisions were refused while it caught up
// on the activation's fence, an outcome the log did not answer, a listing it
// could not read — each left code.repos not in force until somebody edited the
// configuration or restarted the node, while the control plane reported the
// epoch applied. So the reconciler runs the same activation again after each
// such run, the wait after the n-th in a row asked for n failures — the
// backoff grows — and a run that leaves nothing undone ends it.
//
// Mutation: drop the retry and the activation runs once; reset the count at
// each retry and every wait is the first.
func TestAFailedReconcileIsRunAgainUntilItApplies(t *testing.T) {
	t.Parallel()
	var (
		mu    sync.Mutex
		runs  []time.Time
		waits []int
	)
	r := newCodeReconciler(func(_ context.Context, w codeReposWanted) error {
		mu.Lock()
		defer mu.Unlock()
		runs = append(runs, w.at)
		if len(runs) < 3 {
			return errors.New("the activation's fence was not applied inside its budget")
		}
		return nil
	})
	r.retryAfter = func(failures int) time.Duration {
		mu.Lock()
		defer mu.Unlock()
		waits = append(waits, failures)
		return time.Millisecond
	}
	t.Cleanup(r.stop)

	r.ask(t.Context(), nil, reconcileAt(1))
	waitUntil(t, 10*time.Second, "the reconcile to apply", r.idle)
	mu.Lock()
	defer mu.Unlock()
	if len(runs) != 3 || !runs[1].Equal(reconcileAt(1)) || !runs[2].Equal(reconcileAt(1)) {
		t.Errorf("the reconciles run were %v, want the activation asked for three times: twice "+
			"leaving something undone, and once applying", runs)
	}
	if !slices.Equal(waits, []int{1, 2}) {
		t.Errorf("the waits were asked for %v failures in a row, want 1 and then 2", waits)
	}
}

// A RETRY WAITS FOR NOTHING BUT ITS OWN TIME: A NEWER ACTIVATION RUNS AT ONCE
// AND THE RETRY IS DROPPED, AND A STOP RETURNS AT ONCE.
//
// A newer activation decides every repository again from its own document,
// so a request for one while an older activation waits for its retry runs now
// and the older one is never run again. And the engine stops the reconciler
// before withdrawing its admission, so a stop does not wait out a retry — five
// minutes at the ceiling — and nothing runs after it. A request for the
// activation waiting to be run again is dropped, as one for the activation
// running is.
//
// Mutation: keep the retry past a newer request and the reconciler never goes
// idle; wait out the retry in stop and the stop takes the hour.
func TestANewerRequestOrAStopEndsTheRetry(t *testing.T) {
	t.Parallel()
	failing := func(ran chan<- time.Time) func(context.Context, codeReposWanted) error {
		return func(_ context.Context, w codeReposWanted) error {
			ran <- w.at
			if w.at.Equal(reconcileAt(1)) {
				return errors.New("an outcome the code log did not answer")
			}
			return nil
		}
	}
	// waiting is a retry an hour away, saying it was asked for on recorded:
	// the failure is held by the time anything waiting there hears of it.
	waiting := func(recorded chan<- struct{}) func(int) time.Duration {
		return func(int) time.Duration {
			recorded <- struct{}{}
			return time.Hour
		}
	}

	t.Run("a newer request", func(t *testing.T) {
		t.Parallel()
		ran := make(chan time.Time, 8)
		recorded := make(chan struct{}, 8)
		r := newCodeReconciler(failing(ran))
		r.retryAfter = waiting(recorded)
		t.Cleanup(r.stop)
		r.ask(t.Context(), nil, reconcileAt(1))
		nextBegun(t, ran, "the first request")
		<-recorded
		r.ask(t.Context(), nil, reconcileAt(1))
		r.ask(t.Context(), nil, reconcileAt(2))
		if got := nextBegun(t, ran, "a newer request while a retry waits"); !got.Equal(reconcileAt(2)) {
			t.Fatalf("the reconcile of %v began, want the newer activation's, at once", got)
		}
		waitUntil(t, 10*time.Second, "the reconciler to go idle", r.idle)
		select {
		case at := <-ran:
			t.Errorf("a reconcile of %v ran after the newer activation applied", at)
		default:
		}
	})

	t.Run("a stop", func(t *testing.T) {
		t.Parallel()
		ran := make(chan time.Time, 8)
		recorded := make(chan struct{}, 8)
		r := newCodeReconciler(failing(ran))
		r.retryAfter = waiting(recorded)
		r.ask(t.Context(), nil, reconcileAt(1))
		nextBegun(t, ran, "the first request")
		<-recorded
		stopped := make(chan struct{})
		go func() {
			r.stop()
			close(stopped)
		}()
		select {
		case <-stopped:
		case <-time.After(10 * time.Second):
			t.Fatal("stop waited for a retry an hour away")
		}
		if !r.idle() {
			t.Error("a retry is waiting to run after the stop")
		}
	})
}

// AN APPLY ASKS FOR THE RECONCILE AND DOES NOT WAIT FOR IT.
//
// A reconcile writes a record for every repository that changed — ten
// thousand at the most a document may declare — and an apply that waited for
// them stayed behind on its epoch for as long, past the grace after which a
// node behind its peers is shed. So an apply hands its epoch to the reconciler
// and returns: here the reconcile is held until the test ends, and the apply
// has returned with the epoch it asked for in hand.
//
// Mutation: decide the reconcile inside the apply and this one's held
// reconciler is never asked.
func TestAnApplyAsksForTheReconcileAndDoesNotWaitForIt(t *testing.T) {
	t.Parallel()
	e := newSandboxNode(t, parseCompany(t, codeReposDoc(webRepo(""), "")))
	reconciled(t, e)
	e.codeRepos.stop()
	asked := make(chan codeReposWanted, 1)
	release := make(chan struct{})
	e.codeRepos = newCodeReconciler(func(ctx context.Context, w codeReposWanted) error {
		asked <- w
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	})
	t.Cleanup(func() { close(release) })

	applyOK(t, e, codeReposDoc(webRepo("")+apiRepo, ""))
	select {
	case w := <-asked:
		if w.company == nil || len(w.company.Config.Code.Repos) != 2 {
			t.Errorf("the apply asked for a reconcile of %+v, want its own epoch's two "+
				"repositories", w.company)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the apply returned without asking for a reconcile of code.repos")
	}
}

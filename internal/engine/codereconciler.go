package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/backoff"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/statelog"
)

// errCodeReconcileStopped is the cause a reconcile of code.repos is ended with
// when the engine stops ([codeReconciler.stop]).
var errCodeReconcileStopped = errors.New("engine: the engine stopped before the reconcile finished")

// codeReconciler runs this node's reconciles of code.repos
// ([reconcileCodeRepos]) BESIDE the applies that ask for them rather than
// inside them: [Engine.applyCodeRepos] asks and returns.
//
// # Why not inside the apply
//
// A reconcile costs a write for every repository that CHANGED, each one this
// node waits to apply, and a document may declare ten thousand: an activation
// that declares them all for the first time, removes them all or restores them
// all is that many writes — 16–19 s on one 4-core node (internal/code's
// doc.go, measured section) — and in a fleet every data node decides the same
// writes at once, the losers of each race deciding it again from the winner's
// record, each node's applier applying every record. Inside the apply, that
// time is time the node is BEHIND on the epoch, and a node behind longer than
// configplane.LagGrace (45 s) while a peer has applied the epoch is shed: how
// many repositories code.repos declares, and how fast this node and its peers
// write, would decide whether a node keeps its seats. Nothing the node serves
// waits for a repository to exist — every node's rows come from the log,
// whichever node wrote them — so the apply asks for the reconcile, reports the
// epoch applied, and the repositories follow.
//
// # The newest activation wins
//
// One reconcile runs at a time, for the newest activation asked for. Asked
// again while one runs, the reconciler keeps only the newest request, and ENDS
// a running reconcile of an older activation: the newer one decides every
// repository again from its own document, and the fence it raises first
// refuses, on every node, what the older one would still write — so letting it
// finish spends those writes for nothing and holds the newer one back by them.
// It is ended with [code.ErrSuperseded] as its cause, which is what its log
// line says. A request for the activation already running or waiting — to run,
// or to run again — or for an older one, is dropped: the activation asked for
// last is decided whole.
//
// # What a reconcile leaves undone is run again
//
// Out of the apply, a reconcile has no apply outcome to fail: the control
// plane reports the epoch applied whatever the reconcile met, and no peer
// covers for it where this is the only data node. So a reconcile that leaves
// something undone a second run can apply ([reconcileCodeRepos] says what) is
// RUN AGAIN — the same activation, from the rows as they are by then, which
// costs a batch read where nothing changed — after [codeReposRetryWait], which
// grows with each run in a row that leaves something undone, until one leaves
// nothing. A newer request drops the retry, since the newer activation decides
// every repository; a run its context ended (a newer request, the stop) is
// never run again, whatever it answered; and the stop does not wait the retry
// out.
//
// # Stopped before the admission is withdrawn
//
// A reconcile publishes on the code log, so it is stopped with every other
// state-log publisher, before [Engine.withdraw] ([Engine.teardown]): the one in
// flight is ended, a retry waiting is dropped, and nothing begins after — the
// next boot reconciles.
type codeReconciler struct {
	// run is one reconcile of the activation asked for, run to its end or
	// until its context ends, answering what it left undone that running it
	// again can do ([reconcileCodeRepos]).
	run func(ctx context.Context, w codeReposWanted) error
	// retryAfter is how long to wait before running again a reconcile that
	// has left something undone failures times in a row:
	// [codeReposRetryWait]. Set before the first request.
	retryAfter func(failures int) time.Duration

	mu sync.Mutex
	// pending is the newest activation asked for and not yet begun, and
	// running the one in flight, whose context end ends.
	pending, running *codeReposWanted
	end              context.CancelCauseFunc
	// failed is the activation whose last run left something undone, to be
	// run again at retryAt; failures counts its runs in a row that have.
	failed   *codeReposWanted
	failures int
	retryAt  time.Time
	// started is whether the loop is running, which the first request starts
	// — a node that hosts no code never runs one; stopped refuses every
	// request after [codeReconciler.stop].
	started, stopped bool
	// stopLife ends the loop's lifetime, the context it was started with.
	stopLife context.CancelCauseFunc
	wake     chan struct{}
	done     chan struct{}
}

// codeReposWanted is one activation's request for a reconcile: the epoch whose
// code.repos is decided, and the instant it was activated at.
type codeReposWanted struct {
	company *Company
	at      time.Time
}

const (
	// codeReposRetryBase is how long the reconciler waits before it runs
	// again a reconcile that left something undone, doubled for each run in
	// a row that does, up to [codeReposRetryCeiling].
	//
	// ONE WRITE BUDGET ([statelog.DefaultResolveBudget], five seconds), for
	// the failures that clear on their own: a node catching up at boot has
	// every decision refused once the activation's fence is not applied
	// inside that budget, and is given as long again before it is asked
	// again; a stream that lost its leader has elected another well inside
	// it.
	codeReposRetryBase = statelog.DefaultResolveBudget

	// codeReposRetryCeiling is the longest the reconciler waits between runs
	// of a reconcile that keeps leaving something undone.
	//
	// FIVE MINUTES, reached at the seventh run in a row, about five minutes
	// after the first failure. From there a failure that does not clear costs
	// one reconcile every five minutes — an empty fence decision, a paged
	// walk of the rows, and one batch read of the unchanged declarations,
	// 0.47–0.59 s at the ten thousand a document may declare (internal/code's
	// measured section) — plus a write and a line for each repository still
	// not applied, which is the point: a declaration not in force is said
	// every five minutes until it is, rather than once and then never.
	codeReposRetryCeiling = 5 * time.Minute

	// codeReposRetryJitter spreads each wait across a fifth of itself either
	// way — the skill sync's spread for its retry — so the data nodes of a
	// fleet that failed together, under one broker that lost its leader, do
	// not run again on the same second and race each other's decisions.
	codeReposRetryJitter = 0.2
)

// codeReposRetryWait is how long to wait before running again a reconcile
// that has left something undone failures times in a row.
func codeReposRetryWait(failures int) time.Duration {
	return backoff.Jitter(backoff.Doubling(failures, codeReposRetryBase, codeReposRetryCeiling),
		codeReposRetryJitter)
}

// newCodeReconciler is a reconciler whose every reconcile is run, and run
// again after [codeReposRetryWait] while it leaves something undone.
func newCodeReconciler(run func(ctx context.Context, w codeReposWanted) error) *codeReconciler {
	return &codeReconciler{run: run, retryAfter: codeReposRetryWait,
		wake: make(chan struct{}, 1), done: make(chan struct{})}
}

// ask asks for a reconcile of the epoch c at the activation at, and returns at
// once. The loop's lifetime is ctx's values without its end, taken from the
// first request: a request is made from inside an apply, whose context ends
// with it.
func (r *codeReconciler) ask(ctx context.Context, c *Company, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return
	}
	newest := r.pending
	if newest == nil {
		newest = r.running
	}
	if newest == nil {
		newest = r.failed
	}
	if newest != nil && !newest.at.Before(at) {
		return
	}
	if r.running != nil && r.running.at.Before(at) {
		r.end(fmt.Errorf("%w: the activation at %s asked for a reconcile of its own",
			code.ErrSuperseded, at.UTC().Format(time.RFC3339Nano)))
	}
	r.pending = &codeReposWanted{company: c, at: at}
	// THE RETRY GOES WITH THE OLDER ACTIVATION: the newer one decides every
	// repository again, and its own failures count from none.
	r.failed, r.failures = nil, 0
	if !r.started {
		r.started = true
		life, stop := context.WithCancelCause(context.WithoutCancel(ctx))
		r.stopLife = stop
		go r.loop(life)
	}
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// loop runs the newest request, one at a time, and a failed one again when its
// retry is due, until life ends.
func (r *codeReconciler) loop(life context.Context) {
	defer close(r.done)
	for {
		var (
			timer *time.Timer
			due   <-chan time.Time
		)
		if wait, retrying := r.retryDue(); retrying {
			timer = time.NewTimer(wait)
			due = timer.C
		}
		select {
		case <-life.Done():
		case <-r.wake:
		case <-due:
			r.retryNow()
		}
		if timer != nil {
			timer.Stop()
		}
		if life.Err() != nil {
			return
		}
		for r.next(life) {
		}
	}
}

// retryDue is how long until the failed activation's retry, and false where
// none waits.
func (r *codeReconciler) retryDue() (time.Duration, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failed == nil {
		return 0, false
	}
	return time.Until(r.retryAt), true
}

// retryNow makes the failed activation the request to run, unless a newer one
// has been asked for since.
func (r *codeReconciler) retryNow() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failed != nil && r.pending == nil {
		r.pending, r.failed = r.failed, nil
	}
}

// next runs the pending request, if there is one and life has not ended, and
// reports whether it ran one. A run that answers something left undone, and
// whose context nothing ended, is the activation to run again.
func (r *codeReconciler) next(life context.Context) bool {
	r.mu.Lock()
	w := r.pending
	r.pending = nil
	if w == nil || life.Err() != nil {
		r.mu.Unlock()
		return false
	}
	ctx, end := context.WithCancelCause(life)
	r.running, r.end = w, end
	r.mu.Unlock()
	undone := r.run(ctx, *w)
	// ENDED — by a newer request, which runs next and decides every
	// repository, or by the stop — is never run again, whatever it answered.
	ended := ctx.Err() != nil
	end(nil)

	r.mu.Lock()
	r.running, r.end = nil, nil
	var failures int
	var wait time.Duration
	switch {
	case ended:
	case undone == nil:
		r.failures = 0
	default:
		r.failures++
		failures, wait = r.failures, r.retryAfter(r.failures)
		r.failed, r.retryAt = w, time.Now().Add(wait)
	}
	r.mu.Unlock()
	if failures > 0 {
		log.InfoContext(life, "code_repos_retrying", "activation",
			w.at.UTC().Format(time.RFC3339Nano), "attempt", failures, "retry_in", wait.String(),
			"error", undone.Error(),
			"detail", "the reconcile of code.repos left something undone, said in the lines "+
				"before this one, and runs again; the wait doubles with each run in a row that "+
				"does, up to "+codeReposRetryCeiling.String())
	}
	return true
}

// stop ends the reconcile in flight with [errCodeReconcileStopped] as its
// cause, drops a retry waiting and every request after, and returns once the
// loop has.
func (r *codeReconciler) stop() {
	r.mu.Lock()
	r.stopped = true
	started := r.started
	if started {
		r.stopLife(errCodeReconcileStopped)
	}
	r.mu.Unlock()
	if started {
		<-r.done
	}
	r.mu.Lock()
	r.pending, r.failed = nil, nil
	r.mu.Unlock()
}

// idle is whether no reconcile is running or waiting to — to run, or to run
// again.
func (r *codeReconciler) idle() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pending == nil && r.running == nil && r.failed == nil
}

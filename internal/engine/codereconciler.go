package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/backoff"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/statelog"
)

// errCodeReconcileStopped is the cause a reconcile of code.repos is ended with
// when the engine stops ([codeReconciler.stop]).
var errCodeReconcileStopped = errors.New("engine: the engine stopped before the reconcile finished")

// codeReconciler runs this node's reconciles of code.repos
// ([reconcileCodeRepos]) beside the applies that ask ([Engine.applyCodeRepos]),
// never inside: ten thousand repositories take 16–19 s to write
// (internal/gitlet's doc.go, measured section), time behind on the epoch that
// past configplane.LagGrace sheds the node while a peer has applied it; nothing
// the node serves waits for a repository to exist.
//
// One reconcile runs at a time, for the newest activation asked for: a newer
// request ends a running older one with [gitlet.ErrSuperseded] as its cause,
// since the newer decides every repository again and its fence refuses what
// the older would still write; a request no newer than one running or waiting
// is dropped. What a run leaves undone is run again ([codeReconciler.next]).
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
	// codeReposRetryBase is the first wait before a reconcile that left
	// something undone runs again, doubled each run in a row up to
	// [codeReposRetryCeiling]. One write budget
	// ([statelog.DefaultResolveBudget], 5 s), for failures that clear on
	// their own: a node catching up at boot whose fence was not applied
	// inside that budget, a stream electing a new leader.
	codeReposRetryBase = statelog.DefaultResolveBudget

	// codeReposRetryCeiling is the longest wait between runs, reached at the
	// seventh in a row. Five minutes: a failure that does not clear then costs
	// one reconcile every five minutes (an empty fence decision, a paged walk
	// and one batch read, 0.47–0.59 s at ten thousand declarations,
	// internal/gitlet's measured section) plus a write and a line for each
	// repository not applied, so one not in force is said until it is.
	codeReposRetryCeiling = 5 * time.Minute

	// codeReposRetryJitter spreads each wait a fifth either way, as the skill
	// sync's retry does, so data nodes that failed together under one broker
	// do not run again on the same second and race each other's decisions.
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
			gitlet.ErrSuperseded, at.UTC().Format(time.RFC3339Nano)))
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
// reports whether it ran one. A run that leaves undone what a second run can
// apply ([reconcileCodeRepos] says what) is run again after
// [codeReposRetryWait], from the rows as they are by then: out of the apply it
// has no outcome to fail, and where this is the only data node no peer covers
// for it.
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
// cause, drops a waiting retry without waiting it out and every request after,
// and returns once the loop has. A reconcile publishes on the code log, so
// this runs with every other state-log publisher, before [Engine.withdraw]
// ([Engine.teardown]); the next boot reconciles.
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

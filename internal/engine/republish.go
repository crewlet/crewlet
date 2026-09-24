package engine

import (
	"context"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
)

// Coalescing the re-activations a provisioning pass asks for.
//
// # One apply per burst, and a bound that does not depend on anyone's promise
//
// A pass that seals a credential re-activates the current revision so that
// everything built at the last apply is rebuilt against it (see
// [Engine.rebuildForSealedSecrets]). Performed inline, that had two costs and
// the second one was an incident.
//
// A CONNECT IS A BURST. The setup dialog writes one request per surface, so
// connecting one third-party app can seal an organization key, a per-seat
// token and a webhook secret within a second of each other. Each seal is a
// whole-company rebuild and a permanent config revision — 20 of one
// deployment's 56 revisions were automatic reloads — where what the operator
// did was press a button once.
//
// AND AN APPLY WAKES THE PASS THAT CAUSED IT. An apply marks every surface
// stale, which brings the reconcile loop's next tick forward, which runs the
// pass again. A converged pass seals nothing and the cycle stops there — but
// a pass that can NEVER converge seals on every tick by construction, and
// then the cycle is bounded only by how fast an apply completes. One did:
// GitLab minted a token for an account GitLab would not let authenticate,
// read the refusal as a stale credential, and minted another. At the
// reconcile cadence that was slow enough to look like nothing; behind an
// inline rebuild it ran every five seconds and left 144 live year-long
// `api`-scoped tokens from a single connect.
//
// That vendor fault is fixed in `internal/gitlab`, where it lives. This is
// the other half: the rebuild must not be able to amplify the NEXT one.
//
// # Trailing edge, because dropping a request is the original bug
//
// The first request in a quiet period runs immediately — a person who pressed
// Connect is waiting, and the whole point is that the credential goes live
// without them doing anything else. Requests arriving inside the window are
// folded into ONE run at the end of it, rather than skipped: a skipped
// rebuild is exactly the state this mechanism exists to prevent, a credential
// sealed and resolvable that nothing running has been rebuilt against.
//
// # Immediately, but never on the caller's goroutine
//
// "At once" and "before this call returns" are different things, and the
// second one was spending a lease margin that belongs to somebody else. The
// caller is a provisioning pass's Flush, holding the surface lease, and the
// minute [setup.PassDeadline] leaves inside [setup.LeaseTTL] is the status
// write's. See [republisher.request].
type republisher struct {
	mu sync.Mutex

	// last is when a re-activation was last performed, and the zero value
	// means never — so the first request in the life of a node runs at
	// once rather than waiting out a window it was not here for.
	last time.Time

	// pending is armed while a request is owed inside the window. Its
	// party is the LATEST one asked for: a burst is one apply, and the
	// revision it writes is credited to whoever most recently caused it.
	pending *time.Timer
	by      iam.Actor

	// inflight is the immediate run's timer, armed at zero delay so the
	// caller does not block on it. Separate from pending because both are
	// live at once: a burst arms the trailing run while this one is still
	// going. See [republisher.request] for why the caller must not wait.
	inflight *time.Timer

	// stopped is set by [republisher.stop] so a timer that fires during
	// shutdown does not re-activate a revision on a node that is leaving.
	stopped bool

	// runs counts the re-activations that have STARTED and not yet
	// returned, and cancelRuns cancels the context every one of them was
	// given.
	//
	// DISARMING THE TIMERS IS NOT ENOUGH, which is the half [republisher.stop]
	// was missing. A timer that has already fired is past every check its
	// callback makes: it has seen `stopped` false, cleared its field, and is
	// about to call `run`. Stop then found nothing to cancel and returned —
	// so the reload it exists to prevent went on writing a config revision,
	// crediting an operator and waking every peer, while [Engine.Stop] was
	// closing the backends underneath it.
	//
	// Both halves are needed. The cancel is what makes an apply already in
	// flight give up rather than finish; the wait is what makes stop mean
	// "nothing is writing any more" rather than "nothing NEW will start".
	runs       sync.WaitGroup
	cancelRuns context.CancelFunc
	runCtx     context.Context

	// now and window are injectable so the cases can drive the clock
	// rather than sleep through it. Nil and zero take the real ones.
	now    func() time.Time
	window time.Duration
}

// republishWindow is how long one burst of seals is folded into.
//
// FIFTEEN SECONDS, from the two things it sits between. Below it is one
// operator action: connecting a third-party app from the dashboard writes one
// request per surface and Atlassian's alone applies three revisions in a row,
// all inside a second or two — so the window has to cover a burst plus the
// round trips a vendor pass makes between them. Above it is what a person
// waits: they pressed Connect and are watching the card, so the delay before
// the integration is live has to stay under the time it takes to wonder
// whether it worked.
//
// It is NOT a knob. A deployment has no honest reason to want a different
// value: the two bounds are a human's patience and a dialog's write pattern,
// and neither varies by where the engine runs.
const republishWindow = 15 * time.Second

// begin registers a run that is about to start, and hands it the context
// [republisher.stop] cancels. It reports false when shutdown has overtaken it.
//
// THE CALLER HOLDS r.mu, which is what makes the registration safe: stop sets
// `stopped` under the same lock before it waits, so a run that gets a yes here
// is counted before any wait can begin, and one that arrives after gets a no.
func (r *republisher) begin() (context.Context, bool) {
	if r.stopped {
		return nil, false
	}
	if r.runCtx == nil {
		// LAZILY, because the zero value of this type has to work: it is a
		// field on the engine rather than something constructed.
		r.runCtx, r.cancelRuns = context.WithCancel(context.Background())
	}
	r.runs.Add(1)
	return r.runCtx, true
}

func (r *republisher) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

func (r *republisher) every() time.Duration {
	if r.window > 0 {
		return r.window
	}
	return republishWindow
}

// request asks for a re-activation, crediting it to by.
//
// run is passed rather than captured so this type knows nothing about the
// engine — which is what lets the cases drive it with a recorder and assert
// the coalescing itself rather than its effects.
func (r *republisher) request(by iam.Actor, run func(context.Context, iam.Actor)) {
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return
	}
	now := r.clock()
	if elapsed := now.Sub(r.last); r.last.IsZero() || elapsed >= r.every() {
		r.last = now
		r.mu.Unlock()
		// AT ONCE, AND NOT ON THIS GOROUTINE, which are different things.
		//
		// # Why it must not run on the caller's goroutine
		//
		// The caller is [refreshingSink.Flush], which runs at the tail of a
		// provisioning pass — INSIDE the surface lease. [setup.LeaseTTL] is five
		// minutes, [setup.PassDeadline] stops the pass at four, and the minute
		// between them is not slack: it is what the status write that RECORDS
		// the pass has to complete in, under that same lease. This ran inline
		// with a fresh [setup.RecordDeadline] of its own, detached from the
		// pass's context and therefore not bounded by it — so a pass that used
		// its four minutes and then sealed a credential spent that minute HERE,
		// and the record went on to ask for another. The lease is gone by then:
		// a peer may hold the surface, and the write the lease exists to protect
		// lands outside it.
		//
		// Nothing about the reload needs the lease. It re-activates the current
		// revision through the config plane's own compare-and-set — not a write
		// at a third-party app, not a write to the surface's status row — so the
		// fix is to stop charging the lease for it, not to shorten it.
		//
		// # And the goroutine is a timer's, which is already the engine's
		//
		// Same ownership as the windowed path and its own field, because the two
		// are live at the same time: this run is in flight while the next burst
		// is arming the trailing one. [republisher.stop] disarms both, so a
		// re-activation is never written by a node that is leaving, and the run
		// bounds itself ([Engine.rebuildForSealedSecrets]). A person who pressed
		// Connect waits no longer than before — the reload starts now; what
		// stops waiting for it is the pass.
		r.startNow(run, by)
		return
	}
	// INSIDE THE WINDOW. The request is not dropped — it is what the run
	// at the end of the window will carry.
	r.by = by
	if r.pending != nil {
		r.mu.Unlock()
		return
	}
	wait := r.every() - now.Sub(r.last)
	r.pending = time.AfterFunc(wait, func() { r.fire(run) })
	r.mu.Unlock()
}

// startNow performs one run off the caller's goroutine, crediting it to the
// party who asked at that moment.
//
// Its own timer rather than [republisher.pending], which the trailing run
// needs: a burst arms that one while this is still in flight.
func (r *republisher) startNow(run func(context.Context, iam.Actor), by iam.Actor) {
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return
	}
	r.inflight = time.AfterFunc(0, func() {
		r.mu.Lock()
		r.inflight = nil
		ctx, ok := r.begin()
		r.mu.Unlock()
		if !ok {
			return
		}
		defer r.runs.Done()
		// DETACHED FROM THE CALLER, like [republisher.fire]: there is no
		// caller's context here at all, and [Engine.rebuildForSealedSecrets]
		// gives it the deadline. Not detached from SHUTDOWN, which is a
		// different question and the one this context answers.
		run(ctx, by)
	})
	r.mu.Unlock()
}

// fire performs the run one or more requests inside the window asked for.
func (r *republisher) fire(run func(context.Context, iam.Actor)) {
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return
	}
	r.pending = nil
	r.last = r.clock()
	by := r.by
	r.by = iam.Actor{}
	ctx, ok := r.begin()
	r.mu.Unlock()
	if !ok {
		return
	}
	defer r.runs.Done()
	// DETACHED FROM THE CALLER, like the inline path, and bound to shutdown
	// by the context begin hands over.
	run(ctx, by)
}

// stop ends every re-activation this type owns and returns once none is
// running.
//
// A TIMER IS A GOROUTINE'S LIFETIME and this one belongs to the engine. Left
// armed through a shutdown it would write a config revision from a node that
// has already released its seats — crediting an operator, waking every peer,
// on behalf of a process that is leaving.
//
// DISARMING WAS ONLY THE FIRST HALF. A timer stopped before it fires is
// cancelled; one that has already fired is a goroutine already past the
// `stopped` check, and Stop on it reports false and changes nothing. So a
// reload that had just begun ran to completion beside [Engine.Stop] closing
// the store and the broker under it — the exact write this function exists to
// prevent, reached through the one window it did not cover.
//
// Cancel, then WAIT. The cancel makes an apply already in flight give up at
// its next context check instead of finishing; the wait is what lets a caller
// treat this returning as "nothing is writing any more". Neither is enough
// alone: a cancelled apply still needs a moment to unwind, and a wait with no
// cancel would sit out a full config write.
//
// THE WAIT IS OUTSIDE THE LOCK, because a run takes it when it finishes.
// Holding it here would deadlock against the goroutine being waited for.
func (r *republisher) stop() {
	r.mu.Lock()
	r.stopped = true
	if r.pending != nil {
		r.pending.Stop()
		r.pending = nil
	}
	if r.inflight != nil {
		r.inflight.Stop()
		r.inflight = nil
	}
	cancel := r.cancelRuns
	r.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	r.runs.Wait()
}

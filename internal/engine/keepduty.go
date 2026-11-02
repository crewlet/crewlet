package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/schedule"
)

// A fleet duty KEPT while its work runs.
//
// # Why a duty's work renews its lease
//
// A duty is claimed once a turn, and its holder stays the holder by claiming
// again at the next turn, before the TTL runs out. Some turns run longer than
// that: the embedding duty's training of the semantic index, the object
// collector's audit of a store holding millions of objects. Renewed only
// between turns, such a lease lapses mid-work, a peer takes the duty, and two
// nodes do the singleton's work at once — the collector's two holders then
// writing the fleet's one record of it in turn, the older pass's last. So the
// work renews the lease while it runs ([dutyRenewal.keep]), and is ENDED when
// it can no longer say it holds it.
//
// # Three answers, three outcomes (ADR-0005)
//
// A renewal that answers HELD extends the window the work may run in. One that
// answers NOT HELD ends the work at once: another node holds the duty and runs
// its own. One the store does NOT ANSWER — refuses with an error, or HOLDS ON
// TO until the call gives up — changes nothing yet: the last confirmation
// still keeps every other node out until its window ends
// ([coord.ExclusiveThrough]), and the work ends only once that window has run
// out with no renewal confirming it, when another node may hold the duty. A
// store blip is not a lost duty, and treating it as one cut a training off
// over a single renewal nobody answered.
//
// HELD ON TO is the shape that matters on this broker. A claim begins with a
// read from the coordination stream's LEADER, and a member that is not the
// leader drops that request rather than refusing it — so while the leader is
// moving, through an election or a member restarting in a rolling upgrade, a
// renewal hears nothing at all until its own deadline. So each renewal's call
// is bounded by ONE RENEWAL INTERVAL as well as by the window: given the rest
// of the window, as it once was, the first such call held the work until the
// window had run out, and the renewal after it was never asked.
//
// # Four renewals a lease
//
// The renewals are PHASED FROM THE CONFIRMATION — the first one interval after
// the claim was asked, each after it an interval after the one before — never
// from when the work began, which a slow claim puts up to its own length
// later. The work stops one renewal interval before its lease can lapse — the
// margin [coord.ExclusiveThrough] takes, because the window's end is noticed at
// a renewal's tick — so a confirmation buys three intervals of the four a
// lease holds ([keptDutyRenewals]): the renewal due at the first may go
// unanswered for its whole interval, and the one due at the second is still
// asked inside the window, with an interval to answer in. Renewed at the
// cadence the duty is claimed on, a third of a lease, the window would hold
// two intervals and the renewal after an unanswered one would fall on its end,
// never asked: one blip would end a half-hour audit.
//
// # A duty declined while its work runs
//
// A duty that decides from a domain's rows is DECLINED by a node whose copy of
// one of them is out of service (ADR-0072, [dutyDecline]) — and the
// code-retention duty also by a node that does not serve the revision the
// fleet's activation pointer names ([Engine.servedFleetRevision]) — and either
// can become true while the work runs: a training tick of the embedding duty
// runs for minutes, and the fleet can activate a revision this node fails to
// apply while a pass is under way. So work kept with a decline check
// ([dutyRenewal.declining]) asks it before every renewal, and when it answers
// that the node now declines, the work is ENDED with [errDutyDeclined] and the
// duty is not renewed: renewed, the work would go on deciding from a copy that
// went wrong, or against a revision the fleet replaced, after it began. The
// lease is given back by the work's holder once the work has RETURNED, never
// by the renewal that ended it, because something the work already sent could
// otherwise land after another node had taken the duty.
//
// Lifted from the embedding duty's own keepClaimed, because written twice the
// rule is what would differ between the two.

// keptDutyRenewals is how many times a kept duty is renewed in one lease's TTL.
//
// FOUR, the fewest that lets a renewal the store refuses or holds on to cost
// the work nothing: see the file's doc. More would tolerate a longer outage of the
// coordination store at the price of a renewal written more often; the
// realistic failure is one renewal lost to a leader election, and four
// survives it with a quarter of the lease left as the margin against the
// store's clock running ahead of this node's.
const keptDutyRenewals = 4

// errDutyLost is the cause work holding a duty is ended with when a renewal
// answers that another node holds the duty.
var errDutyLost = errors.New("engine: another node holds the duty now")

// errDutyDeclined is the cause work holding a duty is ended with when this
// node declines the duty as the work runs ([dutyRenewal.declining]): a copy
// the work decides from has gone out of service (ADR-0072), or — for a duty
// that waits for it — this node no longer serves the revision the fleet's
// activation pointer names. The duty's `_declined` line says which.
var errDutyDeclined = errors.New("engine: this node declines the duty now")

// errDutyUnconfirmed is the cause work holding a duty is ended with when no
// renewal has confirmed the duty through the window its last confirmation
// bought ([coord.ExclusiveThrough]): the lease may have lapsed, and another node
// may hold it.
var errDutyUnconfirmed = errors.New("engine: no renewal has confirmed the duty for as long as its lease is sure to last")

// dutyClock is the clock a kept duty's renewals are asked, timed and bounded
// on — the system's, or a test's own, so a pass many leases long is driven
// renewal by renewal in microseconds, a renewal the store holds on to
// included.
type dutyClock interface {
	// Now is the instant a renewal is asked at.
	Now() time.Time
	// After delivers one tick once d has passed — at once for d at or below
	// zero — unless stop is called first.
	After(d time.Duration) (tick <-chan time.Time, stop func())
	// WithTimeout is ctx, ended once d has passed: how long one renewal's
	// call may take.
	WithTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc)
}

// systemClock is the system's [dutyClock].
type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

func (systemClock) After(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTimer(d)
	return t.C, func() { t.Stop() }
}

func (systemClock) WithTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, d)
}

// dutyRenewal is how work holding a fleet duty renews the duty's lease: every
// [keptDutyRenewals]th of ttl, the TTL the duty is claimed with.
type dutyRenewal struct {
	// duty is the duty's name, for the lines a renewal logs.
	duty string
	// ttl is the lease's TTL — the one the claim the work renews through
	// asks for.
	ttl time.Duration
	// clock is what renewals are asked and counted on.
	clock dutyClock
	// declined, where set, is asked before every renewal whether this node
	// now declines the duty ([dutyRenewal.declining]). Nil declines nothing.
	declined func(context.Context) bool
}

// newDutyRenewal is a renewal of a duty claimed with ttl, on the system's
// clock.
func newDutyRenewal(duty string, ttl time.Duration) dutyRenewal {
	return dutyRenewal{duty: duty, ttl: ttl, clock: systemClock{}}
}

// declining is r with declined asked before every renewal: when it answers
// that this node now declines the duty — a copy the work decides from went out
// of service, or this node stopped serving the fleet's revision ([dutyDecline])
// — the work is ended with [errDutyDeclined] and the duty is not renewed, and
// [keptDuty.Stop] says so, for the work's holder to give the lease back once
// the work has returned.
func (r dutyRenewal) declining(declined func(context.Context) bool) dutyRenewal {
	r.declined = declined
	return r
}

// every is how often a kept duty is renewed.
func (r dutyRenewal) every() time.Duration { return r.ttl / keptDutyRenewals }

// through is the window a confirmation asked at buys: the TTL from then, less
// one renewal interval.
func (r dutyRenewal) through(asked time.Time) time.Time {
	return coord.ExclusiveThrough(asked, r.ttl, r.every())
}

// keep renews the duty for the work it returns, from a claim that answered held
// having been asked at confirmed. The first renewal is due one interval after
// confirmed — at once, when the claim itself took that long — and each after it
// an interval after the one before ([dutyRenewal.next]), so every renewal falls
// where the window the last confirmation bought counts on it, however long the
// claim took. A nil claim is the single node with no fleet, which always holds
// its duties: the work runs undisturbed and Stop answers held.
//
// The work's context ends with ctx, at Stop, or when a renewal ends it — its
// cause [errDutyLost], [errDutyDeclined], or an error wrapping
// [errDutyUnconfirmed]. Stop MUST be called; it waits for the renewals'
// goroutine, so none outlives the work.
func (r dutyRenewal) keep(ctx context.Context, claim schedule.DutyFunc,
	confirmed time.Time) (context.Context, *keptDuty) {

	work, end := context.WithCancelCause(ctx)
	k := &keptDuty{renewal: r, end: end, done: make(chan struct{}), stopped: make(chan struct{})}
	k.stop = sync.OnceValues(k.finish)
	if claim == nil {
		k.always = true
		close(k.stopped)
		return work, k
	}
	k.until = r.through(confirmed)
	go func() {
		defer close(k.stopped)
		for due := r.next(confirmed); ; due = r.next(due) {
			tick, stop := r.clock.After(due.Sub(r.clock.Now()))
			select {
			case <-k.done:
				stop()
				return
			case <-work.Done():
				stop()
				return
			case <-tick:
			}
			stop()
			if !k.renew(work, claim) {
				return
			}
		}
	}()
	return work, k
}

// next is when the renewal after the one due at due is due: an interval on, or
// now where that has passed.
//
// FROM THE LAST RENEWAL'S DUE TIME, never from when it answered: a renewal the
// store held on to for its whole interval ([keptDuty.renew]) is followed by the
// next at once, still inside the window, where an interval after its answer
// would fall on the window's end. And RE-ANCHORED ON NOW rather than catching
// up, so a node that stopped running for several intervals asks once, not once
// for each renewal it slept through.
func (r dutyRenewal) next(due time.Time) time.Time {
	due = due.Add(r.every())
	if now := r.clock.Now(); due.Before(now) {
		return now
	}
	return due
}

// keptDuty is one piece of work's hold on a duty.
type keptDuty struct {
	renewal dutyRenewal
	// always is a duty with nobody to claim it from: no fleet.
	always bool
	// end ends the work, with the cause a renewal found.
	end context.CancelCauseFunc
	// done is closed by Stop, and stopped by the renewals' goroutine as it
	// returns.
	done, stopped chan struct{}
	stop          func() (bool, error)

	// mu guards what the renewals found. Never held across a claim or a
	// log line.
	mu sync.Mutex
	// until is the window the last confirmation bought, and last the last
	// renewal's error, nil once one has answered.
	until time.Time
	last  error
	// lost is set when a renewal answered that another node holds the duty,
	// declined when this node declined it before a renewal, and ended once a
	// window ran out with no renewal confirming it.
	lost, declined bool
	ended          error
}

// Stop ends the renewals, waits for their goroutine, ends the work, and
// answers whether this node holds the duty as the work ends — THREE-VALUED
// (ADR-0005): (true, nil) confirmed through now; (false, nil) a renewal
// answered that another node holds it; (false, err) the work did not end
// holding it as its holder — err is [errDutyDeclined] where this node declined
// the duty as the work ran, and otherwise no renewal has confirmed it through
// its window, err wrapping [errDutyUnconfirmed] and the last renewal's own
// error. Calling it again answers the same.
//
// DECLINED IS NOT "NOT HELD": the lease is still this node's until it is given
// back, so the holder that sees [errDutyDeclined] gives it back
// ([Engine.workerGiveBack]), and acts on nothing the work found meanwhile.
func (k *keptDuty) Stop() (bool, error) { return k.stop() }

func (k *keptDuty) finish() (bool, error) {
	close(k.done)
	k.end(nil) // a cause a renewal already set stays the cause
	<-k.stopped
	if k.always {
		return true, nil
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	switch {
	case k.lost:
		return false, nil
	case k.declined:
		return false, errDutyDeclined
	case k.ended != nil:
		return false, k.ended
	case !k.renewal.clock.Now().Before(k.until):
		// THE WINDOW RAN OUT between the last renewal's tick and the
		// work's end, before a tick could notice.
		return false, unconfirmed(k.until, k.last)
	}
	return true, nil
}

// renew is one renewal, and reports whether the renewals go on.
func (k *keptDuty) renew(work context.Context, claim schedule.DutyFunc) bool {
	r := k.renewal
	if r.declined != nil && r.declined(work) {
		// DECLINED FIRST, whatever the window says: a node that now
		// declines — its copy gone out of service under the work, or the
		// fleet's revision moved past it — decides nothing more, and a
		// renewal would only keep the duty from a node that does not.
		k.mu.Lock()
		k.declined = true
		k.mu.Unlock()
		k.end(errDutyDeclined)
		return false
	}
	asked := r.clock.Now()
	k.mu.Lock()
	until, last := k.until, k.last
	k.mu.Unlock()
	if !asked.Before(until) {
		// NOT ASKED: the window ran out with no renewal confirming it,
		// and an answer now could not cover the work done while asking.
		k.unconfirmed(work, until, last)
		return false
	}
	// ONE INTERVAL, AND NEVER PAST THE WINDOW IT IS TRYING TO EXTEND. A
	// renewal the store holds on to — a request no member answers while the
	// leader moves, which ends only at the call's own deadline — gives up
	// in time for the next to be asked inside the window: bounded by the
	// window alone, it held the work until the window had run out, and the
	// renewal after it was never asked. And none may keep the work running
	// past what the last confirmation covers.
	call, cancel := r.clock.WithTimeout(work, min(r.every(), until.Sub(asked)))
	held, err := claim(call)
	cancel()
	switch {
	case err == nil && held:
		k.mu.Lock()
		k.until, k.last = r.through(asked), nil
		k.mu.Unlock()
		return true
	case err == nil:
		k.mu.Lock()
		k.lost = true
		k.mu.Unlock()
		log.WarnContext(work, "duty_lost_mid_pass", "duty", r.duty,
			"detail", "the work is stopped; the node that holds the duty now runs its own")
		k.end(errDutyLost)
		return false
	case work.Err() != nil:
		// THE WORK ENDED — Stop, or the engine stopping — and the renewal
		// failed for that reason: it says nothing about the duty.
		return false
	}
	k.mu.Lock()
	k.last = err
	k.mu.Unlock()
	if !r.clock.Now().Before(until) {
		k.unconfirmed(work, until, err)
		return false
	}
	log.WarnContext(work, "duty_renewal_unanswered", "duty", r.duty, "error", err,
		"held_through", until.UTC(),
		"detail", "the work goes on: the last renewal keeps every other node out until then")
	return true
}

// unconfirmed ends the work whose window ran out at until with no renewal
// confirming it, last the last renewal's error.
func (k *keptDuty) unconfirmed(ctx context.Context, until time.Time, last error) {
	err := unconfirmed(until, last)
	k.mu.Lock()
	k.ended = err
	k.mu.Unlock()
	log.WarnContext(ctx, "duty_unconfirmed_mid_pass", "duty", k.renewal.duty, "error", last,
		"held_through", until.UTC(),
		"detail", "the work is stopped: the lease may have lapsed, and another node may hold the duty")
	k.end(err)
}

// unconfirmed is the error of a window that ran out at until with no renewal
// confirming it, wrapping the last renewal's own error where there was one.
func unconfirmed(until time.Time, last error) error {
	through := until.UTC().Format(time.RFC3339)
	if last == nil {
		return fmt.Errorf("%w (confirmed through %s)", errDutyUnconfirmed, through)
	}
	return fmt.Errorf("%w (confirmed through %s): %w", errDutyUnconfirmed, through, last)
}

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

// A fleet duty KEPT while its work runs. A duty is claimed once a turn, but
// some turns outlast its TTL (the embedding duty's training, the object
// collector's audit), and a lease renewed only between turns lapses mid-work,
// so two nodes run the singleton at once. So the work renews the lease while
// it runs ([dutyRenewal.keep]) and is ENDED once it cannot say it holds it.
//
// Three answers, three outcomes (ADR-0005): HELD extends the window the work
// may run in; NOT HELD ends it at once; NO ANSWER (an error, or a call the
// store holds on to) changes nothing until the last confirmation's window
// ([coord.ExclusiveThrough]) runs out unrenewed. A store blip is not a lost
// duty.

// errDutyLost is the cause work holding a duty is ended with when a renewal
// answers that another node holds the duty.
var errDutyLost = errors.New("engine: another node holds the duty now")

// errDutyDeclined is the cause work holding a duty is ended with when this
// node declines the duty as the work runs ([dutyRenewal.declining]). The
// duty's `_declined` line says why.
var errDutyDeclined = errors.New("engine: this node declines the duty now")

// errDutyUnconfirmed is the cause work holding a duty is ended with when no
// renewal has confirmed the duty through the window its last confirmation
// bought ([coord.ExclusiveThrough]): the lease may have lapsed, and another node
// may hold it.
var errDutyUnconfirmed = errors.New("engine: no renewal has confirmed the duty for as long as its lease is sure to last")

// dutyClock is the clock a kept duty's renewals are asked, timed and bounded
// on: the system's, or a test's, which drives a pass many leases long renewal
// by renewal.
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
// [coord.KeptRenewals]th of ttl, the TTL the duty is claimed with.
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

// declining is r with declined asked before every renewal: a node declines a
// duty when a copy it decides from is out of service (ADR-0079, [dutyDecline])
// or, for code retention, when it does not serve the fleet's revision
// ([Engine.servedFleetRevision]), and either can turn true mid-work. Then the
// work ends with [errDutyDeclined], unrenewed, rather than go on deciding from
// a copy gone wrong or a replaced revision. The holder gives the lease back
// once the work has RETURNED ([keptDuty.Stop]), never the renewal that ended
// it, or something the work already sent could land after another node took
// the duty.
func (r dutyRenewal) declining(declined func(context.Context) bool) dutyRenewal {
	r.declined = declined
	return r
}

// every is how often a kept duty is renewed: a [coord.KeptRenewals]th of the
// TTL. The work stops one interval before the lease can lapse (the margin
// [coord.ExclusiveThrough] takes, since the window's end is noticed at a
// tick), so a confirmation buys three intervals: the renewal due at the first
// may go unanswered for its whole interval and the next is still asked inside
// the window. At the claim cadence, a third of a TTL, the renewal after an
// unanswered one would fall on the window's end, and one blip would end a
// half-hour audit.
func (r dutyRenewal) every() time.Duration { return r.ttl / coord.KeptRenewals }

// through is the window a confirmation asked at buys: the TTL from then, less
// one renewal interval.
func (r dutyRenewal) through(asked time.Time) time.Time {
	return coord.ExclusiveThrough(asked, r.ttl, r.every())
}

// keep renews the duty for the work it returns, from a claim that answered
// held when asked at confirmed. Renewals are phased from confirmed (the first
// one interval after it, at once when the claim took that long), never from
// when the work began, so each falls where the window the last confirmation
// bought counts on it. A nil claim is the single node with no fleet, which
// always holds its duties: the work runs undisturbed and Stop answers held.
//
// The work's context ends with ctx, at Stop, or when a renewal ends it with
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

// next is when the renewal after the one due at due is due: an interval on,
// or now where that has passed.
//
// From the last renewal's DUE time, never its answer: after a renewal the
// store held for its whole interval ([keptDuty.renew]) the next is asked at
// once, still inside the window. Re-anchored on now rather than catching up,
// so a node stalled for several intervals asks once.
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
		// DECLINED FIRST, whatever the window says: a node that declines
		// decides nothing more, and a renewal would only keep the duty from a
		// node that does not.
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
	// ONE INTERVAL, AND NEVER PAST THE WINDOW. A claim reads from the
	// coordination stream's leader, and a non-leader member drops the request
	// rather than refusing it, so while the leader moves (an election, a
	// rolling restart) a renewal hears nothing until its deadline. Bounded by
	// the window alone, that one call held the work until the window ran out,
	// the next renewal never asked.
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
		// The work ended (Stop, or the engine stopping) and the renewal failed
		// for that reason: it says nothing about the duty.
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

package engine

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/queue/topics"
)

// THE BUDGET PARK: A SEAT OUT OF ROOM WAITS FOR ITS WINDOW, IT DOES NOT BURN
// ITS MAIL.
//
// A token ceiling is per calendar window (ADR-0019), so "this seat cannot run"
// has an end: the instant the refusing window turns over, or an apply that
// raises the ceiling. Before this, nothing knew that. A wake reaching a seat
// whose window was spent ran a turn, the turn's first charge was refused, and
// a refusal proves nothing reached outside the engine — so the dispatcher
// NAKed it, and the redelivery was refused again, twenty-five times across
// about ten minutes of backoff, until the broker dead-lettered a perfectly
// healthy message. A company that hit its daily ceiling at ten in the morning
// lost every message its seats were sent for the rest of the day.
//
// So BEFORE A DELIVERY IS CLAIMED — before the completion ledger, before the
// answer offer to a parked coding run, before any model — the dispatcher asks
// whether one of the seat's capped windows is refusing ([meter.refusing]). If
// one is, the seat is PARKED:
//
//   - a pause hold under [pauseReasonBudget] is taken on its inbox, so nothing
//     more is delivered to it;
//   - the delivery is DEFERRED with a reason naming the window — `budget: day
//     window 2026-09-23 resets 2026-09-24T07:00:00Z` — which hands it back
//     unacked for one of its deliveries, and quiesces the attachment;
//   - an alarm is armed for the window's end.
//
// The hold is released at the reset, or at once by an apply that changes what
// the park was decided under (a ceiling of either scope, or the company's
// clock, which moves every window's end), and the mail is delivered again in
// order. A turn refused MID-FLIGHT is the same condition found a phase later,
// and is answered the same way unless the turn already acted outside the
// engine — that one is recorded and acked ([Dispatcher.abandon]), because a
// redelivery would repeat what it did.
//
// TWO STOPS, AND EACH HAS ITS OWN OWNER. The quiesce a deferral makes belongs
// to the seat host: the dispatcher notes it ([Dispatcher.NoteDeferred]) and the
// next successful lease renew resumes the attachment, exactly as it does for
// every other deferral. The HOLD is the park's, and is what keeps the resumed
// attachment from being handed anything. Releasing the park therefore lifts
// the hold and never touches the quiesce — an Unquiesce from here could resume
// a consumer the seat host stopped because it cannot prove it owns the seat,
// which is the one state a consumer must never be resumed in. And it closes
// the race a quiesce alone would open: an apply's release landing between a
// deferral's return and the quiesce it causes would find nothing to resume and
// leave the seat deaf for the rest of the window, while a hold is taken inside
// the handler, before the deferral returns.
//
// ONE DELIVERY PER PARK, not one per renew: the renew resumes the attachment,
// and the hold keeps it from being handed the message again until the park is
// released.
//
// KEYED ON THE SEAT'S ID, like every hold on a mailbox: the inbox the hold is
// taken on is named by the id (ADR-0026), so a park recorded under the handle
// would be released, after a rename, by resuming a topic built from the new
// handle — leaving the real one held for the rest of the window. The handle
// rides along for the log lines.
//
// WHO HAS TO AGREE ON IT: this node alone. A park is a pause hold in this
// process's queue client and an alarm in its memory. It is derived from the
// fleet's shared counters every time a delivery arrives, so a restart, or the
// seat moving to a peer — whose attachment carries no hold — simply asks the
// counters again on the first delivery.

// pauseReasonBudget is the hold name the budget park takes on a seat's inbox.
//
// A STABLE KEY, like [pauseReasonNoTurnEngine] beside it and for the same
// reason: holds are keyed by reason so two subsystems gating one inbox cannot
// release each other's, and a key derived from prose would strand every seat
// parked under the old wording.
const pauseReasonBudget = "budget_window"

// alarm is an armed reset: stopped when the park is released some other way.
type alarm interface{ Stop() bool }

// budgetParks is every seat inbox this node holds for want of budget, by the
// seat's id.
type budgetParks struct {
	mu    sync.Mutex
	seats map[uuid.UUID]*budgetParking

	// stopped refuses new alarms once the engine is tearing down: an alarm
	// armed after [Engine.stopBudgetParks] would fire into a closed queue.
	stopped bool

	// now and after are the clock and the alarm, injectable for tests; nil
	// is the wall clock and time.AfterFunc.
	now   func() time.Time
	after func(d time.Duration, f func()) alarm
}

// budgetParking is one parked seat: what the park was decided under, when the
// refusing window turns over, and the handle the seat answered to then, for the
// log lines.
type budgetParking struct {
	handle   string
	basis    budgetBasis
	resetsAt time.Time
	alarm    alarm
}

func (p *budgetParks) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

func (p *budgetParks) arm(d time.Duration, f func()) alarm {
	if p.after != nil {
		return p.after(d, f)
	}
	return time.AfterFunc(d, f)
}

// budgetPark is the dispatcher's budget stage: it parks handle's seat when one
// of its capped windows is refusing, reporting the deferral reason.
//
// What it does NOT do is decide an unknown: a counter that cannot be read
// lets the delivery through, logged, because the turn's own meter is the gate
// and fails closed — parking on a read that failed would hold a seat's mail on
// a store blip that the meter would have ridden out. The error it returns is
// the hold that could not be taken, which the dispatcher NAKs.
func (e *Engine) budgetPark(ctx context.Context, handle string) (string, bool, error) {
	c := e.Company()
	m, ok := e.meterFor(c, handle).(*meter)
	if !ok || m == nil || !m.basis.capped() {
		// No counter, or nothing to refuse with.
		return "", false, nil
	}
	m.now = e.budgetParks.clock
	r, refusing, err := m.refusing(ctx)
	if err != nil {
		log.WarnContext(ctx, "budget_park_unknown", "seat", handle, "error", err,
			"detail", "the counters could not be read, so the delivery runs and "+
				"its own meter decides whether a round fits")
		return "", false, nil
	}
	if !refusing {
		return "", false, nil
	}
	seat, err := e.seatID(handle)
	if err != nil {
		return "", false, err
	}
	subject, group := topics.AgentInbox(seat), topics.AgentInboxGroup(seat)

	// THE RACE WITH THE RELEASE is closed by ordering, as the no-model
	// pause closes its own: the hold is taken and recorded under the lock,
	// and the epoch is read again after that. An apply installs its epoch
	// before it reconciles the parks, so either the reconcile sees this
	// park or the read below sees the epoch that makes it unnecessary. The
	// queue call does not drain, so holding the lock across it cannot call
	// back into here.
	e.budgetParks.mu.Lock()
	err = e.backends.Queue.PauseTopic(ctx, subject, group, pauseReasonBudget)
	if err == nil {
		e.recordBudgetParkLocked(ctx, seat, handle, m.basis, r.Window.End)
	}
	e.budgetParks.mu.Unlock()
	if err != nil {
		return "", false, fmt.Errorf("engine: park %s on its budget: %w", handle, err)
	}

	reason := fmt.Sprintf("budget: %s window %s resets %s",
		r.Window.Period, r.Window.Label, rfc3339(r.Window.End))
	log.InfoContext(ctx, "seat_budget_parked", "seat", handle, "scope", r.Scope,
		"period", string(r.Window.Period), "window", r.Window.Label,
		"used", r.Used, "limit", r.Limit, "resets_at", rfc3339(r.Window.End),
		"detail", "this seat's mail waits on its inbox until the window turns "+
			"over or a revision raises the ceiling")
	if !budgetBasisOf(e.Company(), seat).equal(m.basis) {
		e.releaseBudgetPark(ctx, seat, "the ceilings changed while it parked")
	}
	return reason, true, nil
}

// recordBudgetParkLocked records a park and arms its reset. The caller holds
// e.budgetParks.mu.
//
// A seat parked again while already parked — a second partition of the drain
// that parked it — replaces the record, and the earlier alarm is stopped: the
// ALARM IS THE RECORD'S, never the seat's, so an alarm that fires after its
// record was replaced releases nothing (see [Engine.releaseBudgetParking]).
//
// The alarm releases on context.WithoutCancel(ctx): it fires hours after the
// delivery that parked the seat has settled and its context has ended, and a
// release that inherited that cancellation would lift nothing — while the
// delivery's trace, which the release belongs to, is still worth carrying.
func (e *Engine) recordBudgetParkLocked(ctx context.Context, seat uuid.UUID, handle string,
	basis budgetBasis, resetsAt time.Time) {
	p := &e.budgetParks
	if p.seats == nil {
		p.seats = map[uuid.UUID]*budgetParking{}
	}
	if old := p.seats[seat]; old != nil && old.alarm != nil {
		old.alarm.Stop()
	}
	parking := &budgetParking{handle: handle, basis: basis, resetsAt: resetsAt}
	p.seats[seat] = parking
	if p.stopped {
		return
	}
	// Never a negative wait: a window cut on this clock contains the moment
	// it was cut at, so its end is ahead of it, but the alarm is measured on
	// the monotonic clock and a wall clock stepped forward between the two
	// reads must not arm an alarm that has already passed as though it had
	// not.
	wait := max(resetsAt.Sub(p.clock()), 0)
	released := context.WithoutCancel(ctx)
	parking.alarm = p.arm(wait, func() {
		e.releaseBudgetParking(released, seat, parking, "the window turned over")
	})
}

// releaseBudgetPark lifts the budget hold from one seat's inbox, whatever
// record holds it.
func (e *Engine) releaseBudgetPark(ctx context.Context, seat uuid.UUID, why string) {
	e.budgetParks.mu.Lock()
	parking := e.budgetParks.seats[seat]
	e.budgetParks.mu.Unlock()
	if parking != nil {
		e.releaseBudgetParking(ctx, seat, parking, why)
	}
}

// releaseBudgetParking lifts the hold parking took, if parking is still the
// seat's record.
//
// OUTSIDE THE LOCK for the queue call, because the in-memory twin's resume
// drains synchronously into the seat's handler, and a delivery that finds the
// window still refusing comes straight back to [Engine.budgetPark].
func (e *Engine) releaseBudgetParking(ctx context.Context, seat uuid.UUID, parking *budgetParking, why string) {
	e.budgetParks.mu.Lock()
	if e.budgetParks.seats[seat] != parking {
		// Replaced by a later park, or already released: this alarm, or
		// this caller, is describing a hold that is somebody else's now.
		e.budgetParks.mu.Unlock()
		return
	}
	delete(e.budgetParks.seats, seat)
	if parking.alarm != nil {
		parking.alarm.Stop()
	}
	e.budgetParks.mu.Unlock()

	subject, group := topics.AgentInbox(seat), topics.AgentInboxGroup(seat)
	if err := e.backends.Queue.ResumeTopic(ctx, subject, group, pauseReasonBudget); err != nil {
		// Kept on the record with no alarm, as the no-model release keeps
		// its own: a resume fails only on a client that has stopped, which
		// is a process on its way out, and the record is what
		// [Engine.stopBudgetParks] and the seat's release still clear.
		e.budgetParks.mu.Lock()
		if _, again := e.budgetParks.seats[seat]; !again {
			if e.budgetParks.seats == nil {
				e.budgetParks.seats = map[uuid.UUID]*budgetParking{}
			}
			e.budgetParks.seats[seat] = &budgetParking{
				handle: parking.handle, basis: parking.basis, resetsAt: parking.resetsAt,
			}
		}
		e.budgetParks.mu.Unlock()
		log.WarnContext(ctx, "seat_budget_park_not_released", "seat", parking.handle,
			"agent_id", seat.String(), "error", err,
			"detail", "this seat's held mail stays on its inbox until the queue "+
				"client is replaced")
		return
	}
	log.InfoContext(ctx, "seat_budget_park_released", "seat", parking.handle,
		"agent_id", seat.String(), "reason", why)
}

// reconcileBudgetParks releases every park the company next decides
// differently.
//
// Run on every published company ([Engine.followCompany]) — at boot and on
// every apply — AFTER next is current, so a released inbox's first delivery
// is judged under it. A park whose basis is unchanged stays for its alarm: the
// counters have not moved in its favour and the ceiling it waits on is the
// same. One whose ceilings moved — raised, lowered, removed, or the seat gone
// from the company — is released at once, and the redelivery asks the
// counters again under the new ceilings: a lowered one parks it again, at the
// cost of one delivery, which is cheaper than deciding here what only the
// counters know.
func (e *Engine) reconcileBudgetParks(ctx context.Context, next *Company) {
	e.budgetParks.mu.Lock()
	var changed []uuid.UUID
	for seat, parking := range e.budgetParks.seats {
		if !budgetBasisOf(next, seat).equal(parking.basis) {
			changed = append(changed, seat)
		}
	}
	e.budgetParks.mu.Unlock()
	for _, seat := range sortedSeats(slices.Values(changed)) {
		e.releaseBudgetPark(ctx, seat, "a change of the company moved the ceilings it parked on")
	}
}

// forgetBudgetPark drops a released seat's park without resuming anything.
//
// The seat's release DETACHED its inbox, and a detach releases every hold the
// attachment carried, so there is nothing to lift — only an alarm that would
// otherwise fire into a seat this node no longer holds.
func (e *Engine) forgetBudgetPark(seat uuid.UUID) {
	e.budgetParks.mu.Lock()
	defer e.budgetParks.mu.Unlock()
	if parking := e.budgetParks.seats[seat]; parking != nil {
		if parking.alarm != nil {
			parking.alarm.Stop()
		}
		delete(e.budgetParks.seats, seat)
	}
}

// stopBudgetParks disarms every reset alarm, for a node shutting down.
//
// The holds are left to the queue client, which the node's stop detaches: a
// resume here would only hand a stopping node mail it is about to give back.
func (e *Engine) stopBudgetParks() {
	e.budgetParks.mu.Lock()
	defer e.budgetParks.mu.Unlock()
	e.budgetParks.stopped = true
	for _, seat := range sortedSeats(maps.Keys(e.budgetParks.seats)) {
		if parking := e.budgetParks.seats[seat]; parking.alarm != nil {
			parking.alarm.Stop()
			parking.alarm = nil
		}
	}
}

// budgetBasisOf is the basis a seat's spend is judged by under c, by the
// seat's id. A seat c no longer holds is judged by the company's ceilings
// alone.
func budgetBasisOf(c *Company, seat uuid.UUID) budgetBasis {
	if c == nil || c.Org == nil {
		return basisOf(c, nil)
	}
	return basisOf(c, c.Org.AgentSeatByID(seat))
}

// equal reports whether two bases decide a park alike: the same ceilings in
// each scope and the same clock.
//
// THE CLOCK COUNTS, not only the ceilings, because it is what cuts the windows:
// a company that moves its zone moves the end of every window, so the alarm a
// park armed under the old clock is set for an instant that no longer ends
// anything.
func (b budgetBasis) equal(o budgetBasis) bool {
	return maps.Equal(b.org, o.org) && maps.Equal(b.seat, o.seat) &&
		zoneName(b.zone) == zoneName(o.zone)
}

func zoneName(loc *time.Location) string {
	if loc == nil {
		return time.UTC.String()
	}
	return loc.String()
}

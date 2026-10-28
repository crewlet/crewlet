package engine

import (
	"context"
	"fmt"
	"maps"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/sandbox"
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
//   - an alarm is armed for the window's end;
//   - and the refusal is recorded on the shared counter as the gate's
//     ([meter.turnAway]), so the window's `refused_at` says when its seats
//     were last turned away — a window a coding run or a background pass
//     filled used to park every delivery it was sent while reading as one
//     that had refused nothing.
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
// taken on is named by the agent id derived from the handle (ADR-0013). The
// handle rides along for the log lines.
//
// WHO HAS TO AGREE ON IT: this node alone. A park is a pause hold in this
// process's queue client and an alarm in its memory. It is derived from the
// fleet's shared counters every time a delivery arrives, so a restart, or the
// seat moving to a peer — whose attachment carries no hold — simply asks the
// counters again on the first delivery.

// pauseReasonBudget is the hold name the budget park takes on a seat's inbox.
//
// A STABLE KEY, like [pauseReasonNoTurnEngine] beside it and for the same
// reason: see [parkSpec.hold].
const pauseReasonBudget = "budget_window"

// budgetParkSpec is the budget park's hold and log lines.
var budgetParkSpec = parkSpec{
	hold:        pauseReasonBudget,
	released:    "seat_budget_park_released",
	notReleased: "seat_budget_park_not_released",
}

// budgetParks is every seat inbox this node holds for want of budget, by the
// seat's id, each with the ceilings it was decided under.
type budgetParks = seatParks[budgetBasis]

// budgetPark is the dispatcher's budget stage: it parks handle's seat when one
// of its capped windows is refusing, reporting the deferral reason.
//
// What it does NOT do is decide an unknown: a counter that cannot be read
// lets the delivery through, logged, because the turn's own meter is the gate
// and fails closed — parking on a read that failed would hold a seat's mail on
// a store blip that the meter would have ridden out. The error it returns is
// the hold that could not be taken, which the dispatcher DEFERS (see
// [Dispatcher.parkOnBudget]): the refusal is the node's, and a deferral keeps
// the delivery's place at the head of the seat's inbox.
func (e *Engine) budgetPark(ctx context.Context, handle string) (string, bool, error) {
	c := e.Company()
	m := e.meterFor(c, handle)
	if m == nil || !m.basis.capped() {
		// No counter, or nothing to refuse with.
		return "", false, nil
	}
	r, refusing, err := m.refusing(ctx)
	starved := false
	if err == nil && !refusing {
		// AND A WINDOW TOO SHORT FOR A LAUNCH, which every turn makes: a
		// seat with room left but less than the executor floor refuses
		// every turn at its launch ([executorHeadroom]), and a seat that
		// is not parked takes each redelivery into the same refusal until
		// the broker dead-letters it. Parked, it waits for the window that
		// starves it to turn over, exactly as for a full one.
		r, starved, err = m.starved(ctx)
	}
	if err != nil {
		log.WarnContext(ctx, "budget_park_unknown", "seat", handle, "error", err,
			"detail", "the counters could not be read, so the delivery runs and "+
				"its own meter decides whether a round fits")
		return "", false, nil
	}
	if !refusing && !starved {
		return "", false, nil
	}
	seat, err := e.seatID(handle)
	if err != nil {
		return "", false, err
	}
	// THE RACE WITH THE RELEASE is closed by ordering, as the no-model
	// pause closes its own: the hold is taken and recorded under the lock
	// ([seatParks.take]), and the basis is read again after that.
	if err := e.budgetParks.take(ctx, e, budgetParkSpec, seat, handle, m.basis, r.Window.End,
		"the window turned over"); err != nil {
		return "", false, fmt.Errorf("engine: park %s on its budget: %w", handle, err)
	}
	reason := refusingReason(r)
	if refusing {
		// THE PARK IS THE GATE SAYING NO, and it is recorded as the
		// window's refusal ([meter.turnAway]): the delivery it defers is
		// the turn whose first charge would have been refused, and stamped
		// the window, had it run. Recorded once the hold is taken, so a
		// park that could not be made — the delivery NAKed and asked about
		// again — is one refusal and not two. A STARVED window is not
		// stamped: it has room, and the counter stamps only a window with
		// none, so there is no refusal of a charge to record.
		m.turnAway(ctx, r)
	} else {
		reason = starvedReason(r, m.basis.floor)
	}
	log.InfoContext(ctx, "seat_budget_parked", "seat", handle, "scope", r.Scope,
		"period", string(r.Window.Period), "window", r.Window.Label,
		"used", r.Used, "limit", r.Limit, "floor", m.basis.floor, "starved", starved,
		"resets_at", rfc3339(r.Window.End),
		"detail", "this seat's mail waits on its inbox until the window turns "+
			"over or a revision raises the ceiling")
	if !budgetBasisOf(e.Company(), seat).equal(m.basis) {
		e.releaseBudgetPark(ctx, seat, "the ceilings changed while it parked")
	}
	return reason, true, nil
}

// budgetRefusing is the budget stage's first question ASKED WITHOUT PARKING:
// whether one of a seat's capped windows is refusing outright, as the refusal
// that lifts when that window turns over. For a caller that is not a delivery
// (the turn gate every resume passes, [Engine.seatCannotRun]) and so has no
// mail of its own to hold: it waits for the window's end rather than parks, and
// a delivery that arrives meanwhile takes the park itself. An unreadable
// counter is not a refusal, for the reason [Engine.budgetPark] gives.
//
// A FULL WINDOW ONLY, and not one merely too short for a launch, which
// [Engine.budgetHold] asks as well: the gate asks this of a completion, whose
// first model call is its reviewer's, which a window below the launch floor
// can still pay for. The floor refuses a launch, and a review that sends its
// run back is held at that relaunch ([Engine.heldByBudget]).
func (e *Engine) budgetRefusing(ctx context.Context, c *Company, handle string) (sandbox.Refusal, bool) {
	m := e.meterFor(c, handle)
	if m == nil || !m.basis.capped() {
		return sandbox.Refusal{}, false
	}
	r, refusing, err := m.refusing(ctx)
	if err != nil || !refusing {
		return sandbox.Refusal{}, false
	}
	return sandbox.Refusal{Condition: waitBudget, Reason: refusingReason(r), Until: r.Window.End}, true
}

// budgetHold is what a seat's budget holds a resumed turn back for: a capped
// window refusing outright ([Engine.budgetRefusing]), or one leaving less room
// than an executor launch needs ([meter.starved]) — the two conditions the
// budget stage parks a seat's mail on — as a refusal that lifts when that
// window turns over. False where neither holds, and where the counters cannot
// be read, for the reason [Engine.budgetPark] gives.
//
// FOR A RESUME THAT WILL LAUNCH, which has no mail to park. The turn gate asks
// it of every attempt at a recorded answer's resume ([Engine.seatCannotRun]) —
// an answer always relaunches its executor, so a starved window refuses it as
// surely as a full one — and a collected job's resume that its relaunch was
// refused for is held on it ([sandbox.ErrResumeHeld]).
func (e *Engine) budgetHold(ctx context.Context, c *Company, handle string) (sandbox.Refusal, bool) {
	if refusal, refusing := e.budgetRefusing(ctx, c, handle); refusing {
		return refusal, true
	}
	m := e.meterFor(c, handle)
	if m == nil || !m.basis.capped() {
		return sandbox.Refusal{}, false
	}
	r, starved, err := m.starved(ctx)
	if err != nil || !starved {
		return sandbox.Refusal{}, false
	}
	return sandbox.Refusal{Condition: waitBudget, Reason: starvedReason(r, m.basis.floor),
		Until: r.Window.End}, true
}

// refusingReason is the reason a seat waits on a window r with no room left:
// a parked delivery's deferral and a held resume's refusal alike, so the two
// read the same in a log line and on a dashboard.
func refusingReason(r refusal) string {
	return fmt.Sprintf("budget: %s window %s resets %s",
		r.Window.Period, r.Window.Label, rfc3339(r.Window.End))
}

// starvedReason is the reason a seat waits on a window r whose room is below
// floor, the tokens an executor launch needs; see [refusingReason].
func starvedReason(r refusal, floor int) string {
	return fmt.Sprintf("budget: %s window %s leaves %d tokens, below the %d an executor "+
		"launch needs (turn_engine.executor_min_budget_tokens); resets %s",
		r.Window.Period, r.Window.Label, max(r.Limit-r.Used, 0), floor,
		rfc3339(r.Window.End))
}

// releaseBudgetPark lifts the budget hold from one seat's inbox, whatever
// record holds it.
func (e *Engine) releaseBudgetPark(ctx context.Context, seat uuid.UUID, why string) {
	e.budgetParks.release(ctx, e, budgetParkSpec, seat, why)
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
	for _, seat := range e.budgetParks.changed(func(seat uuid.UUID, basis budgetBasis) bool {
		return budgetBasisOf(next, seat).equal(basis)
	}) {
		e.releaseBudgetPark(ctx, seat, "a change of the company moved the ceilings it parked on")
	}
}

// forgetBudgetPark drops a released seat's park without resuming anything;
// see [seatParks.forget].
func (e *Engine) forgetBudgetPark(seat uuid.UUID) { e.budgetParks.forget(seat) }

// stopBudgetParks disarms every reset alarm, for a node shutting down; see
// [seatParks.stop].
func (e *Engine) stopBudgetParks() { e.budgetParks.stop() }

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
// each scope, the same clock and the same executor floor.
//
// THE CLOCK COUNTS, not only the ceilings, because it is what cuts the windows:
// a company that moves its zone moves the end of every window, so the alarm a
// park armed under the old clock is set for an instant that no longer ends
// anything. AND THE FLOOR: a seat parked starved under one floor may have room
// for a launch under a lower one now, and one parked full is starved sooner
// under a higher one.
func (b budgetBasis) equal(o budgetBasis) bool {
	return maps.Equal(b.org, o.org) && maps.Equal(b.seat, o.seat) &&
		zoneName(b.zone) == zoneName(o.zone) && b.floor == o.floor
}

func zoneName(loc *time.Location) string {
	if loc == nil {
		return time.UTC.String()
	}
	return loc.String()
}

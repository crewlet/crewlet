package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sync"
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
// answer offer to a parked executor, before any model — the dispatcher asks
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
//     were last turned away — a window an executor launch or a background pass
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
// seat's id, each with the ceilings it was decided under, which a change of
// the company releases it on. The floor it waits for room for (the static
// floor, or the one a launch was refused below: its brief beside the static
// floor, a continued session's context beside its brief, or, where its fresh
// brief's floor is held, the static floor or the room below it) is the
// refusal's, read again on the delivery the park hands back, and no reader asks
// it of the park: an ask waiting behind the park is owed for as long as the
// park holds, which its holder answers (parkheld.go).
type budgetParks = seatParks[budgetBasis]

// floorWaits is every seat this node parked on a launch's refusal below its
// floor, by the seat's id, with the window the park waited on
// ([Engine.budgetPark]): what the seat's next dispatched launch takes to know
// that its turn waited a turnover for that floor
// ([agentLauncher.continuedFloor]), and so is held, in a later window, to no
// more than the floor a launch with no brief needs.
//
// WAITED FOR WITHOUT A BOUND, A FLOOR ABOVE THE STATIC ONE HELD A DELIVERY FOR
// EVER. A launch's brief, or a session it continues, can take its floor within
// the most room a window can leave it and past what a busy window leaves at its
// read, the company's other seats spending there first after every turnover:
// refused, parked and refused again in every window, the delivery never ran,
// where before the brief was counted a fresh brief's or an unmeasured
// session's launch went out on the static floor, and a measured session's,
// whose floor counted its brief already, had that band then too. So it waits
// one turnover, and no more.
//
// WHO HAS TO AGREE ON IT: this node alone, as the park does. It rides the park,
// and the seat's next launch is this node's: the park holds the seat's whole
// inbox, and its release hands the seat's mail back in order, the delivery it
// deferred at its head. A seat that moves takes nothing of it to its next
// holder, which may hold the launch one window more, still bounded: refused
// there, it is parked there, and its next try there takes the record that park
// writes. TAKEN, never read, by the seat's next launch, whichever delivery's it
// is: a record left behind would hold a later delivery's launch, which never
// waited, below its floor. And dropped with the seat's other parks when this
// node lets the seat go ([Engine.forgetBudgetPark]).
type floorWaits struct {
	mu    sync.Mutex
	seats map[uuid.UUID]sandbox.BudgetWindow
}

// parked records that seat's launch was refused below its floor and its park
// waits on window w, replacing what an earlier park recorded.
func (f *floorWaits) parked(seat uuid.UUID, w sandbox.BudgetWindow) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.seats == nil {
		f.seats = map[uuid.UUID]sandbox.BudgetWindow{}
	}
	f.seats[seat] = w
}

// take is the window seat's last park on a launch's floor waited on, the zero
// window where none is recorded, and forgets it.
func (f *floorWaits) take(seat uuid.UUID) sandbox.BudgetWindow {
	f.mu.Lock()
	defer f.mu.Unlock()
	w := f.seats[seat]
	delete(f.seats, seat)
	return w
}

// forget drops what seat's parks recorded, for a seat this node lets go of.
func (f *floorWaits) forget(seat uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.seats, seat)
}

// launchRefusedIn is the window r names where refused is an executor launch's
// refusal below its floor ([floorRefusal]), the zero window for every other
// refusal: a round's, or none. What a launch's next try reads to know it has
// waited out the window it was refused in ([floorWaits],
// [sandbox.Refusal.Window]).
func launchRefusedIn(refused error, r refusal) sandbox.BudgetWindow {
	var below *floorRefusal
	if !errors.As(refused, &below) {
		return sandbox.BudgetWindow{}
	}
	return sandbox.BudgetWindow{Period: r.Window.Period, Label: r.Window.Label}
}

// budgetPark is the dispatcher's budget stage: it parks handle's seat when one
// of its capped windows is refusing, or leaves less room than the delivery's
// launch needs, reporting the deferral reason. refused is the budget refusal the
// delivery's turn met, nil before any turn ran: the floor the park waits for
// room for is the one that launch was refused below ([floorOf]): its brief
// beside the static floor; for a first launch that continues its
// conversation's session, that session's context beside its brief; and, where
// its fresh brief's floor is held, the static floor or the room below it
// ([launchFloor.tokens]). It is the static floor where no launch was refused.
//
// AND WHERE A LAUNCH WAS, the window the park waits on is recorded for the
// seat's next launch ([floorWaits]), which takes it in a later window and is
// held there to no more than the floor a launch with no brief needs: a floor
// within the most room a window can leave the launch is still past what a busy
// company's window leaves at the launch's read, and waited for again in every
// window, it went out in none. So the launch waits one turnover for it, and
// no more.
//
// What it does NOT do is decide an unknown: a counter that cannot be read
// lets the delivery through, logged, because the turn's own meter is the gate
// and fails closed — parking on a read that failed would hold a seat's mail on
// a store blip that the meter would have ridden out. The error it returns is
// the hold that could not be taken, which the dispatcher DEFERS (see
// [Dispatcher.parkOnBudget]): the refusal is the node's, and a deferral keeps
// the delivery's place at the head of the seat's inbox.
func (e *Engine) budgetPark(ctx context.Context, handle string, refused error) (string, bool, error) {
	c := e.Company()
	m := e.meterFor(c, handle)
	if m == nil || !m.basis.capped() {
		// No counter, or nothing to refuse with.
		return "", false, nil
	}
	floor := floorOf(c, refused)
	r, refusing, starved, err := m.parks(ctx, floor)
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
	// THE WINDOW A LAUNCH WAS REFUSED BELOW ITS FLOOR IN, recorded with the
	// park and before the seat is asked for below, for the race that ask
	// closes for the park: a release that finds the seat gone forgets both.
	if w := launchRefusedIn(refused, r); w.Valid() {
		e.floorWaits.parked(seat, w)
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
		reason = starvedReason(r, floor)
	}
	log.InfoContext(ctx, "seat_budget_parked", "seat", handle, "scope", r.Scope,
		"period", string(r.Window.Period), "window", r.Window.Label,
		"used", r.Used, "limit", r.Limit, "floor", floor.tokens(), "starved", starved,
		"resets_at", rfc3339(r.Window.End),
		"detail", "this seat's mail waits on its inbox until the window turns "+
			"over or a revision raises the ceiling")
	// THE SEAT AGAIN, once the park is taken: a turn refused mid-flight parks
	// its seat after the turn ran, and the seat's release waits for no turn,
	// so the release can have forgotten the seat's parks already. A park left
	// on a seat this node let go of held its inbox until the window's end and
	// fired its alarm into a seat a peer runs. The release takes the seat out
	// of this node's hands before it forgets the parks, so a park that finds
	// it held is one that release will forget ([seatParks.forget]).
	if _, held := e.launchFence(handle); !held {
		e.releaseBudgetPark(ctx, seat, seatReleasedHere)
		e.floorWaits.forget(seat)
		return reason, true, nil
	}
	if !budgetBasisOf(e.Company(), seat).equal(m.basis) {
		e.releaseBudgetPark(ctx, seat, "the ceilings changed while it parked")
	}
	return reason, true, nil
}

// parks reports the window that parks a seat's mail, and which of the two
// conditions holds it there: a capped window refusing outright, or — where none
// is — one leaving less room than floor, the room the delivery's launch needs
// ([meter.starved]).
//
// AND A WINDOW TOO SHORT FOR A LAUNCH, which every turn makes: a seat with room
// left but less than the executor floor refuses every turn at its launch
// ([executorHeadroom]), and a seat that is not parked takes each redelivery
// into the same refusal until the broker dead-letters it. Parked, it waits for
// the window that starves it to turn over, exactly as for a full one.
//
// ONE RULE for the two readers that ask it: the budget stage that parks a
// seat's mail ([Engine.budgetPark]), and the idle sweep asking whether an ask
// waiting in that mail is still owed its answer ([colleagueOwed]). Three-valued
// as both its halves are.
//
// THE FLOOR IS THE CALLER'S: the static one for a delivery no launch has been
// refused for yet, and the one a launch was refused below where one was
// ([launchFloor]). A turn's first launch needs its brief beside the static
// floor, and one that continues its conversation's session that session's
// context beside its brief, and a window with room for the static floor alone
// holds either back as surely as a full one; one whose fresh brief's floor is
// held is refused below the static floor itself, or below the room its own
// turn's spend left under it, and parks on that.
func (m *meter) parks(ctx context.Context, floor launchFloor) (r refusal, refusing, starved bool, err error) {
	r, refusing, err = m.refusing(ctx)
	if err == nil && !refusing {
		r, starved, err = m.starved(ctx, floor.tokens())
	}
	return r, refusing, starved, err
}

// budgetHold is what a seat's budget holds a resumed turn back for: a capped
// window refusing outright, or one leaving less room than an executor launch
// needs ([meter.starved]) — the two conditions the budget stage parks a seat's
// mail on — as a refusal that lifts when that window turns over. False where
// neither holds, and where the counters cannot be read, for the reason
// [Engine.budgetPark] gives. refused is the budget refusal the resume met, nil
// for a gate asked before any launch ([Engine.seatCannotRun]).
//
// FOR A RESUME THAT WILL LAUNCH, which has no mail to park, and so waits for
// the window's end rather than parks; a delivery that arrives meanwhile takes
// the park itself. The turn gate asks it of every attempt at a recorded
// answer's resume ([Engine.seatCannotRun]) — an answer always relaunches its
// executor, so a starved window refuses it as surely as a full one — and a
// collected job's resume that its review or relaunch was refused for is held
// on it ([sandbox.ErrResumeHeld]). A completion's gate asks it nothing: the
// job is collected and charged first.
//
// AGAINST THE FLOOR THE LAUNCH IT HOLDS BACK WAS REFUSED BELOW ([floorOf]): a
// relaunch's brief beside the static floor, the context of the turn's session
// it continues beside its brief, or, where its fresh brief's floor is held,
// the static floor or the room below it; the static floor where no launch was
// refused. Held to the static floor alone, a resume refused below a larger
// floor was found not starved, released, and refused at its launch again.
//
// AND NAMING THE WINDOW IT WAITS ON where a launch was refused
// ([sandbox.Refusal.Window]), which the release counts onto the job's record
// for the turn's next relaunch, held in a later window to no more than the
// floor a launch with no brief needs, as a dispatch's next launch is through
// its park ([floorWaits]).
func (e *Engine) budgetHold(ctx context.Context, c *Company, handle string, refused error) (sandbox.Refusal, bool) {
	m := e.meterFor(c, handle)
	if m == nil || !m.basis.capped() {
		return sandbox.Refusal{}, false
	}
	floor := floorOf(c, refused)
	r, refusing, err := m.refusing(ctx)
	if err != nil {
		return sandbox.Refusal{}, false
	}
	if refusing {
		return sandbox.Refusal{Condition: waitBudget, Reason: refusingReason(r), Until: r.Window.End,
			Window: launchRefusedIn(refused, r)}, true
	}
	r, starved, err := m.starved(ctx, floor.tokens())
	if err != nil || !starved {
		return sandbox.Refusal{}, false
	}
	return sandbox.Refusal{Condition: waitBudget, Reason: starvedReason(r, floor),
		Until: r.Window.End, Window: launchRefusedIn(refused, r)}, true
}

// refusingReason is the reason a seat waits on a window r with no room left:
// a parked delivery's deferral and a held resume's refusal alike, so the two
// read the same in a log line and on a dashboard.
func refusingReason(r refusal) string {
	return fmt.Sprintf("budget: %s window %s resets %s",
		r.Window.Period, r.Window.Label, rfc3339(r.Window.End))
}

// starvedReason is the reason a seat waits on a window r whose room is below
// floor, the tokens an executor launch needs ([launchFloor]); see
// [refusingReason]. A floor made of a continued session's context, or of a
// brief beside the static floor, says so in the figures it is made of, and a
// held one says why and where it is held: past the most room the seat's and
// the company's capped windows can leave the launch, and what that room is made
// of, or a floor held after its launch was refused below its floor in a window
// that has turned over since ([launchFloor.heldFor]).
func starvedReason(r refusal, floor launchFloor) string {
	need := fmt.Sprintf("the %d an executor launch needs (turn_engine.executor_min_budget_tokens)",
		floor.tokens())
	switch {
	case floor.context > 0:
		need = fmt.Sprintf("the %d an executor launch that continues its session needs (its last "+
			"request carried %d tokens and its brief about %d; turn_engine.executor_min_budget_tokens "+
			"is %d)", floor.tokens(), floor.context, floor.brief, floor.static)
	case floor.held():
		need = fmt.Sprintf("the %d an executor launch needs (its brief about %d beside "+
			"turn_engine.executor_min_budget_tokens, %d, is %s, so it is held at %s)", floor.tokens(),
			floor.brief, floor.static, floor.heldFor(), floor.heldAt())
	case floor.brief > 0:
		need = fmt.Sprintf("the %d an executor launch needs (its brief about %d beside "+
			"turn_engine.executor_min_budget_tokens, %d)", floor.tokens(), floor.brief, floor.static)
	}
	return fmt.Sprintf("budget: %s window %s leaves %d tokens, below %s; resets %s",
		r.Window.Period, r.Window.Label, max(r.Limit-r.Used, 0), need, rfc3339(r.Window.End))
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

// forgetBudgetPark drops a released seat's park without resuming anything
// ([seatParks.forget]), and what its parks recorded of a launch's floor for its
// next launch here ([floorWaits]): the seat's next holder decides its own.
//
// THE RECORD FIRST, before the park's hold is lifted: a lifted hold may hand
// the seat's mail to a handler at once (the in-memory queue drains into it
// synchronously), and a launch there would take the record the release is
// dropping.
func (e *Engine) forgetBudgetPark(ctx context.Context, seat uuid.UUID) {
	e.floorWaits.forget(seat)
	e.budgetParks.forget(ctx, e, budgetParkSpec, seat)
}

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

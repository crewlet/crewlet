package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// THE BUDGET PARK: A SEAT OUT OF ROOM WAITS FOR ITS WINDOW, NOT BURNS ITS MAIL.
// A token ceiling is per calendar window (ADR-0019), so a refusal ends at the
// window's turnover or an apply raising the ceiling; NAKing into a spent window
// dead-lettered healthy mail. So BEFORE A DELIVERY IS CLAIMED the dispatcher
// asks whether a capped window is refusing ([meter.refusing]), and if so PARKS
// the seat: a pause hold under [pauseReasonBudget] on its inbox, the delivery
// DEFERRED unacked with a reason naming the window, an alarm for the window's
// end, and the refusal recorded on the shared counter ([meter.turnAway]).
// Released at the reset or by an apply changing what the park was decided under
// (a ceiling, or the company's clock); a turn refused MID-FLIGHT after acting
// outside the engine is recorded and acked ([Dispatcher.abandon]).

// pauseReasonBudget is the hold name the budget park takes on a seat's inbox,
// KEYED ON THE SEAT'S ID like every mailbox hold (the agent id from the handle,
// ADR-0013). A STABLE KEY, like [pauseReasonNoTurnEngine] beside it: see
// [parkSpec.hold].
//
// TWO STOPS, EACH WITH ITS OWN OWNER. The quiesce a deferral makes is the seat
// host's ([Dispatcher.NoteDeferred]): the next lease renew resumes the
// attachment. The HOLD is the park's and keeps that attachment from being handed
// anything. Releasing lifts the hold and never the quiesce: an Unquiesce from
// here could resume a consumer the seat host stopped because it cannot prove it
// owns the seat. The hold is taken inside the handler, before the deferral
// returns, so an apply's release cannot land between them and leave the seat deaf.
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
// one, or the one a launch was refused below) is the refusal's, read again on
// the delivery the park hands back; no reader asks it of the park, since an ask
// behind the park is owed as long as it holds, which its holder answers
// (parkheld.go).
//
// WHO HAS TO AGREE ON IT: this node alone (a hold in its queue client, an
// alarm in its memory), derived from the shared counters on every delivery, so
// a restart or a seat moving to a peer simply asks them again.
type budgetParks = seatParks[budgetBasis]

// floorWaits is every delivery this node parked its seat for on a launch's
// refusal below its floor, by seat id then event id, with the window the park
// waited on ([Engine.budgetPark]). The delivery's next launch reads it
// ([agentLauncher.continuedFloor]) to know it waited a turnover, so a later
// window holds it to no more than a launch with no brief needs.
//
// BOUNDED: a floor above the static one, waited for without limit, held a
// delivery for ever (a brief's or session's floor can exceed what a busy window
// leaves each turnover). It waits one turnover, and no more.
// WHO HAS TO AGREE ON IT: this node alone, as the park does. A seat that moves
// leaves it behind; its next holder may wait one window more, still bounded.
type floorWaits struct {
	mu    sync.Mutex
	seats map[uuid.UUID]map[uuid.UUID]sandbox.BudgetWindow
}

// parked records that the launch of seat's delivery whose events' ids are
// delivery was refused below its floor and the seat's park waits on window w,
// each id replacing what an earlier park recorded for it.
//
// KEYED BY THE DELIVERY, NEVER BY THE SEAT ALONE. A release hands mail back in
// the seat's order, and a person's message rides the priority lane
// (queue.Prioritize), so its turn runs before the delivery the park deferred.
// Kept per seat, the window went to the person's launch, which never waited,
// and the delivery that did was parked again in every window. The key is each
// event id ([deliveryOf]), never what a redelivery renews (attempt, run id).
func (f *floorWaits) parked(seat uuid.UUID, delivery []uuid.UUID, w sandbox.BudgetWindow) {
	if len(delivery) == 0 {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.seats == nil {
		f.seats = map[uuid.UUID]map[uuid.UUID]sandbox.BudgetWindow{}
	}
	waits := f.seats[seat]
	if waits == nil {
		waits = map[uuid.UUID]sandbox.BudgetWindow{}
		f.seats[seat] = waits
	}
	for _, id := range delivery {
		waits[id] = w
	}
}

// waited is the window seat's parks recorded for each of the events whose ids
// are delivery, in their order, none where none is recorded. A delivery a
// redelivery merged newer mail into can carry more than one: each of its
// events that waited keeps the window its own park recorded.
//
// READ, NEVER TAKEN, by the launch: taken, a try that went no further (refused
// by every chain member) would spend the wait and be held to the floor again.
// Dropped by [floorWaits.gone] and [floorWaits.forget].
func (f *floorWaits) waited(seat uuid.UUID, delivery []uuid.UUID) []sandbox.BudgetWindow {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []sandbox.BudgetWindow
	for _, id := range delivery {
		if w, ok := f.seats[seat][id]; ok {
			out = append(out, w)
		}
	}
	return out
}

// gone drops what seat's parks recorded for the events whose ids are ids, a
// delivery that will not come back to the seat ([Dispatcher.Gone]).
func (f *floorWaits) gone(seat uuid.UUID, ids []uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	waits := f.seats[seat]
	for _, id := range ids {
		delete(waits, id)
	}
	if len(waits) == 0 {
		delete(f.seats, seat)
	}
}

// forget drops what seat's parks recorded, for a seat this node lets go of.
func (f *floorWaits) forget(seat uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.seats, seat)
}

// deliveryOf is the ids a delivery's events are known by in their stream: each
// event's own id, which every redelivery of it and every copy a requeue
// publishes carries, and which the inbox's dedupe, the completion ledger and
// the queue's headroom all name the event by. Never what a redelivery renews.
func deliveryOf(evs []*events.Event) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(evs))
	for _, ev := range evs {
		if ev != nil {
			out = append(out, ev.ID)
		}
	}
	return out
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

// budgetPark is the dispatcher's budget stage: it parks handle's seat when a
// capped window is refusing or leaves less room than the delivery's launch
// needs, reporting the deferral reason. refused is the refusal the turn met
// (nil before any turn ran). The floor waited for is the one that launch was
// refused below ([floorOf]), else the static floor. Where a launch was refused
// the window waited on is recorded for that delivery alone ([floorWaits]).
//
// It does NOT decide an unknown: an unreadable counter lets the delivery
// through, logged, since the turn's meter fails closed. The error it returns
// is a hold that could not be taken, which the dispatcher DEFERS
// ([Dispatcher.parkOnBudget]), keeping the delivery's place in the inbox.
func (e *Engine) budgetPark(ctx context.Context, handle string, delivery []*events.Event,
	refused error) (string, bool, error) {

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
	// THE WINDOW A LAUNCH WAS REFUSED BELOW ITS FLOOR IN, recorded for the
	// delivery whose launch it was, with the park and before the seat is
	// asked for below, for the race that ask closes for the park: a release
	// that finds the seat gone forgets both.
	if w := launchRefusedIn(refused, r); w.Valid() {
		e.floorWaits.parked(seat, deliveryOf(delivery), w)
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

// parks reports the window that parks a seat's mail, and which condition holds
// it: a capped window refusing outright, or, where none is, one leaving less
// room than floor, the room the delivery's launch needs ([meter.starved]).
//
// A WINDOW TOO SHORT FOR A LAUNCH parks too: a seat with room left but less
// than the executor floor refuses every turn at its launch ([executorHeadroom]),
// and an unparked seat takes each redelivery into it until dead-lettered.
//
// ONE RULE for its two readers: the budget stage ([Engine.budgetPark]) and the
// idle sweep asking whether an ask in that mail is still owed ([colleagueOwed]).
// Three-valued as both halves are. THE FLOOR IS THE CALLER'S: the static one,
// or the one a launch was refused below ([launchFloor]).
func (m *meter) parks(ctx context.Context, floor launchFloor) (r refusal, refusing, starved bool, err error) {
	r, refusing, err = m.refusing(ctx)
	if err == nil && !refusing {
		r, starved, err = m.starved(ctx, floor.tokens())
	}
	return r, refusing, starved, err
}

// budgetHold is what a seat's budget holds a resumed turn back for: a capped
// window refusing, or one leaving less room than an executor launch needs
// ([meter.starved]), as a refusal lifting when it turns over. False where
// neither holds or the counters cannot be read (see [Engine.budgetPark]).
// refused is the refusal the resume met, nil before any launch. A resume has
// no mail to park, so it waits for the window's end; the turn gate asks it of
// every resume of a recorded answer ([Engine.seatCannotRun]), and a collected
// job's resume is held on it ([sandbox.ErrResumeHeld]).
// AGAINST THE FLOOR THE LAUNCH WAS REFUSED BELOW ([floorOf]): held to the
// static floor alone, a resume refused below a larger one was found not
// starved, released and refused again. It names the window waited on
// ([sandbox.Refusal.Window]), as [floorWaits] does for a parked delivery.
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
		need = fmt.Sprintf("the %d an executor launch that continues its session needs (its session "+
			"sends %d tokens before its brief, which is about %d; turn_engine.executor_min_budget_tokens "+
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
// deliveries' next launches here ([floorWaits]): the record is the park's, and
// the seat's next holder decides its own.
func (e *Engine) forgetBudgetPark(seat uuid.UUID) {
	e.floorWaits.forget(seat)
	e.budgetParks.forget(seat)
}

// budgetDeliveryGone drops what handle's seat's parks recorded of a launch
// refused below its floor for gone, the events of a delivery that will not come
// back to the seat ([Dispatcher.Gone]): nothing would read it again, and kept,
// it would grow the seat's record for as long as this node holds the seat.
// Another delivery's record, its seat still parked or its next try still to
// come, is kept.
func (e *Engine) budgetDeliveryGone(_ context.Context, handle string, gone []*events.Event) {
	seat, err := e.seatID(handle)
	if err != nil {
		// A SEAT THE RUNNING COMPANY NO LONGER HOLDS is let go of, and its
		// release forgets every record of it ([Engine.forgetBudgetPark]).
		return
	}
	e.floorWaits.gone(seat, deliveryOf(gone))
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

package engine

import (
	"context"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/queue/topics"
)

// A SEAT PARKED UNTIL AN INSTANT: the machinery the budget park
// (budgetpark.go) and the provider park (providerpark.go) share.
//
// Both answer one condition the same way: a seat that cannot run a turn until
// a known instant (a token window turning over, a model's usage limit lifting)
// is not handed work it would only fail, and is not NAKed into the same
// refusal on a backoff until the broker dead-letters its mail. Instead:
//
//   - a pause hold is taken on its inbox, under the park's own reason, so
//     nothing more is delivered to it, and the delivery that found the
//     condition is DEFERRED, which keeps its place at the head of the inbox;
//   - the hold is lifted when the condition ends, and the mail is delivered
//     again in order, ONCE: the hold keeps the resumed attachment from being
//     handed anything until then;
//   - and the record of what the park was decided under (its basis) lets a
//     change of the company release it at once.
//
// WHAT ENDS THE CONDITION is the one place the two differ in kind. A window's
// end IS the end of the budget's condition, so each budget park arms its own
// alarm for it ([seatParks.take] with an instant). A model's limit is the
// limited entries', not the seat's, and its end is learned by asking them: one
// probe per limited chain, whatever number of seats are parked on it, which
// releases them all (providerpark.go). Such a park is taken with no instant
// and no alarm of its own. The held mail is never the question either way: a
// delivery handed back by a park spends one of its deliveries.
//
// What else differs between the two is the condition, the basis and what a
// park says about itself, which each file keeps. What is the same (the record,
// the alarm, the race between a release and a re-park, a seat given up, a
// node stopping) lives here once, so the two cannot drift apart on the parts
// that have to be right for both.
//
// HOLDS ARE KEYED BY REASON, so two parks on one inbox (a seat whose window is
// spent AND whose model is limited) are two holds, and neither release lifts
// the other's. WHO HAS TO AGREE ON A PARK: this node alone. It is a hold in
// this process's queue client and an alarm in its memory, decided again from
// shared facts on the first delivery after a restart or a move.

// alarm is an armed release: stopped when the park is released some other way.
type alarm interface{ Stop() bool }

// parkSpec is what one kind of park is called: the hold it takes on an inbox,
// and the log lines its release writes.
type parkSpec struct {
	// hold is the pause reason, a STABLE KEY: holds are keyed by reason so
	// two subsystems gating one inbox cannot release each other's, and a key
	// derived from prose would strand every seat parked under the old
	// wording.
	hold string
	// released and notReleased are the log events of a release that lifted
	// the hold and of one the queue refused.
	released, notReleased string
}

// seatParks is every seat inbox this node holds under one kind of park, by
// the seat's id, with what each park was decided under (B).
type seatParks[B any] struct {
	mu    sync.Mutex
	seats map[uuid.UUID]*seatParking[B]

	// stopped refuses new alarms once the engine is tearing down: an alarm
	// armed after [seatParks.stop] would fire into a closed queue.
	stopped bool

	// after is the alarm, injectable for tests; nil is time.AfterFunc.
	//
	// THE ALARM AND NOT THE CLOCK. What it waits for is measured against
	// the engine's clock ([Engine.now]), the one the condition was decided
	// on: a park with a clock of its own could arm its alarm for an instant
	// the decision beside it was not made in.
	after func(d time.Duration, f func()) alarm
}

// seatParking is one parked seat: what the park was decided under, when its
// alarm releases it (zero for a park something else releases), and the seat's
// handle, for the log lines.
type seatParking[B any] struct {
	handle string
	basis  B
	until  time.Time
	alarm  alarm
}

func (p *seatParks[B]) arm(d time.Duration, f func()) alarm {
	if p.after != nil {
		return p.after(d, f)
	}
	return time.AfterFunc(d, f)
}

// take holds seat's inbox under spec and records the park until the instant
// given, when its alarm releases it for why. A zero instant arms nothing: the
// park is released by whoever learns its condition ended.
//
// THE HOLD IS TAKEN AND RECORDED UNDER THE LOCK, which is what closes the race
// with a release: a company's apply installs its epoch before it reconciles
// the parks, so either the reconcile sees this park or the caller's read of
// the basis after this returns sees the epoch that makes it unnecessary. The
// queue call does not drain, so holding the lock across it cannot call back
// into here.
func (p *seatParks[B]) take(ctx context.Context, e *Engine, spec parkSpec, seat uuid.UUID,
	handle string, basis B, until time.Time, why string) error {

	subject, group := topics.AgentInbox(seat), topics.AgentInboxGroup(seat)
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := e.backends.Queue.PauseTopic(ctx, subject, group, spec.hold); err != nil {
		return err
	}
	p.recordLocked(ctx, e, spec, seat, &seatParking[B]{handle: handle, basis: basis, until: until}, why)
	return nil
}

// recordLocked records a park and arms its alarm. The caller holds p.mu.
//
// A seat parked again while already parked (a second partition of the drain
// that parked it) replaces the record, and the earlier alarm is stopped: the
// ALARM IS THE RECORD'S, never the seat's, so an alarm that fires after its
// record was replaced releases nothing (see [seatParks.releaseParking]).
func (p *seatParks[B]) recordLocked(ctx context.Context, e *Engine, spec parkSpec,
	seat uuid.UUID, parking *seatParking[B], why string) {

	if p.seats == nil {
		p.seats = map[uuid.UUID]*seatParking[B]{}
	}
	if old := p.seats[seat]; old != nil {
		old.disarm()
	}
	p.seats[seat] = parking
	p.armLocked(ctx, e, spec, seat, parking, why)
}

// armLocked arms parking's alarm for its instant. The caller holds p.mu.
//
// The alarm fires on context.WithoutCancel(ctx): it fires long after the
// delivery that parked the seat has settled and its context has ended, and a
// release that inherited that cancellation would lift nothing, while the
// delivery's trace, which the release belongs to, is still worth carrying.
func (p *seatParks[B]) armLocked(ctx context.Context, e *Engine, spec parkSpec,
	seat uuid.UUID, parking *seatParking[B], why string) {

	if p.stopped || parking.until.IsZero() {
		return
	}
	// Never a negative wait: the instant was decided on the engine's
	// clock, but the alarm is measured on the monotonic one, and a wall
	// clock stepped forward between the two reads must not arm an alarm
	// that has already passed as though it had not.
	wait := max(parking.until.Sub(e.now()), 0)
	released := context.WithoutCancel(ctx)
	parking.alarm = p.arm(wait, func() {
		p.releaseParking(released, e, spec, seat, parking, why)
	})
}

// disarm stops parking's alarm. The caller holds the parks' lock.
func (parking *seatParking[B]) disarm() {
	if parking.alarm != nil {
		parking.alarm.Stop()
		parking.alarm = nil
	}
}

// parking is seat's current park, or nil.
func (p *seatParks[B]) parking(seat uuid.UUID) *seatParking[B] {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.seats[seat]
}

// release lifts the hold from one seat's inbox, whatever record holds it,
// reporting whether it did.
func (p *seatParks[B]) release(ctx context.Context, e *Engine, spec parkSpec, seat uuid.UUID,
	why string) bool {

	if parking := p.parking(seat); parking != nil {
		return p.releaseParking(ctx, e, spec, seat, parking, why)
	}
	return false
}

// releaseParking lifts the hold parking took, if parking is still the seat's
// record, reporting whether it did.
//
// OUTSIDE THE LOCK for the queue call, because the in-memory twin's resume
// drains synchronously into the seat's handler, and a delivery that finds the
// condition still there comes straight back to the park.
func (p *seatParks[B]) releaseParking(ctx context.Context, e *Engine, spec parkSpec,
	seat uuid.UUID, parking *seatParking[B], why string) bool {

	p.mu.Lock()
	if p.seats[seat] != parking {
		// Replaced by a later park, or already released: this alarm, or
		// this caller, is describing a hold that is somebody else's now.
		p.mu.Unlock()
		return false
	}
	delete(p.seats, seat)
	parking.disarm()
	p.mu.Unlock()

	subject, group := topics.AgentInbox(seat), topics.AgentInboxGroup(seat)
	if err := e.backends.Queue.ResumeTopic(ctx, subject, group, spec.hold); err != nil {
		// Kept on the record with no alarm, as the no-model release keeps
		// its own: a resume fails only on a client that has stopped, which
		// is a process on its way out, and the record is what
		// [seatParks.stop] and the seat's release still clear.
		p.mu.Lock()
		if _, again := p.seats[seat]; !again {
			if p.seats == nil {
				p.seats = map[uuid.UUID]*seatParking[B]{}
			}
			p.seats[seat] = &seatParking[B]{handle: parking.handle, basis: parking.basis,
				until: parking.until}
		}
		p.mu.Unlock()
		log.WarnContext(ctx, spec.notReleased, "seat", parking.handle,
			"agent_id", seat.String(), "error", err,
			"detail", "this seat's held mail stays on its inbox until the queue "+
				"client is replaced")
		return false
	}
	log.InfoContext(ctx, spec.released, "seat", parking.handle,
		"agent_id", seat.String(), "reason", why)
	return true
}

// changed is every parked seat whose basis keep says no longer holds, in a
// stable order: what a company's apply releases.
func (p *seatParks[B]) changed(keep func(seat uuid.UUID, basis B) bool) []uuid.UUID {
	p.mu.Lock()
	var out []uuid.UUID
	for seat, parking := range p.seats {
		if !keep(seat, parking.basis) {
			out = append(out, seat)
		}
	}
	p.mu.Unlock()
	return sortedSeats(slices.Values(out))
}

// forget drops a released seat's park without resuming anything, reporting
// whether there was one.
//
// The seat's release DETACHED its inbox, and a detach releases every hold the
// attachment carried, so there is nothing to lift, only an alarm that would
// otherwise fire into a seat this node no longer holds.
func (p *seatParks[B]) forget(seat uuid.UUID) (*seatParking[B], bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	parking := p.seats[seat]
	if parking == nil {
		return nil, false
	}
	parking.disarm()
	delete(p.seats, seat)
	return parking, true
}

// stop disarms every alarm, for a node shutting down.
//
// The holds are left to the queue client, which the node's stop detaches: a
// resume here would only hand a stopping node mail it is about to give back.
func (p *seatParks[B]) stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopped = true
	for _, seat := range sortedSeats(maps.Keys(p.seats)) {
		p.seats[seat].disarm()
	}
}

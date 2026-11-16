package engine

import (
	"context"
	"errors"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/queue"
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
// A PARK OUTLIVES NO SEAT. The seat's release detaches its inbox, which drops
// every hold on it and ends the attachment each was taken for, and then forgets
// its parks; it waits for none of the turns that can park the seat, so a park
// can be asked for in between, or after. Its hold names the attachment the seat
// held as it was asked (seatinbox.go), so one asked after the detach is refused
// and records no park, the forget drops the record of one it finds
// ([seatParks.forget]), and a park that finds its seat gone once it is taken
// undoes itself.
//
// A RELEASE AND A RE-PARK of one seat agree through the record: a park takes
// its hold and records it as one step under the parks' lock, and a release
// drops the record under that lock and lifts the hold outside it, then reads
// the record again and takes back the hold of a park recorded meanwhile, which
// its lift took from it ([seatParks.reholdParked]). What a release says of the
// seat beyond its hold (a provider park's probe and its notice) it says as it
// drops the record, under the same lock, so a park taken after the drop says
// its own after it ([Engine.releaseProviderParking]).
//
// HOLDS ARE KEYED BY REASON, so two parks on one inbox (a seat whose window is
// spent AND whose model is limited) are two holds, and neither release lifts
// the other's. WHO HAS TO AGREE ON A PARK: this node alone. It is a hold in
// this process's queue client and an alarm in its memory, decided again from
// shared facts on the first delivery after a restart or a move.

// alarm is an armed release: stopped when the park is released some other way.
type alarm interface{ Stop() bool }

// seatReleasedHere is why a park ends with its seat's release on this node:
// what its release logs, and what `seat_provider_released` says of a provider
// park.
const seatReleasedHere = "the seat was released by this node"

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
// alarm releases it (zero for a park something else releases), the attachment
// of the seat's inbox its hold was taken for, which its release names, and the
// seat's handle, for the log lines.
type seatParking[B any] struct {
	handle string
	basis  B
	until  time.Time
	inbox  queue.Attachment
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
	// FOR THE ATTACHMENT THE SEAT HOLDS NOW: one its release has ended is
	// refused, so no park is recorded for a seat already let go
	// (seatinbox.go).
	at := e.inboxOf(seat)
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := e.backends.Queue.PauseTopic(ctx, subject, group, at, spec.hold); err != nil {
		return err
	}
	p.recordLocked(ctx, e, spec, seat,
		&seatParking[B]{handle: handle, basis: basis, until: until, inbox: at}, why)
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
		p.releaseParking(released, e, spec, seat, parking, why, nil)
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
		return p.releaseParking(ctx, e, spec, seat, parking, why, nil)
	}
	return false
}

// releaseParking lifts the hold parking took, if parking is still the seat's
// record, reporting whether it did.
//
// dropped, where the kind of park has more to say of the seat than its hold,
// says it as the record is dropped, under the parks' lock: a park of the seat
// is taken under that lock, so one taken after the drop says what it says of
// the seat after this, and one taken before it replaced the record, which this
// then finds and leaves alone ([Engine.releaseProviderParking]). It makes no
// queue call and takes no lock a park's take holds across its own.
//
// OUTSIDE THE LOCK for the queue call, because the in-memory twin's resume
// drains synchronously into the seat's handler, and a delivery that finds the
// condition still there comes straight back to the park. A park taken while
// that call is in flight would lose its hold to it, so the release reads the
// record again once the call returns ([seatParks.reholdParked]).
func (p *seatParks[B]) releaseParking(ctx context.Context, e *Engine, spec parkSpec,
	seat uuid.UUID, parking *seatParking[B], why string, dropped func()) bool {

	p.mu.Lock()
	if p.seats[seat] != parking {
		// Replaced by a later park, or already released: this alarm, or
		// this caller, is describing a hold that is somebody else's now.
		p.mu.Unlock()
		return false
	}
	delete(p.seats, seat)
	parking.disarm()
	if dropped != nil {
		dropped()
	}
	p.mu.Unlock()

	subject, group := topics.AgentInbox(seat), topics.AgentInboxGroup(seat)
	if err := e.backends.Queue.ResumeTopic(ctx, subject, group, parking.inbox, spec.hold); err != nil {
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
				until: parking.until, inbox: parking.inbox}
		}
		p.mu.Unlock()
		log.WarnContext(ctx, spec.notReleased, "seat", parking.handle,
			"agent_id", seat.String(), "error", err,
			"detail", "this seat's held mail stays on its inbox until the queue "+
				"client is replaced")
		return false
	}
	heldAgain := p.reholdParked(ctx, e, spec, seat)
	// SAID WITH THE RELEASE, because the park that holds the seat again can
	// have logged its own line before this one: without it the seat's last
	// line read released while its inbox was held.
	detail := "this park's hold is lifted from the seat's inbox"
	if heldAgain {
		detail = "a park taken while this release was in flight holds the seat again, " +
			"so its inbox stays held until that park is released"
	}
	log.InfoContext(ctx, spec.released, "seat", parking.handle,
		"agent_id", seat.String(), "reason", why, "held_again", heldAgain, "detail", detail)
	return true
}

// reholdParked takes back the hold of a park recorded while a release of the
// seat's earlier park was in flight, which that release's resume has just
// lifted, reporting whether a park stands with the seat's inbox held under it
// once it returns, which the release's line says.
//
// THE WINDOW is the release's own: it drops its record under the lock and
// resumes outside it, and a park taken between the two (a turn on another
// partition that met the condition) re-holds the same attachment, which a hold
// one per reason leaves unchanged on the queue, and records itself. The resume
// then lifts the hold that park owns: the seat stays recorded parked, its
// alarm armed, with its inbox not held, until its next delivery meets the
// condition and parks it again, at the cost of one of that delivery's
// attempts. Read again under the lock once the resume has returned, a park in
// force takes its hold back, for the attachment its record names, which is the
// one the seat holds now or one its release has ended, which the queue refuses
// and the release's forget drops. A park taken once the queue has lifted the
// hold (the resumed mail meeting the condition again, which the in-memory twin
// hands over inside the resume) holds by itself, as does one taken after this
// read, and a release after it reads again after its own resume.
func (p *seatParks[B]) reholdParked(ctx context.Context, e *Engine, spec parkSpec, seat uuid.UUID) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	parking := p.seats[seat]
	if parking == nil {
		return false
	}
	subject, group := topics.AgentInbox(seat), topics.AgentInboxGroup(seat)
	err := e.backends.Queue.PauseTopic(ctx, subject, group, parking.inbox, spec.hold)
	if err == nil {
		return true
	}
	if errors.Is(err, queue.ErrAttachmentGone) {
		return false
	}
	// A pause fails only on a client that has stopped, a process on its way
	// out; the record stays for the release that lifts it.
	log.WarnContext(ctx, "seat_park_hold_not_retaken", "seat", parking.handle,
		"agent_id", seat.String(), "hold", spec.hold, "error", err,
		"detail", "this seat's park stands but its inbox is not held; a delivery "+
			"that meets the condition parks it again")
	return false
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
// whether there was one: its alarm, which would otherwise fire into a seat
// this node no longer holds.
//
// NOT ITS HOLD, which there is nothing left of to lift. The seat's release
// DETACHED its inbox before this, which dropped every hold on it and ended the
// attachment each was taken for, and a park asked between the detach and here
// named that ended attachment, which the queue refused, so no park was
// recorded for it. A lift from here would name the same ended attachment and
// lift nothing; a park taken after this asks for the seat itself once it is
// taken, and undoes itself where the seat is gone ([Engine.budgetPark],
// [Engine.providerPark]).
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

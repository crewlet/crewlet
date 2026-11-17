package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/inbox"
	"github.com/crewlet/crewlet/internal/origin"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// A COMPANY WITH NO MODELS RUNS, AND ITS SEATS' WORK WAITS FOR ONE.
//
// An empty providers.llm is a valid company: an org chart written before its
// credentials exist, which is also what a company created from the dashboard
// is until somebody adds a provider. It is applied like any other. The epoch
// carries no model registry ([Company.Models] is nil), its seats are placed,
// and their mailboxes attach and keep what arrives. What no seat can do is
// think, so the inbox screening parks every delivery ([inbox.Screen], stage
// 4): the seat's inbox is paused first, then the partition is requeued and
// acked, and the work waits on the broker.
//
// PARKED RATHER THAN FAILED. A turn that could not build its runner proves
// nothing reached outside the engine, so the dispatcher NAKs it, and a
// condition that holds until an operator edits the configuration would spend
// the broker's whole delivery budget on each event and then lose it. The one
// failure an operator is told about is the one that says what to change: each
// node logs `company_has_no_models` when it installs such an epoch, and
// `seat_inbox_paused` for every seat whose work it holds, both naming
// providers.llm.
//
// SCHEDULED WORK IS NOT HELD, because it is not sent: the scheduler stays
// disarmed while the company has no model ([Engine.schedulerRuns]). A fire
// parked behind the pause is a standup that runs days late, and a week of
// them would all run the moment a provider arrived.
//
// THE PAUSE IS RELEASED WHEN THE CONDITION CLEARS, and that half is what used
// to be missing: nothing lifted the hold, so a seat paused here stayed deaf
// until the process restarted, however long ago its provider had been added.
// The condition clears in exactly one place, an apply that installs an epoch
// with a model, and that apply releases every inbox this client paused. Boot
// needs no release: holds live in the queue client, and a new process has
// none.

// pauseReasonNoTurnEngine is the hold name the no-model park takes on a seat's
// inbox.
//
// A STABLE KEY, not the screening's prose. Pause holds are keyed by reason so
// two subsystems gating one inbox cannot release each other's hold, which
// means the pause and the release must spell it identically. It is one of
// several a seat's inbox can be held under, each a key of its own: a person's
// pause ([inbox.HoldSeatPaused]), the sandbox coordinator's
// ([inbox.HoldSandbox]), the budget park's ([pauseReasonBudget]) and the
// provider park's ([pauseReasonProvider]). Deriving it from the
// human-readable reason would make an edit to a log message silently strand
// every seat that was parked under the old wording.
const pauseReasonNoTurnEngine = string(inbox.HoldNoTurnEngine)

// modelHolds records the seat inboxes this client paused for want of a model.
//
// RECORDED, because which inboxes this client paused is a fact only the pause
// knows: the queue contract lists no holds, and a release that walked the new
// epoch's seats instead would resume every seat on every apply to lift holds
// that almost never exist, and would still miss one the new revision no
// longer names. BY THE SEAT'S ID, the agent id derived from its handle
// (ADR-0013), which is what the paused topic is named by; the handle rides
// along for the log lines. WITH THE ATTACHMENT the hold was taken for, which
// its lift names (the package doc's "Every hold is taken for one attachment of
// the inbox").
type modelHolds struct {
	mu    sync.Mutex
	seats map[uuid.UUID]modelHold
}

// modelHold is one inbox's no-model pause: the seat's handle, for the log
// lines, and the attachment the hold was taken for.
type modelHold struct {
	handle string
	inbox  queue.Attachment
}

// record notes that this seat's inbox holds the no-model pause.
func (h *modelHolds) record(id uuid.UUID, hold modelHold) {
	if h.seats == nil {
		h.seats = map[uuid.UUID]modelHold{}
	}
	h.seats[id] = hold
}

// holdInbox takes the hold a screening's pause-and-park names, before the park
// requeues the delivery, so the copies buffer on the queue rather than looping
// straight back. Each hold is recorded by the subsystem that lifts it.
func (e *Engine) holdInbox(ctx context.Context, handle string, hold inbox.Hold, reason string) error {
	switch hold {
	case inbox.HoldNoTurnEngine:
		return e.pause(ctx, handle, reason)
	case inbox.HoldSeatPaused:
		return e.holdPausedInbox(ctx, handle)
	}
	// A hold nothing here would ever lift. Refusing it NAKs the delivery,
	// which is loud and recoverable; taking it would strand the seat.
	return fmt.Errorf("engine: no subsystem lifts a %q hold on seat %q", hold, handle)
}

// pause stops delivery on a seat's inbox before the no-model park, so the
// requeued copies buffer on the queue rather than looping straight back.
//
// THE RACE WITH THE RELEASE is closed by ordering, not by a lock held across
// both. The hold is taken and recorded under the record's lock, and the epoch
// is read after that; an apply installs its epoch before the release snapshots
// the record. So either the release sees this hold, or this read sees the
// epoch that makes it unnecessary and lifts it at once. Holding the lock across
// the queue call instead would deadlock on the in-memory twin, whose resume
// drains synchronously into handlers that come straight back here.
func (e *Engine) pause(ctx context.Context, handle, reason string) error {
	id, err := e.seatID(handle)
	if err != nil {
		return err
	}
	subject, group := topics.AgentInbox(id), topics.AgentInboxGroup(id)
	// FOR THE ATTACHMENT THE SEAT HOLDS NOW, which a release ends: a pause
	// asked after it is refused (seatinbox.go).
	at := e.inboxOf(id)
	e.modelHolds.mu.Lock()
	err = e.backends.Queue.PauseTopic(ctx, subject, group, at, pauseReasonNoTurnEngine)
	if err == nil {
		e.modelHolds.record(id, modelHold{handle: handle, inbox: at})
	}
	e.modelHolds.mu.Unlock()
	if err != nil {
		return err
	}
	log.WarnContext(ctx, "seat_inbox_paused", "handle", handle, "reason", reason,
		"detail", "the company configures no model provider, so this seat's "+
			"work waits on its inbox; it is delivered once a revision adds one "+
			"under providers.llm")
	if c := e.Company(); c != nil && c.Models != nil {
		e.releaseModelHolds(ctx)
	}
	return nil
}

// releaseModelHolds lifts the no-model pause from every inbox this client
// paused, for an epoch that has a model.
//
// The caller has already made that epoch current: a delivery the release lets
// through is screened and run against the company that has a model.
//
// OUTSIDE THE LOCK for each queue call, for the reason [Engine.pause] gives,
// and a pause recorded while that call is in flight would lose its hold to it,
// so each lift reads the record again once its call returns
// ([Engine.reholdModelHold]).
func (e *Engine) releaseModelHolds(ctx context.Context) {
	e.modelHolds.mu.Lock()
	held := maps.Clone(e.modelHolds.seats)
	clear(e.modelHolds.seats)
	e.modelHolds.mu.Unlock()

	// SORTED BY THE ID, which is the key: the order only has to be stable so
	// a test and a log read the same way.
	ids := slices.Collect(maps.Keys(held))
	slices.SortFunc(ids, func(a, b uuid.UUID) int {
		return strings.Compare(a.String(), b.String())
	})
	for _, id := range ids {
		hold := held[id]
		handle := hold.handle
		subject, group := topics.AgentInbox(id), topics.AgentInboxGroup(id)
		if err := e.backends.Queue.ResumeTopic(ctx, subject, group, hold.inbox, pauseReasonNoTurnEngine); err != nil {
			// Kept on the record, so the next release tries it again. A
			// resume fails only on a client that has stopped, which is a
			// process on its way out; forgetting the hold would be the one
			// way to make it permanent.
			e.modelHolds.mu.Lock()
			e.modelHolds.record(id, hold)
			e.modelHolds.mu.Unlock()
			log.WarnContext(ctx, "seat_inbox_not_resumed", "handle", handle,
				"error", err, "detail", "this seat's held work stays on its inbox "+
					"until the next release")
			continue
		}
		heldAgain := e.reholdModelHold(ctx, id)
		// SAID WITH THE LIFT, because the pause that holds the seat again
		// can have logged `seat_inbox_paused` before this line: without it
		// the seat's last line read resumed while its inbox was held.
		detail := "this seat's no-model pause is lifted from its inbox"
		if heldAgain {
			detail = "a no-model pause taken while this lift was in flight holds the seat " +
				"again, so its inbox stays held until a revision with a model releases it"
		}
		log.InfoContext(ctx, "seat_inbox_resumed", "handle", handle,
			"reason", pauseReasonNoTurnEngine, "held_again", heldAgain, "detail", detail)
	}
}

// reholdModelHold takes back the hold of a no-model pause recorded for seat id
// while the release was lifting its earlier one, which that lift has just
// taken from it, reporting whether a pause stands with the seat's inbox held
// under it once it returns, which the release's line says. The release drops
// the record under the lock and resumes outside it; a pause taken between the
// two re-holds the same attachment and records itself, and the resume lifts
// its hold. Read again under the lock, a pause recorded meanwhile takes its
// hold back for the attachment its record names. The package doc's "A lift
// takes back a hold taken in its window" says why.
func (e *Engine) reholdModelHold(ctx context.Context, id uuid.UUID) bool {
	e.modelHolds.mu.Lock()
	defer e.modelHolds.mu.Unlock()
	hold, again := e.modelHolds.seats[id]
	if !again {
		return false
	}
	subject, group := topics.AgentInbox(id), topics.AgentInboxGroup(id)
	err := e.backends.Queue.PauseTopic(ctx, subject, group, hold.inbox, pauseReasonNoTurnEngine)
	if err == nil {
		return true
	}
	if errors.Is(err, queue.ErrAttachmentGone) {
		return false
	}
	// A pause fails only on a client that has stopped, a process on its way
	// out; the record stays for the next release.
	log.WarnContext(ctx, "seat_inbox_not_paused", "handle", hold.handle, "error", err,
		"detail", "this seat's no-model pause stands but its inbox is not held; a "+
			"delivery to it pauses it again")
	return false
}

// forgetModelHold drops the record of seat's no-model pause, for a seat this
// node has released ([Engine.releaseSeat]).
//
// NOT THE HOLD, which the release's detach dropped with the attachment it was
// taken for. Left, the record named that ended attachment: the next release
// lifted nothing on it and logged `seat_inbox_resumed` all the same. The
// release forgets the seat's attachment first ([Engine.detachInbox]), so a
// pause asked after this is refused and records nothing.
func (e *Engine) forgetModelHold(seat uuid.UUID) {
	e.modelHolds.mu.Lock()
	defer e.modelHolds.mu.Unlock()
	delete(e.modelHolds.seats, seat)
}

// seatID resolves a seat handle to the id its mailbox is named by, through
// the company this node is running.
//
// A LOOKUP IS SAFE HERE and is not on the host's release paths, which is why
// [placement.Seat] carries the id instead: every caller of this is inside a
// live dispatch — a park, a pause, a release of one — where the company is by
// construction the one the delivery was screened against. A node with no
// company cannot be dispatching at all, so an absent one is a wiring mistake
// and says so rather than answering with a subject named after nothing.
func (e *Engine) seatID(handle string) (_ uuid.UUID, err error) {
	defer origin.Stamp(&err)
	c := e.Company()
	if c == nil || c.Org == nil {
		return uuid.Nil, fmt.Errorf("engine: this node runs no company, so seat "+
			"%q has no mailbox to address", handle)
	}
	id, ok := c.Org.AgentIDFor(c.Org.AgentSeatByHandle(handle))
	if !ok {
		return uuid.Nil, fmt.Errorf("engine: seat %q is no agent seat in the "+
			"running company, so it has no inbox subject", handle)
	}
	return id, nil
}

package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"iter"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/inbox"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/backoff"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// A PERSON'S PAUSE, CARRIED OUT BY WHICHEVER NODE HOLDS THE SEAT.
//
// A pause is a coordination record (coord.SeatPause), written by the node that
// served the person's request and read by every node. This file is the reading
// half: it holds the inbox (for a seat this node holds, taken and recorded as
// one step under the pauses' lock), answers the screening, skips the schedule,
// and stops the running work when the pause asked to. Keyed on the seat's ID;
// this node's cache is derived from a watch and never written back; and it has
// three states, the third being "not yet known". The package doc's "A
// person's pause, carried out by whichever node holds the seat" holds the
// decisions and why.

// The pause watch's timing.
const (
	// seatPausePrimeBudget bounds how long a booting node waits for the
	// first complete answer before it goes on without one. Its deliveries
	// are deferred, not run, until the answer arrives, so the wait buys a
	// node that serves its first mail without a deferral — and past it the
	// watch keeps trying in the background. Ten seconds is a healthy
	// store's listing many times over and the order of one heartbeat, so a
	// slow store costs a boot that long and no more.
	seatPausePrimeBudget = 10 * time.Second

	// seatPauseRewatchBase and seatPauseRewatchCeiling space the re-watch
	// after a watch ends. The ceiling is how stale a pause may go unseen
	// while the store recovers, so it is the fleet's own freshness
	// cadence ([coord.ReconcileInterval], fifteen seconds): the interval at
	// which every other fleet-wide fact is re-read.
	seatPauseRewatchBase    = 500 * time.Millisecond
	seatPauseRewatchCeiling = coord.ReconcileInterval
)

// seatPauseHold is the hold key the pause takes, spelled once for the queue.
const seatPauseHold = string(inbox.HoldSeatPaused)

// errPausesUnread reports a question asked before the first complete answer.
var errPausesUnread = errors.New("engine: this node has not yet read which seats are paused")

// seatPauses is this node's watched copy of every seat pause in the fleet,
// and the holds this queue client took because of them.
type seatPauses struct {
	mu sync.Mutex

	// known is whether a complete answer has ever arrived.
	known bool

	// pauses is that answer, kept current by the watch, keyed by seat id.
	pauses map[uuid.UUID]coord.SeatPause

	// held is every seat inbox this client holds under [seatPauseHold],
	// by the seat id its mailbox is named after, with the attachment the
	// hold was taken for, which its lift names. RECORDED for the reason
	// [modelHolds] is: which holds this client took is a fact only this
	// process knows, and the queue contract lists none.
	held map[uuid.UUID]queue.Attachment

	cancel context.CancelFunc
	done   chan struct{}
}

// pauseOf reads one seat's pause off this node's copy, reporting whether the
// copy has ever been complete.
func (e *Engine) pauseOf(seat uuid.UUID) (p coord.SeatPause, paused, known bool) {
	e.pauses.mu.Lock()
	defer e.pauses.mu.Unlock()
	p, paused = e.pauses.pauses[seat]
	return p, paused, e.pauses.known
}

// pauseOfHandle is [Engine.pauseOf] for a caller that holds the seat by its
// handle — the dispatcher's screening, which is keyed the way a mailbox
// handler is. A handle the running company resolves to no agent seat is a seat
// nobody can have paused here: answered as not paused, with the copy's own
// knownness, so a delivery for a seat outside the company is decided by the
// ownership check rather than deferred for ever.
func (e *Engine) pauseOfHandle(handle string) (paused, known bool) {
	seat, err := e.seatID(handle)
	if err != nil {
		e.pauses.mu.Lock()
		defer e.pauses.mu.Unlock()
		return false, e.pauses.known
	}
	_, paused, known = e.pauseOf(seat)
	return paused, known
}

// seatPaused is the scheduler's question, by the seat's id and three-valued:
// an error is a node that has not read the pauses yet.
func (e *Engine) seatPaused(seat uuid.UUID) (bool, error) {
	_, paused, known := e.pauseOf(seat)
	if !known {
		return false, errPausesUnread
	}
	return paused, nil
}

// acquireInbox records the attachment a node's acquisition makes of seat's
// inbox ([Engine.attachInbox]) and, for a seat a person paused, takes the pause
// hold for it: the first step of the seat's preparation, run before the node
// attaches the inbox ([node.Config.SeatReady]), so a paused seat is attached
// already held, never after its first delivery can arrive. A seat whose pause
// state is not yet known is held by nothing: the screening defers it.
// RECORDED AND TAKEN AS ONE STEP under the pauses' lock, the attachment too,
// so a resume lands wholly before or wholly after it. A hold that cannot be
// taken REFUSES the seat. The package doc's "A person's pause, carried out by
// whichever node holds the seat" says why.
func (e *Engine) acquireInbox(ctx context.Context, seat uuid.UUID, at queue.Attachment) error {
	e.pauses.mu.Lock()
	defer e.pauses.mu.Unlock()
	e.attachInbox(seat, at)
	if _, paused := e.pauses.pauses[seat]; !paused {
		return nil
	}
	subject, group := topics.AgentInbox(seat), topics.AgentInboxGroup(seat)
	if err := e.backends.Queue.PauseTopic(ctx, subject, group, at, seatPauseHold); err != nil {
		return fmt.Errorf("hold the paused seat's inbox before it is attached: %w", err)
	}
	e.recordHeldLocked(seat, at)
	return nil
}

// holdPausedInbox is the dispatcher's pause-and-park for a paused seat: it
// takes the hold a delivery raced, and records it.
//
// THE RACE WITH THE RESUME is closed by ordering, as the no-model pause closes
// its own: the hold is taken and recorded under the lock, and the cache is
// read again under that same lock. A resume that already landed is seen here,
// and the hold is lifted at once; one that lands after sees the record and
// lifts it. The queue call does not drain, so taking it under the lock cannot
// call back into here.
func (e *Engine) holdPausedInbox(ctx context.Context, handle string) error {
	seat, err := e.seatID(handle)
	if err != nil {
		return err
	}
	subject, group := topics.AgentInbox(seat), topics.AgentInboxGroup(seat)
	// FOR THE ATTACHMENT THE DELIVERY CAME IN ON, which the seat holds here
	// until its release ends it (seatinbox.go).
	at := e.inboxOf(seat)
	e.pauses.mu.Lock()
	err = e.backends.Queue.PauseTopic(ctx, subject, group, at, seatPauseHold)
	if err == nil {
		e.recordHeldLocked(seat, at)
	}
	_, still := e.pauses.pauses[seat]
	e.pauses.mu.Unlock()
	if err != nil {
		return err
	}
	if !still {
		e.liftPauseHold(ctx, seat, "the seat was resumed while its delivery was being held")
	}
	return nil
}

// stopFor is the stop a pause asks of a seat's running turn, or nil. The
// handle is what the stop's sentence names the seat by.
func (e *Engine) stopFor(seat uuid.UUID, handle string) error {
	p, paused, _ := e.pauseOf(seat)
	if !paused || !p.StopRunning {
		return nil
	}
	return &stopError{pause: p, handle: handle}
}

// stopError is a running turn a person's pause ended.
//
// A TYPE rather than a formatted error, because the dispatcher that answers it
// names who stopped the turn on the record it publishes, and that is the pause
// the fence closed on — not whatever the cache holds by the time the turn has
// unwound. It unwraps to [turn.ErrStoppedByPerson], which is what every frame
// between the loop and the dispatcher recognises.
type stopError struct {
	pause  coord.SeatPause
	handle string

	// ofRun says the stop was DECIDED ON THE RUN the turn's executor was
	// launching — the launch whose attach it refused
	// ([sandbox.Coordinator.LaunchStopped]) — rather than met by the turn's
	// own fence: that stop's own ending then answers the turn's colleague and
	// owes the turn's end, from the run's record, once its read has kept the
	// job's session ([sandbox.CoordinatorOptions.CloseStopped]), and the
	// frame that launched the job answers and owes nothing of its own
	// ([stoppedByRun]).
	ofRun bool
}

func (e *stopError) Error() string {
	return fmt.Sprintf("%s paused %s and asked for its running turn to stop: %v",
		e.pause.By, e.handle, turn.ErrStoppedByPerson)
}

func (e *stopError) Unwrap() error { return turn.ErrStoppedByPerson }

// stoppedByRun reports whether err is a stop decided on the run the turn was
// launching ([stopError.ofRun]): the run's ending, not the frame, answers the
// turn's colleague and records how the turn ended. Answered by the frame as
// well, two replies reached the asker — each attempt passed the channel's open
// check before either closed it — and the asker was woken twice.
func stoppedByRun(err error) bool {
	var stop *stopError
	return errors.As(err, &stop) && stop.ofRun
}

// stopOf recovers the pause that stopped a turn from its error.
func stopOf(err error) (coord.SeatPause, bool) {
	var stop *stopError
	if errors.As(err, &stop) {
		return stop.pause, true
	}
	return coord.SeatPause{}, false
}

// recordHeldLocked notes a hold this client took, for the attachment inbox
// names. The caller holds the lock.
func (e *Engine) recordHeldLocked(seat uuid.UUID, inbox queue.Attachment) {
	if e.pauses.held == nil {
		e.pauses.held = map[uuid.UUID]queue.Attachment{}
	}
	e.pauses.held[seat] = inbox
}

// forgetPauseHold drops the record of seat's pause hold, for a seat this node
// has released ([Engine.releaseSeat]).
//
// NOT THE HOLD, which the release's detach dropped with the attachment it was
// taken for. Left, the record named that ended attachment, which no lift can
// reach: a resume afterwards lifted nothing on it and logged
// `seat_pause_lifted` all the same. The release forgets the seat's attachment
// first ([Engine.detachInbox]), so a pause folded after this records nothing
// for the seat, and a lift in flight lifts nothing and holds nothing again.
func (e *Engine) forgetPauseHold(seat uuid.UUID) {
	e.pauses.mu.Lock()
	defer e.pauses.mu.Unlock()
	delete(e.pauses.held, seat)
}

// seatLabel is how a log line names a seat: its handle in the running company,
// or its id where the company no longer holds it.
func (e *Engine) seatLabel(seat uuid.UUID) string {
	if handle, ok := e.seatHandle(seat.String()); ok {
		return handle
	}
	return seat.String()
}

// startSeatPauses opens the pause watch and waits, bounded, for its first
// complete answer, then keeps it current in the background.
//
// Detached from the caller's context like every other loop the node runs: a
// watch bound to a signal context would stop at SIGTERM while turns are still
// finishing, and a pause that asked to stop one of them must still reach it.
func (e *Engine) startSeatPauses(ctx context.Context) {
	loopCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	primed := make(chan struct{})
	e.pauses.cancel = cancel
	e.pauses.done = make(chan struct{})
	go func() {
		defer close(e.pauses.done)
		e.watchSeatPauses(loopCtx, primed)
	}()
	wait := time.NewTimer(seatPausePrimeBudget)
	defer wait.Stop()
	select {
	case <-primed:
	case <-wait.C:
		log.WarnContext(ctx, "seat_pauses_unread",
			"budget_seconds", seatPausePrimeBudget.Seconds(),
			"detail", "which seats are paused could not be read in time; this node's "+
				"deliveries are deferred until it can, and the watch keeps trying")
	case <-ctx.Done():
	}
}

// stopSeatPauses ends the watch and waits for it.
func (e *Engine) stopSeatPauses() {
	if e.pauses.cancel == nil {
		return
	}
	e.pauses.cancel()
	<-e.pauses.done
}

// watchSeatPauses keeps this node's copy current until ctx ends, closing
// primed once the first complete answer is in.
func (e *Engine) watchSeatPauses(ctx context.Context, primed chan struct{}) {
	var once sync.Once
	for attempt := 0; ; attempt++ {
		if e.watchOnce(ctx, func() { once.Do(func() { close(primed) }) }) {
			attempt = 0
		}
		if ctx.Err() != nil {
			return
		}
		delay := backoff.Doubling(attempt, seatPauseRewatchBase, seatPauseRewatchCeiling)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

// watchOnce runs one watch to its end, reporting whether it ever delivered a
// complete answer — which is what resets the re-watch's backoff.
func (e *Engine) watchOnce(ctx context.Context, primed func()) bool {
	updates, err := e.backends.Fleet.WatchSeatPauses(ctx)
	if err != nil {
		if ctx.Err() == nil {
			log.WarnContext(ctx, "seat_pause_watch_failed", "error", err,
				"detail", "this node keeps the pauses it last read and watches again")
		}
		return false
	}
	current := false
	snapshot := map[uuid.UUID]coord.SeatPause{}
	for u := range updates {
		switch {
		case u.Current:
			// A COMPLETE ANSWER, which replaces the copy whole: a pause
			// lifted while this node was not listening is missing from it,
			// and only a replacement can notice an absence.
			e.adoptSeatPauses(ctx, snapshot)
			current = true
			primed()
		case !current:
			if u.Pause != nil {
				snapshot[u.Seat] = *u.Pause
			}
		default:
			e.applySeatPause(ctx, u)
		}
	}
	if ctx.Err() == nil {
		log.WarnContext(ctx, "seat_pause_watch_ended",
			"detail", "the watch stopped hearing the store; this node keeps the pauses "+
				"it last read, and a paused seat stays paused, until it hears again")
	}
	return current
}

// adoptSeatPauses replaces this node's copy with a complete answer, taking the
// hold for every paused seat and lifting it from every seat no longer paused.
func (e *Engine) adoptSeatPauses(ctx context.Context, next map[uuid.UUID]coord.SeatPause) {
	e.pauses.mu.Lock()
	previous := e.pauses.pauses
	e.pauses.pauses = next
	e.pauses.known = true
	var lifted []uuid.UUID
	for seat := range previous {
		if _, still := next[seat]; !still {
			lifted = append(lifted, seat)
		}
	}
	for _, seat := range sortedSeats(maps.Keys(next)) {
		e.takePauseHoldLocked(ctx, seat)
	}
	e.pauses.mu.Unlock()
	for _, seat := range sortedSeats(slices.Values(lifted)) {
		e.liftPauseHold(ctx, seat, "the seat was resumed while this node was not listening")
	}
	// AND A STOP PLACED WHILE THIS NODE WAS NOT LISTENING ends the runs it
	// asked to end: a complete answer is the only word this node may ever
	// get of it. Idempotent, so a stop already carried out costs a listing.
	for _, seat := range sortedSeats(maps.Keys(next)) {
		e.stopSeatRuns(ctx, seat, next[seat])
	}
	// A COMPLETE ANSWER is the signal for every seat a retried resume was
	// waiting on the pauses for: one this node had not read at all, and one
	// lifted while it was not listening. A seat still paused is re-checked
	// and waits again.
	e.readmitResumes(waitPause)
	log.InfoContext(ctx, "seat_pauses_read", "paused", len(next))
}

// sortedSeats is a set of seat ids in a stable order, so a test and a log read
// the same way.
func sortedSeats(seats iter.Seq[uuid.UUID]) []uuid.UUID {
	return slices.SortedFunc(seats, func(a, b uuid.UUID) int {
		return cmp.Compare(a.String(), b.String())
	})
}

// applySeatPause folds one change into this node's copy.
func (e *Engine) applySeatPause(ctx context.Context, u coord.SeatPauseUpdate) {
	if u.Pause == nil {
		e.pauses.mu.Lock()
		delete(e.pauses.pauses, u.Seat)
		e.pauses.mu.Unlock()
		e.liftPauseHold(ctx, u.Seat, "a person resumed the seat")
		// AND AN ANSWER THE PAUSE KEPT WAITING resumes now rather than at
		// its next re-check: the resume is the event it was waiting for.
		// The coordinator keeps its waits by handle, which the running
		// company names; a seat it does not name — a revision this node
		// has not applied yet — re-checks every seat waiting on a pause,
		// which costs one look each, since a seat still paused waits again.
		if handle, ok := e.seatHandle(u.Seat.String()); ok {
			e.readmitResumes(waitPause, handle)
		} else {
			e.readmitResumes(waitPause)
		}
		return
	}
	e.pauses.mu.Lock()
	if e.pauses.pauses == nil {
		e.pauses.pauses = map[uuid.UUID]coord.SeatPause{}
	}
	e.pauses.pauses[u.Seat] = *u.Pause
	e.takePauseHoldLocked(ctx, u.Seat)
	e.pauses.mu.Unlock()
	log.InfoContext(ctx, "seat_paused", "seat", e.seatLabel(u.Seat), "agent_id", u.Seat.String(),
		"by", u.Pause.By, "by_kind", string(u.Pause.ByKind), "operator_id", u.Pause.OperatorID,
		"stop_running", u.Pause.StopRunning,
		"detail", "this seat takes no new work; its mail waits on its inbox until "+
			"somebody resumes it")
	// THE STOP, ONCE THE PAUSE IS IN THIS NODE'S COPY: a launch that asks its
	// fence after its row exists then either sees the pause or left a row
	// this listing finds ([sandbox.LaunchRequest.Fence]).
	e.stopSeatRuns(ctx, u.Seat, *u.Pause)
}

// takePauseHoldLocked takes the hold on one paused seat's inbox, for the
// attachment this node holds it under, reporting whether the inbox is held and
// the hold recorded once it returns. The caller holds the lock; the queue call
// does not drain, so that is safe.
//
// A SEAT THIS NODE DOES NOT HOLD is left alone: every node folds every pause,
// and the seat's acquisition records its attachment under this same lock
// ([Engine.acquireInbox]), so one that records it after this fold takes the
// hold itself, while one that recorded it before is held here, before or after
// its attach. A seat released as this runs ended the attachment, and the queue
// refuses the hold.
func (e *Engine) takePauseHoldLocked(ctx context.Context, seat uuid.UUID) bool {
	at := e.inboxOf(seat)
	if at == 0 {
		return false
	}
	subject, group := topics.AgentInbox(seat), topics.AgentInboxGroup(seat)
	err := e.backends.Queue.PauseTopic(ctx, subject, group, at, seatPauseHold)
	if errors.Is(err, queue.ErrAttachmentGone) {
		return false
	}
	if err != nil {
		// A pause fails only on a client that has stopped, which is a
		// process on its way out. The screening is the backstop: it reads
		// the same copy and holds whatever reaches it.
		log.WarnContext(ctx, "seat_pause_hold_not_taken", "agent_id", seat.String(), "error", err)
		return false
	}
	e.recordHeldLocked(seat, at)
	return true
}

// liftPauseHold releases the hold this client took on one seat, if it took
// one.
//
// OUTSIDE THE LOCK for the queue call, because the in-memory twin's resume
// drains synchronously into the seat's handler, and the first thing that
// handler does is read this copy. The cost of that is a window between the
// record's drop and the resume, which the read after the resume closes.
func (e *Engine) liftPauseHold(ctx context.Context, seat uuid.UUID, why string) {
	e.pauses.mu.Lock()
	at, held := e.pauses.held[seat]
	delete(e.pauses.held, seat)
	e.pauses.mu.Unlock()
	if !held {
		return
	}
	subject, group := topics.AgentInbox(seat), topics.AgentInboxGroup(seat)
	if err := e.backends.Queue.ResumeTopic(ctx, subject, group, at, seatPauseHold); err != nil {
		// Kept on the record, as the no-model release keeps its own, so a
		// later lift tries again: a resume fails only on a client that has
		// stopped.
		e.pauses.mu.Lock()
		e.recordHeldLocked(seat, at)
		e.pauses.mu.Unlock()
		log.WarnContext(ctx, "seat_pause_not_lifted", "seat", e.seatLabel(seat),
			"agent_id", seat.String(), "error", err,
			"detail", "this seat's held mail stays on its inbox until the queue client "+
				"is replaced")
		return
	}
	// AND A PAUSE FOLDED WHILE THE LIFT WAS IN FLIGHT HOLDS AGAIN: it re-holds
	// the attachment this lift names and records it, and the resume then lifts
	// the hold that pause owns. So the pause is read again under the lock once
	// the resume has returned, and one in force takes its hold back, for the
	// attachment the seat holds now, and records it
	// ([Engine.takePauseHoldLocked]). A pause folded after this read takes its
	// own; a lift after it reads again after its own resume. The package doc's
	// "A lift takes back a hold taken in its window" says why.
	e.pauses.mu.Lock()
	heldAgain := false
	if _, paused := e.pauses.pauses[seat]; paused {
		heldAgain = e.takePauseHoldLocked(ctx, seat)
	}
	e.pauses.mu.Unlock()
	// SAID WITH THE LIFT, because the pause that holds the seat again can
	// have logged its line before this one: without it the seat's last line
	// read lifted while its inbox was held.
	detail := "this pause's hold is lifted from the seat's inbox"
	if heldAgain {
		detail = "a pause placed while this lift was in flight holds the seat again, " +
			"so its inbox stays held until that pause is lifted"
	}
	log.InfoContext(ctx, "seat_pause_lifted", "seat", e.seatLabel(seat),
		"agent_id", seat.String(), "reason", why, "held_again", heldAgain, "detail", detail)
}

// clearRemovedSeatPauses lifts the pause of every seat the company no longer
// has.
//
// A removed seat's pause would outlive it: the bucket has no age. Every node
// tries; the delete is conditioned on the version read, so one wins. Asked by
// the seat's id (ADR-0013). Called wherever the roster can change
// ([Engine.followCompany]), ONLY ON THE FLEET'S CURRENT COMPANY
// ([Engine.fleetCompany]), as the mailbox sweep judges a seat absent: a node on
// a replaced revision lacks a seat the newer one added, and would delete a
// pause a person just placed on it. The apply that catches it up clears.
func (e *Engine) clearRemovedSeatPauses(ctx context.Context, next *Company) {
	if next == nil || next.Org == nil || e.backends == nil || e.backends.Fleet == nil {
		return
	}
	if err := e.fleetCompany(ctx, next); err != nil {
		log.DebugContext(ctx, "seat_pauses_not_swept", "reason", err.Error(),
			"detail", "a removed seat's pause is cleared by the company the fleet "+
				"is pointed at; this one is not it yet")
		return
	}
	e.pauses.mu.Lock()
	var gone []coord.SeatPause
	for seat, p := range e.pauses.pauses {
		if next.Org.AgentSeatByID(seat) == nil {
			gone = append(gone, p)
		}
	}
	e.pauses.mu.Unlock()
	slices.SortFunc(gone, func(a, b coord.SeatPause) int {
		return cmp.Compare(a.Seat.String(), b.Seat.String())
	})
	for _, p := range gone {
		lifted, err := e.backends.Fleet.DeleteSeatPause(ctx, p.Seat, p.Version)
		switch {
		case err != nil:
			log.WarnContext(ctx, "seat_pause_not_cleared", "agent_id", p.Seat.String(),
				"error", err,
				"detail", "the seat left the company and its pause record stays until the "+
					"next chart or settings change clears it")
		case lifted:
			log.InfoContext(ctx, "seat_pause_cleared", "agent_id", p.Seat.String(),
				"detail", "the seat left the company, so its pause went with it")
		}
	}
}

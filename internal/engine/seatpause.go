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
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// A PERSON'S PAUSE, CARRIED OUT BY WHICHEVER NODE HOLDS THE SEAT.
//
// A pause is a coordination record (coord.SeatPause): written by the node that
// served the person's request, read by every node. This file is the reading
// half, and it does four things with what it reads:
//
//   - HOLD THE INBOX. The moment a pause lands, this node takes the
//     [inbox.HoldSeatPaused] hold on the seat's mailbox, and lifts it the
//     moment the pause is lifted — so the seat's mail waits on the broker,
//     in order, and is delivered when somebody resumes it. Taken for every
//     paused seat rather than only the ones attached here, because a hold
//     lives on the subscription rather than on an attachment and costs a map
//     entry; a detach drops it, which is why the node takes it again before
//     it attaches a paused seat it acquires ([node.Config.AttachHolds]).
//   - ANSWER THE SCREENING. [inbox.Conditions.Paused] and PauseUnknown are
//     read off this cache, so a delivery that raced the hold is held and
//     parked rather than run, and one that arrives before this node has read
//     the pauses at all is deferred rather than guessed at.
//   - SKIP THE SCHEDULE. A paused seat's fires are recorded, not sent.
//   - STOP THE RUNNING TURN, when the pause asked to. The per-turn fence is
//     the seat's ownership fence composed with this cache: a pause carrying
//     StopRunning closes it with [turn.ErrStoppedByPerson] at the turn's next
//     round boundary, and the dispatcher spends the trigger rather than
//     retrying it. A DETACHED coding run is not fenced — it outlives its turn
//     by design — but the turn that resumes it is, and ends there.
//
// # Keyed on the seat's ID
//
// Every pause, every hold and every question here is about a seat's derived
// agent id, never its handle (ADR-0026): the record is keyed on it, the
// mailbox a hold sits on is named by it, and the scheduler and the dispatcher
// ask by it. Keyed on the handle, a rename read as a resume — the record named
// an address the seat no longer answered to — and a hire on the freed handle
// arrived paused by somebody who had paused a different colleague. The handle
// appears only in log lines, resolved from the running company.
//
// # Who has to agree on it
//
// The record is the whole company's (coordination). This cache is THIS NODE's
// copy of it, derived from a watch, and nothing reads it but this process: a
// peer asks its own copy. It is never written back.
//
// # Three states, not two
//
// Until the watch's first complete answer arrives this node does not know
// which seats are paused, and says so ([seatPauses.known] false): the
// screening defers, the scheduler dispatches (a paused seat's inbox holds a
// fire rather than running it), and nothing takes or lifts a hold. A watch
// that ends — the store went away — keeps the last complete answer rather than
// forgetting it: an unreachable store is not a resume.

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
	// by the seat id its mailbox is named after. RECORDED for the reason
	// [modelHolds] is: which holds this client took is a fact only this
	// process knows, and the queue contract lists none.
	held map[uuid.UUID]struct{}

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

// attachHolds names the holds a seat's mailbox is attached under — see
// [node.Config.AttachHolds]. A seat whose pause state is not yet known is
// attached with none: the screening defers its deliveries until it is.
func (e *Engine) attachHolds(seat uuid.UUID) []string {
	e.pauses.mu.Lock()
	defer e.pauses.mu.Unlock()
	if _, paused := e.pauses.pauses[seat]; !paused {
		return nil
	}
	// Recorded as HELD because the node is about to take it: the lift that
	// arrives after the attach must know there is something to release.
	e.recordHeldLocked(seat)
	return []string{seatPauseHold}
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
	e.pauses.mu.Lock()
	err = e.backends.Queue.PauseTopic(ctx, subject, group, seatPauseHold)
	if err == nil {
		e.recordHeldLocked(seat)
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
}

func (e *stopError) Error() string {
	return fmt.Sprintf("%s paused %s and asked for its running turn to stop: %v",
		e.pause.By, e.handle, turn.ErrStoppedByPerson)
}

func (e *stopError) Unwrap() error { return turn.ErrStoppedByPerson }

// stopOf recovers the pause that stopped a turn from its error.
func stopOf(err error) (coord.SeatPause, bool) {
	var stop *stopError
	if errors.As(err, &stop) {
		return stop.pause, true
	}
	return coord.SeatPause{}, false
}

// recordHeldLocked notes a hold this client took. The caller holds the lock.
func (e *Engine) recordHeldLocked(seat uuid.UUID) {
	if e.pauses.held == nil {
		e.pauses.held = map[uuid.UUID]struct{}{}
	}
	e.pauses.held[seat] = struct{}{}
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
}

// takePauseHoldLocked takes the hold on one paused seat's inbox. The caller
// holds the lock; the queue call does not drain, so that is safe.
func (e *Engine) takePauseHoldLocked(ctx context.Context, seat uuid.UUID) {
	subject, group := topics.AgentInbox(seat), topics.AgentInboxGroup(seat)
	if err := e.backends.Queue.PauseTopic(ctx, subject, group, seatPauseHold); err != nil {
		// A pause fails only on a client that has stopped, which is a
		// process on its way out. The screening is the backstop: it reads
		// the same copy and holds whatever reaches it.
		log.WarnContext(ctx, "seat_pause_hold_not_taken", "agent_id", seat.String(), "error", err)
		return
	}
	e.recordHeldLocked(seat)
}

// liftPauseHold releases the hold this client took on one seat, if it took
// one.
//
// OUTSIDE THE LOCK for the queue call, because the in-memory twin's resume
// drains synchronously into the seat's handler, and the first thing that
// handler does is read this copy.
func (e *Engine) liftPauseHold(ctx context.Context, seat uuid.UUID, why string) {
	e.pauses.mu.Lock()
	_, held := e.pauses.held[seat]
	delete(e.pauses.held, seat)
	e.pauses.mu.Unlock()
	if !held {
		return
	}
	subject, group := topics.AgentInbox(seat), topics.AgentInboxGroup(seat)
	if err := e.backends.Queue.ResumeTopic(ctx, subject, group, seatPauseHold); err != nil {
		// Kept on the record, as the no-model release keeps its own, so a
		// later lift tries again: a resume fails only on a client that has
		// stopped.
		e.pauses.mu.Lock()
		e.recordHeldLocked(seat)
		e.pauses.mu.Unlock()
		log.WarnContext(ctx, "seat_pause_not_lifted", "seat", e.seatLabel(seat),
			"agent_id", seat.String(), "error", err,
			"detail", "this seat's held mail stays on its inbox until the queue client "+
				"is replaced")
		return
	}
	log.InfoContext(ctx, "seat_pause_lifted", "seat", e.seatLabel(seat),
		"agent_id", seat.String(), "reason", why)
}

// clearRemovedSeatPauses lifts the pause of every seat the company no longer
// has.
//
// A pause belongs to a seat, and a seat the chart removed takes no work to
// hold. Left, the record would outlive the seat for ever — the bucket has no
// age. Every node composing the company tries; the delete is conditioned on
// the version this node read, so one wins and the rest find it gone, which the
// watch then tells every copy.
//
// ASKED BY THE SEAT'S ID, which a rename does not move, so a renamed seat
// keeps its pause; a seat removed and a new one hired under the freed handle
// have different ids, so the hire never inherits the leaver's pause.
//
// CALLED WHEREVER THE ROSTER CAN CHANGE, which is every published company
// ([Engine.convergeOn], which an apply and a chart write both reach). Seats are
// the chart's, and a removal moves no settings revision — run from the apply
// alone, a seat removed through /chart kept its pause record until somebody
// next applied settings.
//
// AND ONLY ON A COMPANY THAT HOLDS EVERY HIRE ([Engine.wholeRoster]), the rule
// the mailbox sweep judges a seat absent by. A node behind the chart log —
// lagging, or holding a hire's record it could not apply — does not have a seat
// hired on another node, and a pause somebody put on that new seat would read
// here as a pause on a seat the company does not have: the node would delete a
// pause a person had just placed. A company short of the log's end clears
// nothing; a node that has caught up, or the next convergence here, does.
func (e *Engine) clearRemovedSeatPauses(ctx context.Context, next *Company) {
	if next == nil || next.Org == nil || e.backends == nil || e.backends.Fleet == nil {
		return
	}
	if _, err := e.wholeRoster(ctx, func() *Company { return next }); err != nil {
		log.DebugContext(ctx, "seat_pauses_not_swept", "reason", err.Error(),
			"detail", "a removed seat's pause is cleared by a company that holds "+
				"every hire; this one does not yet")
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

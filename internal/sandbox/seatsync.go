package sandbox

import (
	"context"
	"errors"
	"slices"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
)

// SeatRuns is what this node knows about one seat's detached runs: whether one
// HOLDS the seat, and whether one is waiting for a person's answer. How many,
// against the seat's cap, is [Coordinator.SeatCount].
//
// The inbox screening reads BOTH values together because they change together:
// a run parking stops holding and starts awaiting in one moment, so two calls
// could land between the halves and see an idle seat with no question open, the
// state in which a person's reply is eaten as an ordinary turn.
func (c *Coordinator) SeatRuns(handle string) (held, awaitsAnswer bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	runs := c.runs[handle]
	return runs.holding > 0, runs.awaiting > 0
}

// SeatHeldBySandbox reports whether a detached run is HOLDING a seat, so it
// takes no new turn until the run settles: a job can run for hours, past any
// broker ack window, so the seat's inbox is HELD (nothing fetched) rather than
// its mail consumed and held (seathold.go). NOT "is a run waiting for an
// answer", which is [Coordinator.SeatRuns]'s second value; see [seatRuns].
func (c *Coordinator) SeatHeldBySandbox(handle string) bool {
	held, _ := c.SeatRuns(handle)
	return held
}

// setOf says which count a status belongs to. A status that owns no seat-level
// state (a run that has ended, or the empty string a write passes for no row
// at all, countwrite.go) belongs to neither.
func setOf(status string) (holding, awaiting int) {
	switch {
	case slices.Contains(Holding, status):
		return 1, 0
	case slices.Contains(Awaiting, status):
		return 0, 1
	}
	return 0, 0
}

// adjustLocked applies the deltas to handle's seat under c.mu in one step for
// both counts (a park is a decrement and an increment [Coordinator.SeatRuns]
// must never see apart) and reports the counts before and after, for the caller
// to free the seat outside the lock ([Coordinator.seatFreed]). Clamped at zero:
// a decrement for a run this node never counted is ordinary, and a negative
// count would swallow the next increment. See the package doc, "Seat counts".
func (c *Coordinator) adjustLocked(handle string, holding, awaiting int) (was, runs seatRuns) {
	c.moves[handle]++
	was = c.runs[handle]
	runs = was
	runs.holding = max(runs.holding+holding, 0)
	runs.awaiting = max(runs.awaiting+awaiting, 0)
	if runs == (seatRuns{}) {
		delete(c.runs, handle)
	} else {
		c.runs[handle] = runs
	}
	return was, runs
}

// moved says that a seat's runs moved in the store under this node's hand (an
// ending deleted a run's record), so a recount whose listing was read before is
// read again ([Coordinator.syncSeat]) rather than written after the recount
// that follows the delete. Said for every delete of this node's.
func (c *Coordinator) moved(handle string) {
	if handle == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.moves[handle]++
}

// seatFreed readmits on the seat's inbox what its runs leaving [Holding] freed
// ([Coordinator.readmitFreed]), signals [ConversationBusy] for a seat one of
// whose runs has just left, and [SeatFull] where that freed a slot below its
// cap, so an answer owed on it is resumed now rather than at its backstop, and
// tells [CoordinatorOptions.Busy] of a seat that became busy or free. Called
// outside c.mu, which [Coordinator.Readmit] takes.
func (c *Coordinator) seatFreed(ctx context.Context, handle string, was, now seatRuns) {
	c.readmitFreed(ctx, handle)
	// A RUN PARKED ON A WAIT ENDING frees the conversations it kept busy too.
	if now.holding < was.holding || now.awaiting < was.awaiting {
		c.Readmit(ConversationBusy, handle)
	}
	if now.holding < was.holding && now.holding < c.capOf(handle) {
		c.Readmit(SeatFull, handle)
	}
	if (was.holding > 0) != (now.holding > 0) && c.busy != nil {
		c.busy(handle)
	}
}

// OnEvent routes a seat's control-topic delivery. A refusal for a seat this node
// no longer holds names [SeatLost], handing the delivery to its holder. While
// this node keeps a clock on the box host's store ([Coordinator.unannouncedJob])
// a completion is acknowledged only where its run settled; every other answer
// hands it back marked [ErrUnannounced] and it is renewed (renew.go). A refusal
// the topic acknowledges is first recorded on the job's record
// ([Coordinator.oweRefused]). See the package doc, "Routing a control-topic
// delivery".
func (c *Coordinator) OnEvent(ctx context.Context, ev *events.Event) error {
	if ev == nil {
		return nil
	}
	switch payload := ev.Data.(type) {
	case *types.ExecutorLaunched:
		return c.OnStarted(ctx, *payload)
	case *types.ExecutorFinished:
		c.hearAnnouncement(ctx, *payload, ev)
		ctx = delivering(ctx, ev)
		answer := c.renewing(ctx, *payload, ev, c.OnCompleted(ctx, *payload, ev))
		return c.lost(payload.AgentHandle, c.oweRefused(ctx, *payload, answer))
	case *types.ExecutorAnswerGiven:
		if payload.FoldIn != "" {
			// A FOLD-IN'S ANSWER IS RECORDED ALREADY (AnswerDesk.AnswerFoldIn):
			// this delivery only hands back the wake it decides, should it wait
			// aside ([Coordinator.readmitFoldIn]), and tells the seat's router
			// to read it.
			c.readmitFoldIn(ctx, *payload)
			if c.foldInAnswered != nil {
				c.foldInAnswered(payload.AgentHandle)
			}
			return nil
		}
		return c.lost(payload.AgentHandle, c.OnAnswerGiven(ctx, *payload, ev))
	case *types.ExecutorWaitOver:
		return c.lost(payload.AgentHandle, c.OnWaitOver(ctx, *payload, ev))
	}
	return nil
}

// lost names a refusal of a seat this node no longer holds as [SeatLost],
// asking the node's own lease rather than taking the refusal's word: see
// [SeatLost] for the refusal that names no loss.
func (c *Coordinator) lost(handle string, err error) error {
	if err == nil || !errors.Is(err, ErrSeatNotHeld) {
		return err
	}
	if _, held := c.leaseOf(handle); held {
		return err
	}
	return &TurnRefused{Refusal: Refusal{Condition: SeatLost, Reason: err.Error()}, Cause: err}
}

// OnStarted recounts the seat from the store.
//
// NOT WHAT HOLDS THE SEAT: the launch did that as it wrote the row
// ([Coordinator.Launch]); this event arrives on a subscription the launching
// turn never waits for, and holding from here left a window for the seat's next
// mail to run beside the job. It puts the store's answer under the counts, on
// whichever node holds the seat. Idempotent on redelivery because it consults
// the store rather than incrementing: a double increment would hold the seat
// forever.
func (c *Coordinator) OnStarted(ctx context.Context, ev types.ExecutorLaunched) error {
	if ev.AgentHandle == "" {
		return nil
	}
	c.syncSeat(ctx, ev.AgentHandle)
	log.InfoContext(ctx, "sandbox_agent_busy",
		"agent", ev.AgentHandle, "turn_id", ev.TurnID, "sandbox_id", ev.SandboxID)
	return nil
}

// syncSeat sets both of a seat's counts from one listing of the store, the
// arbiter, so redeliveries, restarts and takeovers converge; an unreadable store
// leaves them alone. A recount during which this node moved the seat, or
// another recount was written, reads again, up to [seatRecountRounds]; a run
// with a write of this node's out is counted where those writes leave it
// ([Coordinator.countedLocked]), so a write's answer moves the count once. See
// the package doc, "Recounting a seat".
func (c *Coordinator) syncSeat(ctx context.Context, handle string) {
	for range seatRecountRounds {
		c.mu.Lock()
		moved := c.moves[handle]
		c.mu.Unlock()
		runs, err := c.pending.ListActiveForSeat(ctx, handle)
		if err != nil {
			log.WarnContext(ctx, "sandbox_busy_sync_failed", "agent", handle, "error", err.Error())
			return
		}
		served := c.servedOf(ctx, runs)
		c.mu.Lock()
		if c.moves[handle] != moved {
			c.mu.Unlock()
			continue
		}
		// COUNTED UNDER THE LOCK writes this node has out move the count once
		// their answers are taken, so such a run is counted where the moves so
		// far leave it, row listed or not.
		counts := c.countedLocked(handle, runs)
		c.heldFromListingLocked(handle, runs, served)
		// THIS RECOUNT MOVES THE SEAT TOO, for an older recount still on its way.
		c.moves[handle]++
		was := c.runs[handle]
		if counts == (seatRuns{}) {
			delete(c.runs, handle)
		} else {
			c.runs[handle] = counts
		}
		c.mu.Unlock()
		c.seatFreed(ctx, handle, was, counts)
		// The hold follows the recount, as it follows every transition.
		c.reconcileHold(ctx, handle)
		return
	}
	log.WarnContext(ctx, "sandbox_busy_sync_unsettled", "agent", handle,
		"detail", "the seat's runs kept moving under the recount, so the counts this node "+
			"already kept stand")
}

// countedLocked is handle's seat's counts from a listing of its runs: each run
// with no write out in the set its status names, and EVERY RUN WITH A WRITE OUT
// where the moves so far leave it ([Coordinator.writing]), whatever set the
// listing found it in or whether it has its row: a create landed after the
// listing, or a delete before it, has its move still to come (countwrite.go).
// See [Coordinator.syncSeat]. Under mu.
func (c *Coordinator) countedLocked(handle string, runs []PendingRun) seatRuns {
	var counts seatRuns
	for _, run := range runs {
		if c.writing[run.TurnID] != nil {
			continue
		}
		part := countOf(run.Status)
		counts.holding += part.holding
		counts.awaiting += part.awaiting
	}
	for _, writes := range c.writing {
		if writes.handle == handle {
			counts.holding += writes.counted.holding
			counts.awaiting += writes.counted.awaiting
		}
	}
	return counts
}

// seatRecountRounds bounds [Coordinator.syncSeat]'s listings.
//
// A recount is read again only when THIS NODE moved the seat while the listing
// ran, so it races only the seat's own transitions, which come in bursts no
// longer than an answered question's: answer recorded, run claimed, claim given
// back, next launch, four store writes at a listing's pace. Eight rounds is that
// burst twice over, so a recount starting at a burst's head still settles; a
// recount giving up may leave counts uncorrected a while (a park and a recorded
// answer trigger no recount). A bound rather than more, because every round
// lists every run the fleet holds.
const seatRecountRounds = 8

package sandbox

import "context"

// # Every write that moves a run between its seat's sets moves the count once
//
// A seat's two counts ([seatRuns]) are moved by the writes this node makes to
// the seat's runs, and set wholesale by a recount from the store's listing
// ([Coordinator.syncSeat]). A write that takes a run from one set to the other,
// or onto the store or off it, moves the count when its answer is taken, never
// as it lands: a write the store did not answer may not have landed, and one it
// refused moved nothing. Moved after the write's answer and outside the lock, as
// an answer's record, claim and declines moved it and an ending's delete still
// did, a recount whose listing was read after the write landed, and written
// before the move, counted the run where the write put it, and the move then
// counted it there a second time. The recount's guard reads again only where
// this node moved the seat while it listed, and a write that has landed with its
// move still to come is no move yet.
//
// So each such write is RECORDED BEFORE IT IS SENT, with the status it moves
// the run from and the one it moves it to ([Coordinator.writingCount]); a
// recount counts every run of the seat with a write out where the moves made so
// far leave it, whatever its listing found it in and WHETHER OR NOT ITS LISTING
// HAS ITS ROW ([Coordinator.countedLocked]): a delete that landed before the
// listing leaves the run out of it, its move still to come; and the write's
// answer makes its move once, under the lock, in the step that takes it
// ([Coordinator.wroteCountLocked]). The writes are the open of a run's row
// ([Coordinator.Launch], no row to launching, or the claim its resumed turn
// relaunches to launching), a park's question ([Coordinator.park], resumed to
// awaiting), an answer's record ([Coordinator.record], the question to
// answered), the claim that resumes the answer
// ([Coordinator.resumeOwedAdmitted], answered to resumed), either decline that
// lets it go ([Coordinator.dropUnopened], [Coordinator.declineAnswer], answered
// back to the question), the delete that ends a run this node counts, from its
// status to no row at all ([Coordinator.finishEnding], for the endings that
// count their run out: [Coordinator.settleNoted], [Coordinator.StopSeat] and
// [Coordinator.abandon], [ending.counted]), and a recovery's takeover of each
// run of the seat it acquires, from no set to the one it leaves the run in
// ([Coordinator.RecoverSeat], answered once the recovery is done with the run).
// A claim's release keeps a record of its own, which a recount reads the same
// way (claimrelease.go).
//
// Each double count cost the seat something. Counted out of the question set
// twice, a seat holding a second question read as having none open, and that
// question's answer was run as an unrelated turn ([seatRuns.awaiting]); counted
// into it twice, the seat's mail was offered to an answer match with nothing
// more to answer; counted holding twice, a claim kept the seat's inbox held
// until the next recount; and counted out of the holding set twice by an
// ending's delete, a seat whose other run still worked read free beside it, and
// its next mail ran beside that run.
//
// A DELETE THAT DID NOT LAND MOVES NOTHING EITHER. The ending is kept for this
// node's retry ([Coordinator.oweEnding]), which holds the seat's mail behind it,
// and its run stays counted where a recount reads its row until a delete lands.
// Counted out as the ending was decided, whether or not the delete landed, the
// seat read a run still on its row as gone, and the next recount counted it
// back. An ending that turns out not to be this node's, somebody else having
// ended the run or a newer lease owning it, or a run that moved on before its
// stop, makes no delete here at all: its caller recounts the seat, which reads
// the row as it stands, or none. An ending whose run this node does not count,
// or recounts the seat after, moves nothing at its delete: a recount after a
// write's answer reads the store's own answer, which no move then counts a
// second time.
//
// # Two writes out on one run move it in the order they were recorded
//
// A run can have a second write out before the first one's answer is taken: an
// answer by turn recorded to a question the park has written and not yet heard
// back on ([Coordinator.AnswerByTurn]); and a claim or a decline of one answer
// beside another, the attempt a reply makes as it is recorded and the one the
// answer's series makes on its timer. Writes on one run that each need the
// status the one before left (the record needs the park's question) land in the
// order they were recorded, and of writes that need the same status (two claims
// of one answer) at most one lands. Their answers come back in whatever order
// their callers do. So a write whose answer is taken while one recorded before
// it is still out moves nothing yet: its move is made with that earlier one's,
// in the order the two were recorded, in the step that takes the earlier answer.
// Made at once, as it was, the record's move counted the run out of the question
// set before the park's had counted it in, which the clamp at zero
// ([Coordinator.adjustLocked]) swallowed on a seat with no other question, and
// the park's move then counted the answered run as a question.
//
// THE WRITES OF ONE RUN ARE KEYED BY ITS TURN, whatever job each names: a turn
// has one row, which the store holds once, and a recount that counted a run's
// row apart from a write out on another job of it would count the one row twice.

// runWrites is the writes this node has out on one run of the seat named by
// handle ([Coordinator.writing]), in the order they were recorded, and the
// run's part of its seat's counts as the moves made so far leave it.
type runWrites struct {
	handle  string
	counted seatRuns
	out     []*countWrite
}

// countWrite is one write that moves a run between its seat's sets, from the
// status from to the status to: recorded before it is sent
// ([Coordinator.writingCount]), and done once its answer is taken, landed or
// not ([Coordinator.wroteCountLocked]).
type countWrite struct {
	turn         string
	handle       string
	from, to     string
	done, landed bool
}

// countOf is a run's part of its seat's counts in status: one in the set the
// status names, or nothing.
func countOf(status string) seatRuns {
	holding, awaiting := setOf(status)
	return seatRuns{holding: holding, awaiting: awaiting}
}

// writingCount records, before it is sent, a write that moves run from the
// status from to the status to, and returns it for
// [Coordinator.wroteCountLocked] to take its answer. Until run has no write
// out, a recount counts it where the moves made so far leave it: in from's set
// for a run with no other write out, until a move is made. A write recorded
// while another is out on run sets no count of its own: the run stays counted
// where the earlier write's moves leave it, and this write's move follows that
// one's.
func (c *Coordinator) writingCount(run PendingRun, from, to string) *countWrite {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writingCountLocked(run, from, to)
}

// writingCountLocked is [Coordinator.writingCount] for a caller holding c.mu.
func (c *Coordinator) writingCountLocked(run PendingRun, from, to string) *countWrite {
	w := &countWrite{turn: run.TurnID, handle: run.AgentHandle, from: from, to: to}
	writes := c.writing[w.turn]
	if writes == nil {
		writes = &runWrites{handle: run.AgentHandle, counted: countOf(from)}
		c.writing[w.turn] = writes
	}
	writes.out = append(writes.out, w)
	return w
}

// takingOver records, before a recovery fences any of them, the takeover of
// every run its listing found on handle's seat ([Coordinator.RecoverSeat]),
// each from no set at all, and starts the seat's counts afresh from what this
// node's writes out on the seat leave them, once they are recorded. It returns
// the takeovers, one per run, for [Coordinator.takeoverAnswered] to answer.
//
// AFRESH, because whatever this node counted of the seat before its recovery
// is a recount's, off the control topic the recovery attaches first, and the
// recovery counts each run again where it leaves it: added to that recount's,
// every run it took over was counted twice, and a tail it reaped stayed
// counted. Nothing else of this node's moves the seat before its recovery: its
// mailbox is not open, and a write of an earlier holding moves nothing
// ([Coordinator.ReleaseSeat]). A recount whose listing was read before this
// step reads again, as after any move ([Coordinator.syncSeat]).
func (c *Coordinator) takingOver(ctx context.Context, handle string, runs []PendingRun) []*countWrite {
	takeovers := make([]*countWrite, len(runs))
	c.mu.Lock()
	for i, run := range runs {
		takeovers[i] = c.writingCountLocked(run, "", "")
	}
	var counts seatRuns
	for _, writes := range c.writing {
		if writes.handle == handle {
			counts.holding += writes.counted.holding
			counts.awaiting += writes.counted.awaiting
		}
	}
	c.moves[handle]++
	was := c.runs[handle]
	if counts == (seatRuns{}) {
		delete(c.runs, handle)
	} else {
		c.runs[handle] = counts
	}
	c.mu.Unlock()
	c.seatFreed(handle, was, counts)
	c.reconcileHold(ctx, handle)
	return takeovers
}

// takeoverAnswered answers a takeover [Coordinator.takingOver] recorded:
// status is the one the recovery leaves its run counted in
// ([Coordinator.recoverRun]), and none for a run it counts in no set, which
// moves nothing.
func (c *Coordinator) takeoverAnswered(w *countWrite, status string) {
	c.mu.Lock()
	w.to = status
	moved := c.wroteCountLocked(w, status != "")
	c.mu.Unlock()
	moved()
}

// wroteCountLocked takes the answer to w, landed or not, and makes the moves
// that answer lets go: w's own, unless a write recorded before it on the same
// run is still out, and the moves of the writes recorded after it whose answers
// came first, in the order they were all recorded. A write that did not land,
// or whose answer the store did not give, moves nothing, and once the run's
// last write is answered a recount reads the run off the store's listing again.
// Under mu; the caller runs what it returns once it has let go of the lock,
// which frees the seat and has its inbox hold follow the move, as
// [Coordinator.moveRunLocked]'s does.
func (c *Coordinator) wroteCountLocked(w *countWrite, landed bool) func() {
	w.done, w.landed = true, landed
	writes := c.writing[w.turn]
	var move seatRuns
	for len(writes.out) > 0 && writes.out[0].done {
		next := writes.out[0]
		writes.out = writes.out[1:]
		if next.landed {
			from, to := countOf(next.from), countOf(next.to)
			move.holding += to.holding - from.holding
			move.awaiting += to.awaiting - from.awaiting
		}
	}
	writes.counted.holding += move.holding
	writes.counted.awaiting += move.awaiting
	if len(writes.out) == 0 {
		delete(c.writing, w.turn)
	}
	if move == (seatRuns{}) || w.handle == "" {
		return func() {}
	}
	was, runs := c.adjustLocked(w.handle, move.holding, move.awaiting)
	return func() {
		c.seatFreed(w.handle, was, runs)
		c.reconcileHold(c.life, w.handle)
	}
}

// countOfLocked is the part of its seat's counts a recount reads a listed run
// with no write out as ([Coordinator.countedLocked]): as the claim it still is
// for a run whose release is out ([Coordinator.releases]), and in the set its
// status names otherwise. Under mu.
func (c *Coordinator) countOfLocked(run PendingRun) seatRuns {
	if c.releasingLocked(run.TurnID, run.LaunchID) {
		return countOf(StatusResumed)
	}
	return countOf(run.Status)
}

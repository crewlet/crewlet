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
// A claim's release is one more such write, from the claim to the status it
// returns the run to, out for as long as the release is out or owed
// (claimrelease.go).
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
// A RELEASE STILL OWED HOLDS THE MOVES RECORDED BEHIND IT. A claim's release
// the store did not answer stays out on its run until an attempt at it is
// answered, its series making one at most [answerRetryCeiling] apart, and
// none while a claim of the run is on its way ([Coordinator.oweRelease],
// [Coordinator.retryOwed]), unless a claim of its job that wins takes it over
// first or a later job's release takes its place (claimrelease.go). A write
// recorded behind it on the run, the park of a job its resumed turn
// relaunched before it failed and handed the claim back, say, has its answer
// taken and moves nothing until then, and the run stays counted where the
// moves made before the release left it, holding the seat as the claim does.
// That is the recorded order's own rule, and the wait it makes is transient
// and bounded: it lasts only while the release is out, which the first
// attempt at it the store answers ends at the latest. And the seat's mail is
// held behind the owed release throughout ([Coordinator.owe]): what its inbox
// has waits there, and a delivery that raced the hold, or found it refused,
// waits as well, since the inbox's screening runs nothing on a seat it reads
// held, and it reads this one held while the run is counted as the claim
// ([Coordinator.SeatRuns]). So a run counted as the claim though its job has
// parked on a question is never missed by an answer match. Moved ahead of the
// release instead, the later write's move would be made before the move it
// follows, which is what this section's rule refuses.
//
// # Where each write puts the run
//
// A write that lands moves its run to the set its own status names, whatever
// the writes before it did, and the seat's counts by the difference from where
// the moves made so far had left it. Not by the sets it was recorded from and
// to: a write before it on the run whose answer was lost may have landed all
// the same, and moved by its recorded sets, a later write took the run out of
// a set it was never counted in. A park's question can land with its answer
// lost, the park moving nothing and handing its claim back, its release owed,
// and a reply then answers that question: the record, moved from the question
// set to the answer while the run was still counted as the claim, took the
// seat's count of questions down for a question it never counted, and a seat
// holding another question read it as closed. The status a write is recorded
// from is where a recount counts a run with no other write out, until a move
// is made ([Coordinator.writingCount]).
//
// # A move for a seat this node no longer holds moves nothing
//
// A seat's release drops every write this node has out on its runs, in the
// step that drops the seat's counts ([Coordinator.ReleaseSeat]), as it drops the
// claims' releases it was making or owed, and a write's answer taken after it
// moves nothing. Moved all the same, an answer taken after the release counted
// its run for a seat this node no longer held, and once this node took the
// seat back its recovery counted the run where it found it: a park's question
// counted twice, the seat's mail offered to an answer match with nothing more
// to answer, and an answer's record taking the seat's other question out of
// the question set. Kept in its entry instead, the write held the recovery's
// takeover of its run behind it, the run counted where the write was recorded
// from until the write's answer came. A write recorded after the release, by a
// frame of the earlier holding still running here, moves a count nothing reads
// for a seat this node does not hold, and the recovery that takes the seat back
// starts its counts afresh ([Coordinator.takingOver]).
//
// # Every write is settled on every way out
//
// A write is answered on every way out of its store call, a panic included
// ([Coordinator.settleWrite], deferred where the write is recorded): the
// queue's handler recovers a panic and the process lives on, and a write left
// out held every later write's move on its run behind it, the run counted
// where the write was recorded from until the seat moved. A panic is no answer
// from the store, so such a write moves nothing. So is a claim's release,
// whose record refused every claim of its job, and kept its run counted as
// the claim, for as long as it stood ([Coordinator.unclaimOwing]), and the
// count of a job's claims on their way, which held an owed release of the job
// back while it was above nothing ([Coordinator.claimCompletion]).
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

	// entry is the run's entry the write was recorded in: one the seat's
	// release dropped is no longer the run's ([Coordinator.ReleaseSeat]),
	// and the write's answer then moves nothing.
	entry *runWrites
}

// settleWrite answers w as a write that did not land, unless its answer was
// taken already: deferred on every way out of a write, a panic included, so that
// one whose store call panicked is not left out on its run. The queue's handler
// recovers a panic and the process lives on: left out, the write held every
// later write's move on its run behind it, the run counted where it was
// recorded from until the seat moved. A panic is no answer from the store, and
// a write whose answer the store did not give moves nothing.
func (c *Coordinator) settleWrite(w *countWrite) {
	moved := func() {}
	c.mu.Lock()
	if !w.done {
		moved = c.wroteCountLocked(w, false)
	}
	c.mu.Unlock()
	moved()
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
// where the earlier write's moves leave it, and this write's move, to its own
// to's set, follows that one's.
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
	w.entry = writes
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
// ([Coordinator.recoverRun]), none for a run it counts in no set. ANSWERED AS
// LANDED EITHER WAY: the recovery's own reading of the run is its last word on
// where this node counts it, after any write recorded on the run before it.
func (c *Coordinator) takeoverAnswered(w *countWrite, status string) {
	c.mu.Lock()
	w.to = status
	moved := c.wroteCountLocked(w, true)
	c.mu.Unlock()
	moved()
}

// wroteCountLocked takes the answer to w, landed or not, and makes the moves
// that answer lets go: w's own, unless a write recorded before it on the same
// run is still out, and the moves of the writes recorded after it whose answers
// came first, in the order they were all recorded, each landed one putting the
// run in its own to's set. A write that did not land, or whose answer the store
// did not give, moves nothing, and once the run's last write is answered a
// recount reads the run off the store's listing again. Nor does a write whose
// seat this node let go of while it was out.
// Under mu; the caller runs what it returns once it has let go of the lock,
// which frees the seat and has its inbox hold follow the move.
func (c *Coordinator) wroteCountLocked(w *countWrite, landed bool) func() {
	w.done, w.landed = true, landed
	writes := c.writing[w.turn]
	if writes != w.entry {
		// A SEAT THIS NODE NO LONGER HOLDS: see "A move for a seat this
		// node no longer holds moves nothing" above.
		return func() {}
	}
	before := writes.counted
	for len(writes.out) > 0 && writes.out[0].done {
		next := writes.out[0]
		writes.out = writes.out[1:]
		if next.landed {
			// WHERE THE WRITE PUT THE RUN, whatever the writes before it
			// did: see "Where each write puts the run" above.
			writes.counted = countOf(next.to)
		}
	}
	move := seatRuns{
		holding:  writes.counted.holding - before.holding,
		awaiting: writes.counted.awaiting - before.awaiting,
	}
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

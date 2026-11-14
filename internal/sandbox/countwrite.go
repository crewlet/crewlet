package sandbox

// # A write that moves a run between its seat's sets moves the count once
//
// A seat's two counts ([seatRuns]) are moved by the writes this node makes to
// the seat's runs, and set wholesale by a recount from the store's listing
// ([Coordinator.syncSeat]). A write that takes a run from one set to the other
// moves the count when its answer is taken, never as it lands: a write the
// store did not answer may not have landed, and one it refused moved nothing.
// Moved after the write's answer and outside the lock, as an answer's record,
// claim and declines moved it, a recount whose listing was read after the write
// landed, and written before the move, counted the run where the write put it,
// and the move then counted it there a second time. The recount's guard reads
// again only where this node moved the seat while it listed, and a write that
// has landed with its move still to come is no move yet.
//
// So each such write is RECORDED BEFORE IT IS SENT, with the status it moves
// the run from and the one it moves it to ([Coordinator.writingCount]); a
// recount counts a run with a write out where the moves made so far leave it,
// whatever the listing found it in ([Coordinator.countOfLocked]); and the
// write's answer makes its move once, under the lock, in the step that takes it
// ([Coordinator.wroteCountLocked]). The writes are a park's question
// ([Coordinator.park], resumed to awaiting), an answer's record
// ([Coordinator.record], the question to answered), the claim that resumes the
// answer ([Coordinator.resumeOwedAdmitted], answered to resumed), and either
// decline that lets it go ([Coordinator.dropUnopened],
// [Coordinator.declineAnswer], answered back to the question). A claim's
// release keeps a record of its own, which a recount reads the same way
// (claimrelease.go).
//
// Each double count cost the seat something. Counted out of the question set
// twice, a seat holding a second question read as having none open, and that
// question's answer was run as an unrelated turn ([seatRuns.awaiting]); counted
// into it twice, the seat's mail was offered to an answer match with nothing
// more to answer; and counted holding twice, a claim kept the seat's inbox held
// until the next recount.
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
// ([Coordinator.adjust]) swallowed on a seat with no other question, and the
// park's move then counted the answered run as a question.

// jobKey names one job of a turn's run: the turn, and the launch that made the
// job.
type jobKey struct{ turn, launch string }

// runWrites is the writes this node has out on one job's run
// ([Coordinator.writing]), in the order they were recorded, and the run's part
// of its seat's counts as the moves made so far leave it.
type runWrites struct {
	counted seatRuns
	out     []*countWrite
}

// countWrite is one write that moves a run between its seat's sets, from the
// status from to the status to: recorded before it is sent
// ([Coordinator.writingCount]), and done once its answer is taken, landed or
// not ([Coordinator.wroteCountLocked]).
type countWrite struct {
	key          jobKey
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
	w := &countWrite{key: jobKey{turn: run.TurnID, launch: run.LaunchID}, handle: run.AgentHandle,
		from: from, to: to}
	c.mu.Lock()
	defer c.mu.Unlock()
	writes := c.writing[w.key]
	if writes == nil {
		writes = &runWrites{counted: countOf(from)}
		c.writing[w.key] = writes
	}
	writes.out = append(writes.out, w)
	return w
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
	writes := c.writing[w.key]
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
		delete(c.writing, w.key)
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

// countOfLocked is run's part of its seat's counts as a recount reads it off a
// listing ([Coordinator.syncSeat]): as the claim it still is for a run whose
// release is out ([Coordinator.releases]), where the moves made so far leave a
// run with another write out ([Coordinator.writing]), and in the set its status
// names otherwise. Under mu.
func (c *Coordinator) countOfLocked(run PendingRun) seatRuns {
	if c.releasingLocked(run.TurnID, run.LaunchID) {
		return countOf(StatusResumed)
	}
	if writes := c.writing[jobKey{turn: run.TurnID, launch: run.LaunchID}]; writes != nil {
		return writes.counted
	}
	return countOf(run.Status)
}

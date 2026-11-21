package sandbox

import "context"

// Every write that moves a run between its seat's sets moves the count once:
// recorded before it is sent ([Coordinator.writingCount]), counted by a recount
// where the moves made so far leave it ([Coordinator.countedLocked]), and moved
// once when its answer is taken ([Coordinator.wroteCountLocked]). A delete that
// did not land moves nothing; two writes out on one run move in the order
// recorded, a release still owed holding those behind it; a write moves the run
// to the set its status names; a seat's release drops every write out; every
// write is settled on every way out ([Coordinator.settleWrite]). Keyed by turn.
// See the package doc, "Every write that moves a run between its seat's sets
// moves the count once".

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
// node's writes out on the seat leave them. It returns the takeovers, one per
// run, for [Coordinator.takeoverAnswered] to answer. Afresh, because what this
// node counted before is a recount's off the control topic, and added to it
// every run taken over would be counted twice. See the package doc, "Every
// write that moves a run between its seat's sets moves the count once".
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

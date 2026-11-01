package livestate

import "github.com/crewlet/crewlet/internal/events/types"

// The turn a seat is on, and the last one it ended.
//
// A LIVE CALL IS NOT A TURN. The in-flight call is one phase's model call, and
// it is cleared the instant that phase completes — so between phases, during
// the prefetch before the first one, and for the whole of a wait on a detached
// executor, a seat that was plainly in the middle of a turn showed nothing at
// all. A parked turn was worse than blank: its suspension publishes a turn
// completion, and the projection read that as the end of the turn and said the
// seat was idle while its work ran on in a box. The turn is therefore held
// here on its own, from the event that ANNOUNCES it (`agent_turn_started`, which
// every turn and every resumed segment publishes before its prefetch) to the one
// that ends it, and the stage says where in it the seat is.
//
// ORDERING. The start, the phases and the completion travel on different
// subjects, so any of them can overtake another. Two guards, the same two the
// in-flight call has: a stage moves only on an event not older than the one
// that set it, and a turn that has ENDED is remembered by id so a straggler
// from it cannot put it back on the seat.

// touchTurn records that a step of turn `turnID` happened on this seat, at
// `stage`.
//
// It creates the turn when the seat is on none — the start may have been
// missed or overtaken — and replaces a turn the seat was on only with a NEWER
// one: an event of an older turn arriving late is not the seat moving back to
// it.
func (s *LiveState) touchTurn(agent *agentLive, env Envelope, payload map[string]any, stage Stage) {
	s.markTurn(agent, env, payload, stage, true)
}

// noteTurn records an event that names turn `turnID` without being one of its
// own steps — see [phaseStage] — and so MOVES NO STAGE: it opens the turn at
// `stage` on a seat that holds none, as [LiveState.touchTurn] does, and leaves
// the stage of a turn the seat holds, and the instant that stage was set,
// where the turn's own steps put them. What it still states is what every
// event of the turn states: the node that published it, the item the turn is
// on, and a failure.
func (s *LiveState) noteTurn(agent *agentLive, env Envelope, payload map[string]any, stage Stage) {
	s.markTurn(agent, env, payload, stage, false)
}

// phaseStage is the stage a phase record puts its turn in, and whether it
// MOVES the stage of a turn the seat already holds.
//
// ONLY THE TURN'S OWN PHASES MOVE IT — onboarding, execute and review, each
// published by the segment that runs it. A NESTED record (`host_phase` set)
// names the turn and is not one of them: a delegated worker, or the round-cap
// judge a phase consults, each run while the phase that called it waits. That
// phase's own events stage the turn. (Nor does a collected launch of the
// executor's run move it, which is no phase record at all — see
// [LiveState.applyTurnEvent].)
//
// EVERY WORKER RUNS WHILE ITS TURN IS PARKED. `delegate` is the executor's
// alone and every executor is its coding CLI's run in a box, which calls it
// over the bridge after the turn has parked. Read as one of the turn's own
// phases, a worker's record un-parked the turn: the seat's strip stopped
// saying its executor was working while it worked on, and the seat's
// next spawn took the turn off it as one its old instance had died with (the
// spawn arm of [LiveState.applyTurnEvent] spares a parked turn alone). Such a
// record still OPENS the turn on a seat that holds none, at the stage it
// implies: parked under the executor's run, in a phase under any other host.
func phaseStage(payload map[string]any) (stage Stage, moves bool) {
	host := str(payload, "host_phase")
	switch {
	case host == string(types.PhaseExecute):
		return StageParked, false
	case host != "":
		return StagePhase, false
	}
	return StagePhase, true
}

// markTurn is [LiveState.touchTurn] and [LiveState.noteTurn]: `moves` says
// whether the event moves the turn's stage.
func (s *LiveState) markTurn(agent *agentLive, env Envelope, payload map[string]any, stage Stage, moves bool) {
	turnID := str(payload, "turn_id")
	if turnID == "" {
		return
	}
	at := newStamp(env.Timestamp)
	if ended, ok := s.endedTurns.get(turnID); ok && (at.empty() || !at.after(ended)) {
		return
	}
	cur := agent.turn
	if cur != nil && cur.TurnID != turnID {
		if !at.empty() && !agent.turnAt.empty() && at.before(agent.turnAt) {
			return
		}
		cur = nil
	}
	if cur == nil {
		// AN EVENT OLDER THAN THE SEAT'S LAST TURN MOVE opens nothing: a
		// seat runs one turn at a time, so it belongs to a turn already
		// over — one whose end this projection never saw.
		if agent.turn == nil && !at.empty() && !agent.turnAt.empty() && at.before(agent.turnAt) {
			return
		}
		cur = &LiveTurn{TurnID: turnID, StartedAt: env.Timestamp, Stage: stage}
		agent.turn = cur
		agent.turnAt = at
	}
	// A STAGE FROM AN OLDER EVENT DOES NOT MOVE IT. The start is published
	// before the prefetch and the phase after it, but they are two subjects:
	// a start that lands after its phase began would otherwise put a working
	// seat back to gathering context. A RESUMED segment's start is newer
	// than the suspension that parked it, so it moves the turn on.
	if at.empty() || agent.turnAt.empty() || !at.before(agent.turnAt) {
		if moves {
			cur.Stage = stage
			if !at.empty() {
				agent.turnAt = at
			}
		}
		if node := str(payload, "node"); node != "" {
			cur.Node = node
		}
	}
	if item := workItemOf(payload); item != nil {
		cur.WorkItem = item
	}
	if basis := str(payload, "work_item_basis"); basis != "" {
		cur.WorkItemBasis = types.WorkItemBasis(basis)
	}
	// THE START STATES WHEN THE TURN BEGAN, and it wins over the guess the
	// first event seen made — unless it is a RESUMED segment's, whose own
	// start is the segment's, while the turn began when it first did.
	if env.Type == "agent_turn_started" && !flag(payload, "resumed") {
		if started := str(payload, "started_at"); started != "" {
			cur.StartedAt = started
		}
	}
	if env.Failed {
		cur.failed = true
	}
}

// endTurnRecord ends turn `turnID` on this seat: it records the seat's last
// turn and, when the seat is still on that turn, takes it off.
//
// `failed` is whether the ending event itself was a failure; any earlier
// failure of the same turn is already on the held turn. `stopped` is whether a
// person ended it, which is the outcome whatever else went wrong in it: the
// turn ended because somebody meant it to, and a stop is never a failure.
func (s *LiveState) endTurnRecord(agent *agentLive, env Envelope, turnID string, failed, stopped bool) {
	if turnID == "" {
		return
	}
	at := newStamp(env.Timestamp)
	s.endedTurns.put(turnID, at)
	if agent.turn != nil && agent.turn.TurnID == turnID {
		failed = failed || agent.turn.failed
		agent.turn = nil
		agent.turnAt = at
	}
	outcome := OutcomeCompleted
	switch {
	case stopped:
		outcome = OutcomeStopped
	case failed:
		outcome = OutcomeFailed
	}
	s.setLastTurn(agent, LastTurn{TurnID: turnID, EndedAt: env.Timestamp, Outcome: outcome}, at)
}

// setLastTurn replaces the seat's last turn with `last`, unless the one held
// ended later.
func (s *LiveState) setLastTurn(agent *agentLive, last LastTurn, at stamp) {
	if agent.lastTurn != nil && !at.empty() && !agent.lastTurnAt.empty() &&
		at.before(agent.lastTurnAt) {
		return
	}
	agent.lastTurn = &last
	agent.lastTurnAt = at
}

// extendLastTurn moves the last turn's end to a later event OF THAT TURN — the
// reflection pass publishes after the completion, and the turn list's `ended_at`
// is the newest event of the turn, so the two agree.
func (s *LiveState) extendLastTurn(agent *agentLive, env Envelope, turnID string) {
	last := agent.lastTurn
	if last == nil || turnID == "" || last.TurnID != turnID {
		return
	}
	at := newStamp(env.Timestamp)
	if at.empty() || !at.after(agent.lastTurnAt) {
		return
	}
	dup := *last
	dup.EndedAt = env.Timestamp
	agent.lastTurn = &dup
	agent.lastTurnAt = at
}

// applyTurnStarted is `agent_turn_started`: the turn is announced before its
// prefetch, so the seat is working from here rather than from its first phase.
func (s *LiveState) applyTurnStarted(agent *agentLive, env Envelope, payload map[string]any) bool {
	turnID := str(payload, "turn_id")
	if turnID == "" {
		return false
	}
	if ended, ok := s.endedTurns.get(turnID); ok {
		if at := newStamp(env.Timestamp); at.empty() || !at.after(ended) {
			return false
		}
	}
	before := agent.overlay()
	s.touchTurn(agent, env, payload, StageContext)
	// THE SEAT IS WORKING FROM ITS TURN'S START, not from its first phase:
	// the prefetch before it is work, and a seat that looked idle through
	// it looked idle with a wake in hand. On the state machine's own
	// reorder guard, so a start older than the seat's newest state — a
	// failure hold that came after it — moves nothing.
	at := newStamp(env.Timestamp)
	if agent.turn != nil && agent.turn.TurnID == turnID && agent.turn.Stage == StageContext &&
		(at.empty() || agent.stateTS.empty() || !at.before(agent.stateTS)) {
		agent.working()
		if !at.empty() {
			agent.stateTS = at
		}
	}
	return !sameTurnOverlay(before, agent.overlay())
}

// endLostRun is an executor that was LOST — `executor_failed`. The turn that
// launched it was parked waiting for it, and the run's loss settles that turn
// like any other lost turn: nothing resumes it, so a turn left on the seat
// would read as parked for good.
//
// ONLY A PARKED TURN ENDS HERE. A run fails while still launching too, when the
// turn that launched it is still running and goes on to complete on its own —
// so a turn in any other stage only learns that it carried a failure.
func (s *LiveState) endLostRun(env Envelope, payload map[string]any) bool {
	id := str(payload, "agent_id")
	turnID := str(payload, "turn_id")
	if id == "" || turnID == "" {
		return false
	}
	agent := s.agents[id]
	if agent == nil {
		return false
	}
	switch held := agent.turn; {
	case held != nil && held.TurnID == turnID && held.Stage == StageParked:
		s.endTurnRecord(agent, env, turnID, true, false)
		return true
	case held != nil && held.TurnID == turnID:
		held.failed = true
	}
	return false
}

// sameTurnOverlay reports whether two overlays agree on everything a turn event
// can move.
func sameTurnOverlay(a, b Overlay) bool {
	return turnEqual(a.Turn, b.Turn) && lastEqual(a.LastTurn, b.LastTurn)
}

func turnEqual(a, b *LiveTurn) bool {
	switch {
	case a == nil || b == nil:
		return a == b
	case (a.WorkItem == nil) != (b.WorkItem == nil):
		return false
	case a.WorkItem != nil && *a.WorkItem != *b.WorkItem:
		return false
	}
	return a.TurnID == b.TurnID && a.WorkItemBasis == b.WorkItemBasis &&
		a.StartedAt == b.StartedAt && a.Stage == b.Stage && a.Node == b.Node
}

func lastEqual(a, b *LastTurn) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// clone copies a turn for a reader, so the one handed out cannot change under
// it.
func (t *LiveTurn) clone() *LiveTurn {
	if t == nil {
		return nil
	}
	dup := *t
	dup.WorkItem = cloneItem(t.WorkItem)
	return &dup
}

func (l *LastTurn) clone() *LastTurn {
	if l == nil {
		return nil
	}
	dup := *l
	return &dup
}

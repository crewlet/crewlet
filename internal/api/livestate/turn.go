package livestate

import (
	"cmp"
	"slices"

	"github.com/crewlet/crewlet/internal/events/types"
)

// The turns a seat is on, and the last one it ended.
//
// A LIVE CALL IS NOT A TURN: it is cleared when its phase completes, so a seat
// between phases or waiting on a detached executor would show nothing, and a
// parked turn's suspension publishes a completion that read as the turn's end.
// A turn is held on its own, from `agent_turn_started` to the event that ends
// it, and its stage says where in it the seat is.

// SEVERAL AT ONCE: a seat runs up to its sandbox.max_concurrent_runs turns, at
// most one per conversation, so a turn is held by its id beside the others and
// no event of one turn is read against another's. An event of a turn the seat
// does not hold opens it, however old, unless its end was seen.
//
// ORDERING. Start, phases and completion travel on different subjects and can
// overtake one another. Two guards, as for the in-flight call: a stage moves
// only on an event not older than the one that set THAT turn's stage, and an
// ENDED turn is remembered by id so a straggler cannot put it back on the seat.

// heldTurn is a turn the seat is on and `at`, the instant of the event that
// last moved it: the turn's own reorder guard.
type heldTurn struct {
	turn *LiveTurn
	at   stamp
}

// turnList is the seat's turns for a reader, oldest first by start, `[]` for
// none: copies, so the list handed out cannot change under it.
func (a *agentLive) turnList() []LiveTurn {
	out := make([]LiveTurn, 0, len(a.turns))
	for _, held := range a.turns {
		out = append(out, *held.turn.clone())
	}
	slices.SortFunc(out, func(x, y LiveTurn) int {
		return cmp.Or(newStamp(x.StartedAt).chronological(newStamp(y.StartedAt)),
			cmp.Compare(x.TurnID, y.TurnID))
	})
	return out
}

// touchTurn records that a step of turn `turnID` happened on this seat, at
// `stage`.
//
// It opens the turn when the seat does not hold it — the start may have been
// missed or overtaken — beside any other turn the seat is on.
func (s *LiveState) touchTurn(agent *agentLive, env Envelope, payload map[string]any, stage Stage) {
	s.markTurn(agent, env, payload, stage, true)
}

// noteTurn records an event that names turn `turnID` without being one of its
// own steps — see [phaseStage] — and so MOVES NO STAGE: it opens the turn at
// `stage` on a seat that does not hold it, as [LiveState.touchTurn] does, and leaves
// the stage of a turn the seat holds, and the instant that stage was set,
// where the turn's own steps put them. What it still states is what every
// event of the turn states: the node that published it, the item the turn is
// on, and a failure.
func (s *LiveState) noteTurn(agent *agentLive, env Envelope, payload map[string]any, stage Stage) {
	s.markTurn(agent, env, payload, stage, false)
}

// phaseStage is the stage a phase record puts its turn in, and whether it
// MOVES the stage of a turn the seat already holds. ONLY THE TURN'S OWN PHASES MOVE IT: onboarding, execute and review. A NESTED
// record (`host_phase` set) is a delegated worker or the round-cap judge,
// running while its calling phase waits; that phase's events stage the turn.
//
// EVERY WORKER RUNS WHILE ITS TURN IS PARKED (`delegate` is the executor's
// alone, called after the turn parked). Read as a phase, a worker's record
// un-parked the turn, and the next spawn took it off the seat (the spawn arm of
// [LiveState.applyTurnEvent] spares a parked turn alone). Such a record still
// OPENS the turn on a seat holding none: parked under the executor's run, in a
// phase under any other host.
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

// ownFailure reports whether an event is a failure of its turn's own work. A nested record's
// failure (`host_phase` set: a worker, or a round-cap judge) is that record's alone: the phase
// that called it reads it as an answer, so it fails neither the turn nor the seat.
func ownFailure(env Envelope, payload map[string]any) bool {
	return env.Failed && str(payload, "host_phase") == ""
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
	held := agent.turns[turnID]
	if held == nil {
		// OPENED HOWEVER OLD THE EVENT: the seat's other turns say nothing
		// about this one, which may be a second turn running beside them.
		held = &heldTurn{turn: &LiveTurn{TurnID: turnID, StartedAt: env.Timestamp, Stage: stage}, at: at}
		if agent.turns == nil {
			agent.turns = map[string]*heldTurn{}
		}
		agent.turns[turnID] = held
	}
	cur := held.turn
	// A STAGE FROM AN OLDER EVENT DOES NOT MOVE IT. The start is published
	// before the prefetch and the phase after it, but they are two subjects:
	// a start that lands after its phase began would otherwise put a working
	// seat back to gathering context. A RESUMED segment's start is newer
	// than the suspension that parked it, so it moves the turn on.
	if at.empty() || held.at.empty() || !at.before(held.at) {
		if moves {
			cur.Stage = stage
			if !at.empty() {
				held.at = at
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
	if ownFailure(env, payload) {
		cur.failed = true
	}
}

// endTurnRecord ends turn `turnID` on this seat: it records the seat's last
// turn and, when the seat holds that turn, takes it off.
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
	if held := agent.turns[turnID]; held != nil {
		failed = failed || held.turn.failed
		delete(agent.turns, turnID)
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
	if held := agent.turns[turnID]; held != nil && held.turn.Stage == StageContext &&
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
	switch held := agent.turns[turnID]; {
	case held != nil && held.turn.Stage == StageParked:
		s.endTurnRecord(agent, env, turnID, true, false)
		return true
	case held != nil:
		held.turn.failed = true
	}
	return false
}

// sameTurnOverlay reports whether two overlays agree on everything a turn event
// can move.
func sameTurnOverlay(a, b Overlay) bool {
	return slices.EqualFunc(a.Turns, b.Turns, turnEqual) && lastEqual(a.LastTurn, b.LastTurn)
}

func turnEqual(a, b LiveTurn) bool {
	switch {
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

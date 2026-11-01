package livestate

// The in-flight call: seeded when a phase opens, folded on every progress
// round, frozen on failure, cleared on a clean completion.

import (
	"slices"

	"github.com/crewlet/crewlet/internal/events/types"
)

// clone returns a deep-enough copy for a reader to hold.
//
// The overlay a caller receives must not alias the projection's own state: a
// dashboard push is serialized on another goroutine, and the maps and slices
// inside a call are replaced wholesale by the next round rather than mutated —
// except on the failure path, which writes into the live call in place. One
// copy at the boundary is cheaper to reason about than auditing which fields
// each writer touches.
func (c *LiveCall) clone() *LiveCall {
	if c == nil {
		return nil
	}
	dup := *c
	dup.Error = c.Error.clone()
	if c.Trigger != nil {
		dup.Trigger = make(map[string]any, len(c.Trigger))
		for k, v := range c.Trigger {
			dup.Trigger[k] = v
		}
	}
	dup.PromptMessages = append([]any(nil), c.PromptMessages...)
	dup.ToolExecutions = append([]any(nil), c.ToolExecutions...)
	dup.RoundNarration = append([]any(nil), c.RoundNarration...)
	dup.Rounds = append([]any(nil), c.Rounds...)
	dup.Invocations = append([]any(nil), c.Invocations...)
	dup.RunningCalls = append([]any(nil), c.RunningCalls...)
	dup.Steers = append([]any(nil), c.Steers...)
	dup.WorkItem = cloneItem(c.WorkItem)
	if c.PartialRound != nil {
		dup.PartialRound = make(map[string]any, len(c.PartialRound))
		for k, v := range c.PartialRound {
			dup.PartialRound[k] = v
		}
	}
	return &dup
}

func (e *ErrorInfo) clone() *ErrorInfo {
	if e == nil {
		return nil
	}
	dup := *e
	return &dup
}

// clone copies the window list, so an overlay handed to a caller cannot be
// changed under it by the next report. A window's Limit pointer is shared: it
// is decoded fresh for every report and never written after.
func (m *BudgetMeter) clone() *BudgetMeter {
	if m == nil {
		return nil
	}
	return &BudgetMeter{Windows: slices.Clone(m.Windows)}
}

// sameCall reports whether a call is the one these coordinates name.
func (c *LiveCall) sameCall(turnID, phase string, iteration int) bool {
	return c != nil && c.TurnID == turnID && c.Phase == phase && c.Iteration == iteration
}

// runningAs reports whether a call is the one these coordinates name and is
// still running: not a call its phase's failure froze ([recordPhaseFailure]).
//
// A FROZEN CALL IS AN ATTEMPT THAT ENDED, and its phase can run again under
// the same coordinates — the review of a parked turn's resume that is held or
// retried ([CallVersions]). That run is a call of its own: its start seeds a
// fresh placeholder, its rounds are not judged against the rounds the failed
// attempt reached, and it takes neither that attempt's prompt nor its clock.
// Read as the same call, the new run's start left the failed call on the seat
// and its first rounds were dropped as stale behind it, so the seat drew the
// earlier attempt's failure while its review ran again. No straggler of the
// failed attempt's own reaches the question: the state machine's reorder
// guard drops a start older than the failure, and [LiveState.applyProgress]'s
// straggler guard every round not newer than it.
func (c *LiveCall) runningAs(turnID, phase string, iteration int) bool {
	return c.sameCall(turnID, phase, iteration) && c.InProgress
}

// beginCall seeds a placeholder when a phase opens.
//
// The placeholder makes the live row appear the instant a phase starts;
// progress rounds then fill in the model, response and tool calls. RoundNum is
// -1 rather than 0 so the FIRST real round, round 0, is strictly newer than it
// — with 0 the opening round would read as a stale repeat of the placeholder
// and be dropped, and the row would sit empty until round 1.
func beginCall(env Envelope, payload map[string]any) *LiveCall {
	return &LiveCall{
		TurnID:         str(payload, "turn_id"),
		WorkKey:        str(payload, "work_key"),
		Phase:          str(payload, "phase"),
		Iteration:      num(payload, "iteration"),
		Trigger:        mapping(payload, "trigger"),
		PromptMessages: []any{},
		ToolExecutions: []any{},
		RoundNarration: []any{},
		Rounds:         []any{},
		Invocations:    []any{},
		WorkItem:       workItemOf(payload),
		Node:           str(payload, "node"),
		RoundNum:       openingRound,
		InProgress:     true,
		StartedAt:      env.Timestamp,
		UpdatedAt:      env.Timestamp,
	}
}

// openingRound is the RoundNum a phase's opening frame carries — the update
// published before its first provider call, and the only one carrying its
// prompt. -1 rather than 0 so the first real round is strictly newer; see
// [beginCall].
const openingRound = -1

// applyProgress folds one progress round into the in-flight call, returning the
// agent id of the seat whose call moved, or "" when the round was stale or
// named no seat.
func (s *LiveState) applyProgress(env Envelope, payload map[string]any) string {
	id := str(payload, "agent_id")
	if id == "" {
		return ""
	}
	// LOOKED UP, not created: a round the guards below discard moves nothing,
	// and an entry made for it would claim the projection knows the seat.
	agent := s.agents[id]
	var cur *LiveCall
	if agent != nil {
		cur = agent.liveCall
	}

	turnID := str(payload, "turn_id")
	phase := str(payload, "phase")
	iteration := num(payload, "iteration")
	roundNum := num(payload, "round_num")
	at := newStamp(env.Timestamp)

	// The phase already published its completion and this round is not
	// newer than it: a straggler that lost a cross-topic race, not live
	// work. Seeding a call from it would put a permanent in-flight row on
	// a finished phase. A phase that RUNS AGAIN under the same coordinates
	// — the review of a parked turn's resume that is held or retried
	// ([CallVersions]) — streams rounds newer than the completion the
	// attempt before it published, and they pass through. Keyed on turn id,
	// so a loop run outside the turn engine is never matched against an
	// unrelated one.
	if turnID != "" {
		if finished, ok := s.finishedCalls.get(callKey(turnID, phase, iteration)); ok {
			if at.empty() || !at.after(finished) {
				return ""
			}
		}
	}

	switch {
	case cur.runningAs(turnID, phase, iteration):
		// THE OPENING FRAME IS THE ONLY CARRIER OF THE PROMPT, and it
		// travels on a different subject from the rounds, so it can land
		// after one of them. Dropping it as stale would leave the call with
		// no prompt for its whole life — the one moment an operator most
		// wants to know what the model was actually told. It fills in what
		// it alone carries and moves nothing else: not the round, not the
		// tokens, not the clock.
		if roundNum == openingRound && cur.RoundNum > openingRound {
			was := *cur
			if cur.Prompt == "" {
				cur.Prompt = str(payload, "prompt")
			}
			if len(cur.PromptMessages) == 0 {
				cur.PromptMessages = list(payload, "prompt_messages")
			}
			// cur is the held call, so agent is the entry holding it.
			cur.Versions = s.restamp(&was, cur)
			agent.liveCallSeq = s.versions
			return id
		}
		// A stale earlier round of the SAME call is ignored.
		if roundNum < cur.RoundNum {
			return ""
		}
	case cur != nil && !at.empty() && newStamp(cur.UpdatedAt).after(at):
		// A different call, and the one held is newer.
		return ""
	}

	// Moved BELOW both guards. It used to run first, so a straggler that
	// lost the cross-topic race — a progress round arriving after its phase
	// had already completed — flipped the seat to "working" and then
	// returned "" at the guard. Nothing was pushed, so nothing ever
	// corrected it: the seat sat rendering as working, with no live call to
	// show, until the next real round. A round this function is about to
	// discard must not move the seat either.
	agent = s.ensureAgent(id)
	agent.working()
	s.touchTurn(agent, env, payload, StagePhase)
	if phase != "" {
		agent.currentPhase = phase
	}
	agent.currentIteration = iteration

	// Prefer the round's own trigger; fall back to the placeholder's,
	// seeded from the phase start, so the source never blanks out
	// mid-call.
	trigger := mapping(payload, "trigger")
	if trigger == nil && cur != nil {
		trigger = cur.Trigger
	}
	if trigger == nil {
		trigger = map[string]any{}
	}

	// CARRIED, like the trigger and for the same reason — and because the
	// prompt is now sent ONCE, on the opening frame. It never changes over
	// a phase and it is the largest thing on the event: republishing a
	// 30 KB system prompt five times a second, for the length of the phase,
	// to every open dashboard, was most of what a progress frame weighed.
	prompt := str(payload, "prompt")
	promptMessages := list(payload, "prompt_messages")
	if cur.runningAs(turnID, phase, iteration) {
		if prompt == "" {
			prompt = cur.Prompt
		}
		if len(promptMessages) == 0 {
			promptMessages = cur.PromptMessages
		}
	}

	// CARRIED, never restamped: the call started when it started, and a
	// round landing is not a new beginning. A progress round that arrives
	// before its own phase_started (different subjects, no ordering) opens
	// the clock instead.
	startedAt := env.Timestamp
	if cur.runningAs(turnID, phase, iteration) && cur.StartedAt != "" {
		startedAt = cur.StartedAt
	}

	agent.liveCall = &LiveCall{
		TurnID:         turnID,
		WorkKey:        str(payload, "work_key"),
		Phase:          phase,
		Iteration:      iteration,
		Trigger:        trigger,
		Model:          str(payload, "model"),
		Prompt:         prompt,
		PromptMessages: promptMessages,
		Response:       str(payload, "response"),
		InputTokens:    num(payload, "input_tokens"),
		OutputTokens:   num(payload, "output_tokens"),
		TotalTokens:    num(payload, "total_tokens"),
		ToolExecutions: list(payload, "tool_executions"),
		RoundNarration: list(payload, "round_narration"),
		PartialRound:   mapping(payload, "partial_round"),
		RoundNum:       roundNum,
		RoundsUsed:     roundNum + 1,
		// NOT carried: each frame states the whole list and the whole
		// cap, so an absent value is the frame saying there is none —
		// a closed round, a call that returned — and carrying the
		// previous one would leave a finished tool call on screen as
		// running.
		Rounds:         list(payload, "rounds"),
		Invocations:    list(payload, "invocations"),
		MaxRounds:      num(payload, "max_rounds"),
		RoundCeiling:   num(payload, "round_ceiling"),
		RoundStartedAt: str(payload, "round_started_at"),
		RunStartedAt:   str(payload, "run_started_at"),
		RunningCalls:   runningCalls(payload),
		// NOT carried, for Rounds' reason: every frame states the phase's
		// whole list.
		Steers:           list(payload, "steers"),
		CacheReadTokens:  num(payload, "cache_read_tokens"),
		CacheWriteTokens: num(payload, "cache_write_tokens"),
		// NOT carried, for Rounds' reason: every frame states the item the
		// turn is on now, and the queue stamps every frame's node.
		WorkItem:   workItemOf(payload),
		Node:       str(payload, "node"),
		InProgress: true,
		StartedAt:  startedAt,
		UpdatedAt:  env.Timestamp,
	}
	// The versions this round's fields are at: the held call's where a
	// field is what it held, the projection's next where it moved — and
	// every field at the next for a call of its own. A call its failure
	// froze under the same key is held here too: a version names a copy's
	// value, so a field the run again has equal to it keeps the version a
	// tab already holds that value at.
	var held *LiveCall
	if cur.sameCall(turnID, phase, iteration) {
		held = cur
	}
	agent.liveCall.Versions = s.restamp(held, agent.liveCall)
	agent.liveCallSeq = s.versions
	return id
}

// recordPhaseFailure remembers a failed phase and FREEZES its call rather than
// clearing it.
//
// A phase that dies mid-call is exactly when an operator most wants to see the
// call. Clearing it here would blank the row the moment the failure lands;
// instead it is stamped failed and kept, carrying whatever the phase managed —
// prompt, partial response, tool calls — with the error attached.
func (s *LiveState) recordPhaseFailure(agent *agentLive, env Envelope, payload map[string]any) {
	failure := failureOf(env, payload, str(payload, "phase"))
	agent.lastError = failure

	call := agent.liveCall
	if !call.sameCall(str(payload, "turn_id"), str(payload, "phase"), num(payload, "iteration")) {
		return
	}
	was := *call
	defer func() { call.Versions = s.restamp(&was, call); agent.liveCallSeq = s.versions }()
	call.InProgress = false
	call.Failed = true
	call.Error = failure
	if response := str(payload, "response"); response != "" {
		call.Response = response
	}
	if model := str(payload, "model"); model != "" {
		call.Model = model
	}
	if tools := list(payload, "tool_executions"); len(tools) > 0 {
		call.ToolExecutions = tools
	}
	// Frozen with the rest: a phase that died mid-round is exactly when the
	// model's last words matter, and dropping them here would leave the
	// failed row showing tool calls with nothing that asked for them.
	if narration := list(payload, "round_narration"); len(narration) > 0 {
		call.RoundNarration = narration
	}
	// And the runs, the failed one included: which harness run died, and on
	// whose entry, is the first question a failed phase is opened for.
	if runs := list(payload, "invocations"); len(runs) > 0 {
		call.Invocations = runs
	}
	// The round in flight is NOT frozen — it is over. A partial says "this
	// text is still arriving", and the last progress frame before a provider
	// died carries one for a round that will never commit. Left on the frozen
	// call it never goes away: the failure payload cannot displace it (a
	// snapshot holds committed rounds only), so a phase that died an hour ago
	// keeps rendering that round as streaming, with the running ring and a
	// blinking caret, on a card that also says it failed.
	call.PartialRound = nil
	// And for the same reason neither is a call or a round in flight: the
	// phase is over, and a frozen card still reading "running 2m" against
	// a tool call that will never return is the streaming caret again.
	call.RunningCalls = nil
	call.RoundStartedAt = ""
	call.RunStartedAt = ""
}

// failureOf is the seat's last error as a failed record of `phase` states it:
// its kind (`error` where it named none), its words, its turn and itself.
func failureOf(env Envelope, payload map[string]any, phase string) *ErrorInfo {
	kind := str(payload, "error_kind")
	if kind == "" {
		kind = "error"
	}
	return &ErrorInfo{
		Kind:    kind,
		Detail:  str(payload, "error"),
		Phase:   phase,
		TurnID:  str(payload, "turn_id"),
		At:      env.Timestamp,
		EventID: env.ID,
	}
}

// clearCall drops a seat's live call and advances its overlay sequence, so the
// clear is ordered against a call a push carries or a tab fetches whole that
// the clear overtook on the wire — see [Overlay.LiveCallSeq]. A no-op when
// there is nothing to clear, so a tab holding nothing has no sequence to move.
func (s *LiveState) clearCall(a *agentLive) {
	if a.liveCall == nil {
		return
	}
	a.liveCall = nil
	s.versions++
	a.liveCallSeq = s.versions
}

// finishLiveCall closes out the in-flight call when its phase completes.
//
// It records the call as finished — so no later progress round can resurrect it
// — and clears the live row. Only when the completed phase MATCHES the live
// call: a late completion for a prior phase must not wipe a newer phase's row.
func (s *LiveState) finishLiveCall(agent *agentLive, env Envelope, payload map[string]any) {
	turnID := str(payload, "turn_id")
	phase := str(payload, "phase")
	iteration := num(payload, "iteration")

	// Whatever the outcome, this phase is over: no later progress round
	// belongs to it. Recorded BEFORE the early returns below so it holds
	// for a failed phase, and for a completion that arrives with no live
	// call to clear.
	if turnID != "" {
		s.finishedCalls.put(callKey(turnID, phase, iteration), newStamp(env.Timestamp))
	}
	if agent.liveCall == nil {
		return
	}
	// A failed phase deliberately keeps its frozen call on screen. Only a
	// clean completion clears.
	if flag(payload, "failed") {
		return
	}
	if agent.liveCall.sameCall(turnID, phase, iteration) {
		s.clearCall(agent)
	}
}

// runningCalls reads the calls in flight a payload lists, or nil when it lists
// none.
//
// ONLY THE OBJECTS: each entry is masked key by key ([MaskCall] drops its
// arguments for a reader without the seat's body), and an entry that is not
// an object has no key to drop — kept, a producer's malformed entry would
// reach every reader whole.
func runningCalls(payload map[string]any) []any {
	var out []any
	for _, entry := range list(payload, "running_calls") {
		if call, ok := entry.(map[string]any); ok {
			out = append(out, call)
		}
	}
	return out
}

// workItemOf reads the `work_item` a payload names, or nil when it names none
// or names one with no id — an item is identified by its backend and id, and a
// key alone moves with the item.
func workItemOf(payload map[string]any) *types.WorkItem {
	m := mapping(payload, "work_item")
	if m == nil || str(m, "id") == "" {
		return nil
	}
	return &types.WorkItem{
		Backend: types.WorkBackend(str(m, "backend")),
		ID:      str(m, "id"),
		Key:     str(m, "key"),
		Project: str(m, "project"),
	}
}

// cloneItem copies an item, so an overlay handed to a caller shares nothing the
// projection holds.
func cloneItem(w *types.WorkItem) *types.WorkItem {
	if w == nil {
		return nil
	}
	dup := *w
	return &dup
}

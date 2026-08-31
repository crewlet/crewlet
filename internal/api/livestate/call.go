package livestate

// The in-flight call: seeded when a phase opens, folded on every progress
// round, frozen on failure, cleared on a clean completion.

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

func (m *Meter) clone() *Meter {
	if m == nil {
		return nil
	}
	dup := *m
	return &dup
}

// sameCall reports whether a call is the one these coordinates name.
func (c *LiveCall) sameCall(turnID, phase string, iteration int) bool {
	return c != nil && c.TurnID == turnID && c.Phase == phase && c.Iteration == iteration
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
		Phase:          str(payload, "phase"),
		Iteration:      num(payload, "iteration"),
		Trigger:        mapping(payload, "trigger"),
		PromptMessages: []any{},
		ToolExecutions: []any{},
		RoundNarration: []any{},
		RoundNum:       -1,
		InProgress:     true,
		UpdatedAt:      env.Timestamp,
	}
}

// applyProgress folds one progress round into the in-flight call, returning the
// role whose call moved or "" when the round was stale.
func (s *LiveState) applyProgress(env Envelope, payload map[string]any) string {
	role := str(payload, "role", "agent_role")
	if role == "" {
		return ""
	}
	agent := s.ensureAgent(role)
	if id := str(payload, "agent_id"); id != "" {
		agent.runtimeID = id
	}

	turnID := str(payload, "turn_id")
	phase := str(payload, "phase")
	iteration := num(payload, "iteration")
	roundNum := num(payload, "round_num")
	at := newStamp(env.Timestamp)

	// The phase already published its completion and this round is not
	// newer than it: a straggler that lost a cross-topic race, not live
	// work. Seeding a call from it would put a permanent in-flight row on
	// a finished phase. A RESUMED phase's rounds are newer than its
	// suspend checkpoint and pass through. Keyed on turn id, so a loop run
	// outside the turn engine is never matched against an unrelated one.
	if turnID != "" {
		if finished, ok := s.finishedCalls.get(callKey(turnID, phase, iteration)); ok {
			if at.empty() || !at.after(finished) {
				return ""
			}
		}
	}

	cur := agent.liveCall
	switch {
	case cur.sameCall(turnID, phase, iteration):
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
	if agent.state != "working" {
		agent.state = "working"
		agent.afkReason = ""
	}
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

	agent.liveCall = &LiveCall{
		TurnID:         turnID,
		Phase:          phase,
		Iteration:      iteration,
		Trigger:        trigger,
		Model:          str(payload, "model"),
		Prompt:         str(payload, "prompt"),
		PromptMessages: list(payload, "prompt_messages"),
		Response:       str(payload, "response"),
		InputTokens:    num(payload, "input_tokens"),
		OutputTokens:   num(payload, "output_tokens"),
		TotalTokens:    num(payload, "total_tokens"),
		ToolExecutions: list(payload, "tool_executions"),
		RoundNarration: list(payload, "round_narration"),
		PartialRound:   mapping(payload, "partial_round"),
		RoundNum:       roundNum,
		Rounds:         roundNum + 1,
		InProgress:     true,
		UpdatedAt:      env.Timestamp,
	}
	return role
}

// recordPhaseFailure remembers a failed phase and FREEZES its call rather than
// clearing it.
//
// A phase that dies mid-call is exactly when an operator most wants to see the
// call. Clearing it here would blank the row the moment the failure lands;
// instead it is stamped failed and kept, carrying whatever the phase managed —
// prompt, partial response, tool calls — with the error attached.
func (s *LiveState) recordPhaseFailure(agent *agentLive, env Envelope, payload map[string]any) {
	kind := str(payload, "error_kind")
	if kind == "" {
		kind = "error"
	}
	failure := &ErrorInfo{
		Kind:    kind,
		Message: str(payload, "error"),
		Phase:   str(payload, "phase"),
		TurnID:  str(payload, "turn_id"),
		At:      env.Timestamp,
		EventID: env.ID,
	}
	agent.lastError = failure

	call := agent.liveCall
	if !call.sameCall(str(payload, "turn_id"), str(payload, "phase"), num(payload, "iteration")) {
		return
	}
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
		agent.liveCall = nil
	}
}

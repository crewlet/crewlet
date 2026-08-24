package livestate

// applyBudget folds one meter report into the projection.
//
// Two guards, both load-bearing.
//
// A report from a DIFFERENT meter id replaces everything. The counters are a
// process-lifetime meter, so a restart legitimately zeroes them; merging, or
// taking a maximum, would pin a phantom high-water mark that no later report
// could ever clear.
//
// A report with a seq at or below the one held is dropped. Broker ordering
// holds only within a topic and the API reads a broadcast subscription across
// all of them, so an older report can arrive after a newer one and walk the
// meter backwards on screen.
func (s *LiveState) applyBudget(payload map[string]any) Change {
	var change Change
	meterID := str(payload, "meter_id")
	seq := num(payload, "seq")

	// Only a report from the SAME meter can be stale. A different meter id
	// is a different engine run, whose sequence numbers are unrelated to
	// the held one's — comparing them would let a restarted engine's first
	// report be refused as old.
	if meterID != "" && meterID == s.budget.MeterID && seq <= s.budget.Seq {
		return change
	}
	// Nothing is cleared here for a new meter, and that is deliberate
	// rather than an omission: every seat this report does not mention
	// loses its bar in the sweep at the end, which covers a new run and a
	// same-run seat that lost its cap with one rule. An extra clear here
	// would be a second implementation of the same decision, agreeing with
	// the first only for as long as nobody edits either.

	s.budget = OrgBudget{
		MeterID: meterID,
		Seq:     seq,
		Org: Meter{
			Used:      num(payload, "org_used_tokens"),
			Max:       num(payload, "org_max_tokens"),
			RefusedAt: str(payload, "org_refused_at"),
		},
	}
	change.Budget = true

	// Only metered seats are reported. A seat that LOST its meter — a cap
	// edited down to zero, a role decommissioned — must lose its bar
	// rather than keep the last figure it had.
	reported := map[string]struct{}{}
	for _, row := range list(payload, "agents") {
		fields, ok := row.(map[string]any)
		if !ok {
			continue
		}
		role := str(fields, "role")
		if role == "" {
			continue
		}
		reported[role] = struct{}{}
		agent := s.ensureAgent(role)
		if agent.runtimeID == "" {
			agent.runtimeID = str(fields, "agent_id")
		}
		agent.budget = &Meter{
			Used:      num(fields, "used_tokens"),
			Max:       num(fields, "max_tokens"),
			RefusedAt: str(fields, "refused_at"),
		}
		change.agentMoved(role)
	}
	for _, agent := range s.agents {
		if agent.budget != nil {
			if _, ok := reported[agent.role]; !ok {
				agent.budget = nil
				change.agentMoved(agent.role)
			}
		}
	}
	return change
}

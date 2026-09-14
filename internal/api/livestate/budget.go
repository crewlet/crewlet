package livestate

// applyBudget folds one meter report into the projection.
//
// Every node publishes a report of the SAME shared counter, each under its own
// meter id (the node's incarnation), so a report is a complete snapshot of the
// fleet's figures as one node read them at one moment. Three rules follow, and
// each is load-bearing.
//
// A report REPLACES what is held rather than merging with it. Merging, or
// taking a maximum, would pin a figure no later report could lower: an operator
// who resets a counter would watch the old number stay on screen.
//
// A report from the SAME meter with a seq at or below the one held is dropped.
// Broker ordering holds only within a topic and the API reads a broadcast
// subscription across all of them, so an older report can arrive after a newer
// one and walk the meter backwards. The seq is the guard within one meter
// because it is immune to that node's clock being stepped.
//
// A report from a DIFFERENT meter whose timestamp is older than the one held is
// dropped too. Sequence numbers of two nodes are unrelated, so comparing them
// would refuse a restarted node's first report as old; but accepting every
// other meter's report unconditionally let a delayed frame from one node land
// after a newer frame from another and walk the meter backwards just the same.
// Their reads are of one counter, so the later READ is the truer one, and the
// envelope timestamp is when it was read. Clock skew between nodes bounds how
// wrong that can be: a node ahead by a second holds the meter for a second.
func (s *LiveState) applyBudget(env Envelope, payload map[string]any) Change {
	var change Change
	meterID := str(payload, "meter_id")
	seq := num(payload, "seq")
	at := newStamp(env.Timestamp)

	switch {
	case meterID != "" && meterID == s.budget.MeterID:
		if seq <= s.budget.Seq {
			return change
		}
	case !at.empty() && !s.budgetAt.empty() && at.before(s.budgetAt):
		return change
	}
	// Nothing is cleared here for a new meter, and that is deliberate
	// rather than an omission: every seat this report does not mention
	// loses its bar in the sweep at the end, which covers a node on another
	// revision and a seat that lost its cap with one rule. An extra clear
	// here would be a second implementation of the same decision, agreeing
	// with the first only for as long as nobody edits either.

	s.budget = OrgBudget{
		MeterID: meterID,
		Seq:     seq,
		Org: Meter{
			Used: num(payload, "org_used_tokens"),
			Max:  num(payload, "org_max_tokens"),
		},
	}
	// Only an ADVANCE moves the guard. A report with no usable timestamp is
	// still applied, for the reason every other guard here lets one
	// through, but it must not erase the instant the next report is
	// compared against.
	if !at.empty() {
		s.budgetAt = at
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
			Used: num(fields, "used_tokens"),
			Max:  num(fields, "max_tokens"),
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

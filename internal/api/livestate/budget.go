package livestate

import (
	"encoding/json"

	"github.com/crewlet/crewlet/internal/events/types"
)

// applyBudget folds one meter report into the projection.
//
// Each node reports the SAME shared counter under its own meter id (its
// incarnation): a complete snapshot as that node read it. A report REPLACES what
// is held; merging or a maximum would pin a figure a turned-over window could
// never lower. A report from the SAME meter with a seq at or below the held one
// is dropped: ordering holds only within a topic, so an older report can arrive
// late, and seq is immune to a stepped clock. A report from a DIFFERENT meter
// older by timestamp is dropped too: two nodes' seqs are unrelated (comparing
// them would refuse a restarted node's first report), and the later READ of one
// counter is the truer. Clock skew bounds the error: a node a second ahead holds
// the meter a second.
func (s *LiveState) applyBudget(env Envelope, payload map[string]any) (change Change) {
	meterID := str(payload, "meter_id")
	seq := num(payload, "seq")
	at := newStamp(env.Timestamp)

	switch {
	case meterID != "" && s.budget != nil && meterID == s.budget.MeterID:
		if seq <= s.budget.Seq {
			return change
		}
	case !at.empty() && !s.budgetAt.empty() && at.before(s.budgetAt):
		return change
	}
	// DECODED INTO THE WIRE TYPE, not read field by field: the projection
	// holds the windows exactly as the frame carried them, because a fold
	// that picked named fields out of the payload is what dropped the
	// refusal stamp on its way to the push. A frame that does not decode
	// is not a reading, so it is dropped whole rather than half-applied.
	var report types.BudgetMeters
	if !decodePayload(payload, &report) {
		return change
	}
	// THE COMPANY'S WINDOWS STOP EVERY SEAT, so a report that turns the
	// org meter to refusing — or back — moves seats it never names.
	before := s.states()
	defer s.noteMoved(before, &change)
	// Nothing is cleared here for a new meter, and that is deliberate
	// rather than an omission: every seat this report does not mention
	// loses its bar in the sweep at the end, which covers a node on another
	// revision and a seat that lost its cap with one rule. An extra clear
	// here would be a second implementation of the same decision, agreeing
	// with the first only for as long as nobody edits either.

	s.budget = &OrgBudget{
		MeterID:  meterID,
		Seq:      seq,
		Timezone: report.Timezone,
		Org:      BudgetMeter{Windows: report.Org.Windows},
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
	// removed, a seat taken out of the chart — must lose its bar rather than keep
	// the last figure it had.
	reported := map[string]struct{}{}
	for _, seat := range report.Seats {
		// BY AGENT ID, the key every other event moves a seat under: a
		// meter keyed by the role name beside it drew one seat's bar on
		// every seat sharing its name. The role and handle the frame
		// carries are what the seat was called when it was read, and the
		// roster row this is merged onto names it as it is NOW.
		if seat.AgentID == "" {
			continue
		}
		reported[seat.AgentID] = struct{}{}
		agent := s.ensureAgent(seat.AgentID)
		agent.budget = &BudgetMeter{Windows: seat.Windows}
		change.agentMoved(seat.AgentID)
	}
	for _, agent := range s.agents {
		if agent.budget != nil {
			if _, ok := reported[agent.agentID]; !ok {
				agent.budget = nil
				change.agentMoved(agent.agentID)
			}
		}
	}
	return change
}

// decodePayload reads a generic payload into its wire type, reporting whether
// it decoded.
func decodePayload(payload map[string]any, into any) bool {
	raw, err := json.Marshal(payload)
	if err != nil {
		return false
	}
	return json.Unmarshal(raw, into) == nil
}

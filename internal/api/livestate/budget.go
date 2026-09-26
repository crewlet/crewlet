package livestate

import (
	"encoding/json"

	"github.com/crewlet/crewlet/internal/events/types"
)

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
func (s *LiveState) applyBudget(env Envelope, payload map[string]any) (change Change) {
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

	s.budget = OrgBudget{
		MeterID:  meterID,
		Seq:      seq,
		Timezone: report.Timezone,
		Org:      BudgetMeter{Windows: windowsOrEmpty(report.Org.Windows)},
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
	// removed, a role decommissioned — must lose its bar rather than keep
	// the last figure it had.
	reported := map[string]struct{}{}
	for _, seat := range report.Seats {
		if seat.Role == "" {
			continue
		}
		reported[seat.Role] = struct{}{}
		agent := s.ensureAgent(seat.Role)
		if agent.runtimeID == "" {
			agent.runtimeID = seat.AgentID
		}
		agent.budget = &BudgetMeter{Windows: windowsOrEmpty(seat.Windows)}
		change.agentMoved(seat.Role)
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

// windowsOrEmpty is a window list the push states as `[]` rather than null
// when a scope caps none: the client reads null as "not loaded yet".
func windowsOrEmpty(ws []WindowMeter) []WindowMeter {
	if ws == nil {
		return []WindowMeter{}
	}
	return ws
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

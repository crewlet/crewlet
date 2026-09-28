package tokens

import "slices"

// WHICH EVENTS CARRY SPEND, AND HOW ONE BECOMES A [Record], stated once.
//
// Two producers turn events into records — the event store, which promotes a
// record's dimensions into columns as the publish listener writes the row, and
// the live projection, which folds the same events as they stream — and both
// feed [Aggregate]. A rule written into each of them is two rules: the store
// and the projection each spelled the event types and the mapping of a coding
// run onto its phase and its coding agent, each comment saying it matched the
// other, and nothing held the two against each other. The first producer to
// learn a new spend event or a new fallback would have placed the same spend
// in a different row from the other, and a refresh — which replaces the live
// window with the store's — would have moved it on the reader's screen, which
// is the disagreement this package exists to remove.

// The two event types that carry spend. Gated on the TYPE rather than on "does
// the payload happen to have these fields", because several other events carry
// a `model`, a `turn_id` or a token count — agent_turn_completed sums its own
// phases — and a rollup that counted them would count spend twice, or calls
// that never happened.
const (
	// PhaseCompletedEvent is a completed phase: the engine's own model
	// calls, under the model that answered them.
	PhaseCompletedEvent = "agent_phase_completed"

	// RunUsageEvent is what one detached coding run spent in its box, one
	// record per launch, published at the collect — see [Record].
	RunUsageEvent = "sandbox_run_usage"
)

// SpendEvents is every event type a spend record is read from, in the order a
// statement binds them.
func SpendEvents() []string { return []string{PhaseCompletedEvent, RunUsageEvent} }

// IsSpendEvent reports whether an event of this type carries a spend record.
func IsSpendEvent(eventType string) bool {
	return slices.Contains(SpendEvents(), eventType)
}

// RunPhase is the phase a detached coding run's usage record is counted under:
// `execute`, because the executor is what launches a run — `run_sandbox` is its
// tool — and a run is its work continued in a box. The usage event names no
// phase; named here rather than imported from the phase vocabulary so this
// package stays a leaf.
const RunPhase = "execute"

// Fields is how a producer reads one field of a spend event's payload, in
// whatever form it holds the payload: the event store the shallow raw form it
// decodes on the publishing goroutine, the live projection the decoded map it
// already holds. Each reading yields the zero value for a field that is
// absent or of another type, so one malformed field costs that field and never
// the record — dropping the record would understate spend that happened.
type Fields interface {
	String(field string) string
	Int(field string) int
	Float(field string) float64
}

// Spent is the spend record an event of eventType carries, read through
// fields, and false for an event that carries none ([IsSpendEvent]).
//
// It fills every dimension the PAYLOAD states — the phase, the nesting, the
// model, the run and the unit of work, the token counts and the price — and
// leaves the three the producer owns: EventID and Timestamp are the
// envelope's, and AgentID and AgentRole are the store's tag rule on one side
// and the projection's seat identity on the other, a rule over every event
// type rather than over spend.
//
// A PHASE'S MODEL is its reported model, or where it names none the provider
// slot it ran on — the backfill in the event store's migration 0015 read the
// same fallback, so history and new rows agree on what "model" means. A
// CODING RUN stands under [RunPhase] with its coding agent as its model
// ([Record]); it carries no iteration, nesting or worker.
func Spent(eventType string, fields Fields) (Record, bool) {
	record := Record{
		TurnID:       fields.String("turn_id"),
		WorkKey:      fields.String("work_key"),
		InputTokens:  fields.Int("input_tokens"),
		OutputTokens: fields.Int("output_tokens"),
		TotalTokens:  fields.Int("total_tokens"),
		CostUSD:      fields.Float("cost_usd"),
	}
	switch eventType {
	case RunUsageEvent:
		record.Phase, record.Model = RunPhase, fields.String("coding_agent")
	case PhaseCompletedEvent:
		record.Phase = fields.String("phase")
		record.HostPhase = fields.String("host_phase")
		record.Worker = fields.String("worker")
		record.Iteration = fields.Int("iteration")
		if record.Model = fields.String("model"); record.Model == "" {
			record.Model = fields.String("provider_key")
		}
	default:
		return Record{}, false
	}
	return record, true
}

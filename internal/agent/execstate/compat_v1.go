package execstate

import (
	"encoding/json"
	"fmt"

	"github.com/crewlet/crewlet/internal/agent/ledger"
)

// The v1 reader, and it is PERMANENT.
//
// Not a migration window: a suspended run can sit parked for as long as its
// box's pause TTL allows and a person takes to answer, and there is no pass
// that rewrites parked rows — the blob is opaque to the sandbox layer that
// holds it, which is what keeps that layer free of agent imports. So the only
// thing that can read a v1 row is a build that still knows how, and the day
// this file is deleted is the day those runs become unresumable with no
// symptom but a refusal in a log.
//
// It reads only. Nothing writes v1 again, which is why there is no downgrade:
// a v1 build handed a v2 blob refuses it (see [Decode]), and refusing is
// correct — it would otherwise resume a turn believing every round before the
// suspend had called nothing.

// versionV1 is the format the three-phase turn engine wrote.
const versionV1 = 1

// iterationV1 is one round as the three-phase engine recorded it.
//
// TWO CALL LISTS, because two phases made calls and the delivery gate took a
// different view of each: the planner's recon and the executor's actions were
// separately addressable, and the gate reconciled the executor's against what
// the plan had NAMED. Nothing takes two views of one list any more, so the
// upgrade concatenates them in the order they ran.
type iterationV1 struct {
	Iteration    int           `json:"iteration"`
	PlanSummary  string        `json:"plan_summary,omitempty"`
	PlanCalls    []ledger.Call `json:"plan_tool_calls,omitempty"`
	ExecuteCalls []ledger.Call `json:"execute_tool_calls,omitempty"`

	Reads       []string `json:"read_only_names,omitempty"`
	ExecuteText string   `json:"execute_text,omitempty"`
	ReviewNotes string   `json:"review_notes,omitempty"`

	CompletedWork string `json:"completed_work,omitempty"`
}

// stateV1 is [State] with the v1 iteration shape.
//
// Every other field is spelled identically and carries the same meaning, which
// is what makes this an upgrade rather than a parallel format: the alias below
// borrows State's own tags so a field added to State is read out of a v1 blob
// too, and only the one list that genuinely changed is restated.
type stateV1 struct {
	stateFields
	Iterations []iterationV1 `json:"iteration_history"`
}

// stateFields is State without its own JSON identity, so stateV1 can embed the
// unchanged fields rather than copy them. A copy is how the two would come to
// disagree about a field only one of them was updated for.
type stateFields State

// upgradeV1 reads a v1 blob as the current state.
func upgradeV1(raw []byte) (State, error) {
	var v1 stateV1
	if err := json.Unmarshal(raw, &v1); err != nil {
		return State{}, fmt.Errorf("execstate: decode v1: %w", err)
	}
	out := State(v1.stateFields)
	out.Version = Version
	out.Iterations = make([]ledger.Iteration, 0, len(v1.Iterations))
	for _, rec := range v1.Iterations {
		out.Iterations = append(out.Iterations, ledger.Iteration{
			Iteration: rec.Iteration,
			// The PLAN SUMMARY becomes the intent, because that is what
			// it was: the planner's account of what the round set out to
			// do, which is now the executor's own.
			Intent: rec.PlanSummary,
			// Plan's calls FIRST — they ran first, and a ledger read as a
			// timeline is how the duplicate-delivery rule works. The
			// planner's recon really did fire before the executor acted.
			Calls:         append(append([]ledger.Call{}, rec.PlanCalls...), rec.ExecuteCalls...),
			Reads:         rec.Reads,
			Text:          rec.ExecuteText,
			ReviewNotes:   rec.ReviewNotes,
			CompletedWork: rec.CompletedWork,
		})
	}
	if len(out.Iterations) == 0 {
		out.Iterations = nil
	}
	return out, nil
}

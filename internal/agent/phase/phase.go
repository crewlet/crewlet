// Package phase holds the turn engine's per-phase vocabulary: which phase is
// running, which model it runs on, and what a phase concluded.
//
// The delivery question — did this turn actually reach anybody? — used to live
// here too, because it was asked of a PREDICTION the planner made and had to
// be reconciled against a separate phase's calls. With one loop there is no
// prediction, and the question is asked of the turn's own record instead:
// internal/agent/turn.
//
// # Resolution
//
// Which provider entries a seat's phase runs on is ONE resolution, [Resolve],
// which also names the level that decided it and the LLM fleet it runs on,
// so a page showing where a job lands reads the engine's answer, never a copy
// of its precedence. A fleet's member for each piece of work is its
// balancer's pick ([Registry.Chain], internal/providers/llm/fleet).
package phase

// Phase names one pass of the turn engine.
type Phase string

// The phases. Execute and Review are the turn's own two passes; Subagent,
// Auxiliary, Judge and Triage are the scopes a seat's work spills into, each with its
// own provider chain so an operator can point cheap work at a cheap model.
//
// `execute` names the phase although it now decides as well as acts:
// deciding is part of the seat doing its own work, which is what every
// dashboard and rollup that reads the value means by it. The executor runs
// as its harness's own run, in a box, and what each launch of that run
// spent is a record of the event vocabulary (events/types.ExecutorCollected)
// filed under Execute — no phase of either vocabulary, and no provider chain
// resolved for it, because the run IS the Execute chain's member that
// launched it.
const (
	Execute   Phase = "execute"
	Review    Phase = "review"
	Subagent  Phase = "subagent"
	Auxiliary Phase = "auxiliary"
	Judge     Phase = "judge"
	// Onboarding is the dedicated first-turn pass, BEFORE the executor.
	// Its own phase rather than a hint inside the executor's prompt,
	// because on a genuine first turn reading the team's pages and
	// persisting conventions consumed the whole round budget and could
	// starve the executor's own submission entirely.
	Onboarding Phase = "onboarding"
	// Triage decides one wake to a busy seat from somebody other than its
	// running run's requester (ADR-0168): a one-tool structured decision on
	// the engine host. Its own chain because it runs on every such wake and
	// decides only a label, so an operator points it at a cheap model.
	Triage Phase = "triage"
)

func (p Phase) String() string { return string(p) }

// Decision is what a turn concluded.
type Decision string

const (
	// Done — the work is finished and the turn ends.
	Done Decision = "done"
	// SelfIterate — loop back with a correction.
	SelfIterate Decision = "self_iterate"
	// Failed — the turn ended without delivering and will not retry.
	Failed Decision = "failed"
	// Skipped — nobody was asking this seat to do anything.
	Skipped Decision = "skipped"
)

func (d Decision) String() string { return string(d) }

// ReviewDecisions are the decisions a REVIEWER may submit, which is not every
// decision this package defines.
//
// [Skipped] is absent: a skip is the engine's own reading of a round nobody
// was waiting on, taken before the reviewer is called, so a reviewer reaching
// it would mean the turn ran after deciding not to.
//
// One list serves the submission schema's `enum` and the decoder's validation:
// two spellings could disagree unnoticed, and a schema offering a value the
// decoder refuses bounces a model that did as told, which cannot see the
// lists differ.
func ReviewDecisions() []Decision {
	// Returned fresh, never a package-level slice: a shared backing array is
	// one caller's append away from rewriting everybody else's list.
	return []Decision{Done, SelfIterate, Failed}
}

// Valid reports whether d is one of the four this package defines.
//
// EMPTY IS NOT VALID here, unlike the tool-choice and transport enums: every
// producer of a Decision has concluded something, and the absent-field default
// belongs where the wire shape is decoded — [runner] reads a missing review
// decision as Done, for a reason it states — not in a predicate that would let
// an unset field travel as if it meant something.
func (d Decision) Valid() bool {
	switch d {
	case Done, SelfIterate, Failed, Skipped:
		return true
	default:
		return false
	}
}

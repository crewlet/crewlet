// Package phase holds the turn engine's per-phase vocabulary: which phase is
// running, which model it runs on, and what a phase concluded.
//
// The delivery question — did this turn actually reach anybody? — used to live
// here too, because it was asked of a PREDICTION the planner made and had to
// be reconciled against a separate phase's calls. With one loop there is no
// prediction, and the question is asked of the turn's own record instead:
// internal/agent/turn.
package phase

// Phase names one pass of the turn engine.
type Phase string

// The phases. Execute and Review are the turn's own two passes; Subagent,
// Auxiliary, Judge and Sandbox are the scopes a seat's work spills into, each
// with its own provider chain so an operator can point cheap work at a cheap
// model.
//
// `execute` keeps its wire string although the phase it names now decides as
// well as acts. The value is written into the event store, backfilled into
// two columns by an applied migration, and read by every dashboard and
// rollup; renaming it would buy a better word at the cost of a value
// migration and a mixed fleet reporting two names for one thing.
const (
	Execute   Phase = "execute"
	Review    Phase = "review"
	Subagent  Phase = "subagent"
	Auxiliary Phase = "auxiliary"
	Judge     Phase = "judge"
	Sandbox   Phase = "sandbox"
	// Onboarding is the dedicated first-turn pass, BEFORE the executor.
	// Its own phase rather than a hint inside the executor's prompt,
	// because on a genuine first turn reading the team's pages and
	// persisting conventions consumed the whole round budget and could
	// starve the executor's own submission entirely.
	Onboarding Phase = "onboarding"
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

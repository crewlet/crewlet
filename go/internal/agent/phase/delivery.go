// Package phase holds the turn engine's per-phase vocabulary: which phase is
// running, which model it runs on, and the one question a phase's outcome
// turns on — did it actually DELIVER?
package phase

import "slices"

// Phase names one pass of the turn engine.
type Phase string

const (
	Plan      Phase = "plan"
	Execute   Phase = "execute"
	Review    Phase = "review"
	Subagent  Phase = "subagent"
	Auxiliary Phase = "auxiliary"
	Judge     Phase = "judge"
	Sandbox   Phase = "sandbox"
)

func (p Phase) String() string { return string(p) }

// Delivery describes what a phase was asked to do and what it did, so the
// question below has one set of inputs rather than four call sites gathering
// their own.
type Delivery struct {
	// Called is every tool the phase actually invoked.
	Called []string

	// PlannedResolved is the subset of the plan's tools_needed that
	// RESOLVE in the phase's catalogue. Names that do not resolve are
	// excluded here on purpose — see Delivered.
	PlannedResolved []string

	// MCPTools is every tool backed by an MCP server on this surface.
	MCPTools []string

	// KnownReads is every tool POSITIVELY annotated read-only. Positively
	// is the operative word: an unannotated tool is not a known read, and
	// treating it as one exempts every unannotated tool from the fence.
	KnownReads []string
}

// Delivered answers whether a phase performed the action its plan implies.
//
// Two rules, and the second exists because the first cannot always apply:
//
//   - NAME-PRECISE when the planner named tools that resolve in the
//     catalogue: the exact tool must have been called.
//   - Otherwise the planner named ONLY PHANTOMS — MCP tool names it cannot
//     see and guessed wrong — so there is nothing to name-match against.
//     Delivered iff the phase called a SERVER-BACKED MCP TOOL that is not a
//     positively-known read: the real delivery tool it discovered, whatever
//     its name turned out to be.
//
// Requiring server-backed is the whole point. A delivery to a shared surface
// only ever comes from an MCP server, so a first-party builtin called during
// recon never counts, and neither does an explicit read.
//
// The pair of failures this balances between, both of which have happened:
//
//   - Too strict, and a real delivery through a discovered MCP tool reads as
//     undelivered, the done→self_iterate override fires, and the agent posts
//     the same message twice.
//   - Too loose, and a plan whose only delivery tool was a wrong guess, which
//     then called nothing but a builtin, reads as delivered — so "reply hi"
//     produces text, never calls Slack, and completes silently having acted
//     on nothing.
func Delivered(d Delivery) bool {
	if len(d.PlannedResolved) > 0 {
		for _, name := range d.Called {
			if slices.Contains(d.PlannedResolved, name) {
				return true
			}
		}
		return false
	}
	for _, name := range d.Called {
		if slices.Contains(d.MCPTools, name) && !slices.Contains(d.KnownReads, name) {
			return true
		}
	}
	return false
}

// Phantoms returns the planned tool names that do not resolve in the
// catalogue, so a caller can report them.
//
// Worth surfacing rather than silently ignoring: a planner naming tools that
// do not exist is usually guessing at an MCP surface it cannot see, and that
// is the signal that its prompt is missing the catalogue rather than that the
// model is bad at planning.
func Phantoms(planned, catalogue []string) []string {
	var out []string
	for _, name := range planned {
		if !slices.Contains(catalogue, name) {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// ResolvePlanned splits a plan's tools_needed into the names that resolve in
// the catalogue and those that do not.
//
// The split matters because the two halves answer DIFFERENT questions and the
// engine reads them separately: Delivered keys off the RESOLVED half, while
// the phase's INTENT — whether the plan meant to act at all — keys off the raw
// list. A plan naming only phantoms still intended to deliver, and treating it
// as intending nothing is how a failed delivery becomes a successful turn.
func ResolvePlanned(planned, catalogue []string) (resolved, phantoms []string) {
	for _, name := range planned {
		if slices.Contains(catalogue, name) {
			resolved = append(resolved, name)
		} else {
			phantoms = append(phantoms, name)
		}
	}
	slices.Sort(resolved)
	slices.Sort(phantoms)
	return resolved, phantoms
}

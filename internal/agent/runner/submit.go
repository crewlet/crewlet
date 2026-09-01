// Package runner drives the three phases against real models and real tools.
//
// It is the wiring between four things that each know nothing about the
// others: the prompt builder, the tool registry, the provider chain, and the
// turn loop's [turn.Phases] contract. Everything here is plumbing plus the one
// rule plumbing cannot express: what happens when the model never gives a
// structured answer at all.
//
// What a structured answer IS, and the three rules for capturing one, are
// [github.com/crewlet/crewlet/internal/agent/structured]'s. What counts as a
// VALID one stays here, in the decoders below, because that belongs to the
// phase doing the asking.
package runner

import (
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/structured"
	"github.com/crewlet/crewlet/internal/agent/turn"
)

// The names of the two structured-output tools. A phase ENDS by calling its
// own; that is what turns free prose into a decision the engine can act on.
const (
	SubmitPlanTool   = "submit_plan"
	SubmitReviewTool = "submit_review"
)

// planPayload is the wire shape of a submitted plan.
type planPayload struct {
	Decision        string   `json:"decision"`
	Reasoning       string   `json:"reasoning"`
	Steps           []step   `json:"steps"`
	ToolsNeeded     []string `json:"tools_needed"`
	SuccessCriteria []string `json:"success_criteria"`
}

type step struct {
	Intent    string   `json:"intent"`
	Approach  string   `json:"approach"`
	Tools     []string `json:"tools"`
	OnFailure string   `json:"on_failure"`
}

func decodePlan(args map[string]any) (planPayload, error) {
	var p planPayload
	if err := structured.Remarshal(args, &p); err != nil {
		return p, err
	}
	switch turn.PlanDecision(p.Decision) {
	case turn.PlanRun, turn.PlanDirect, turn.PlanSkip:
	case "":
		// An absent decision is `plan`, not an error. It is the common
		// case and the one every other field is written for; failing here
		// would reject a complete plan over its most predictable omission.
		p.Decision = string(turn.PlanRun)
	default:
		return p, fmt.Errorf("decision must be one of plan, direct or skip, got %q", p.Decision)
	}
	if turn.PlanDecision(p.Decision) != turn.PlanSkip && len(p.ToolsNeeded) == 0 && len(p.Steps) == 0 {
		// A plan that names no tools and lists no steps has decided
		// nothing. Saying so is what makes the model try again inside the
		// phase, instead of Execute receiving an empty plan and
		// improvising against the full surface.
		return p, fmt.Errorf("a %q decision needs steps or tools_needed", p.Decision)
	}
	return p, nil
}

// Summary renders the plan as the prose Execute and Review read.
//
// It carries each step's APPROACH as well as its intent, because the planner
// may have pre-composed the exact content Execute should produce — the reply
// text, the comment body — and Execute cannot see what Plan saw. Dropping the
// approach makes Execute re-derive data the planner already gathered, or
// invent it.
func (p planPayload) Summary() string {
	if turn.PlanDecision(p.Decision) == turn.PlanSkip {
		if p.Reasoning != "" {
			return "(skip) " + p.Reasoning
		}
		return "(skip: not addressed to me)"
	}
	if len(p.Steps) == 0 {
		if p.Reasoning != "" {
			return p.Reasoning
		}
		if turn.PlanDecision(p.Decision) == turn.PlanDirect {
			return "(direct: no explicit plan; the executor improvises)"
		}
		return ""
	}
	var b strings.Builder
	for i, s := range p.Steps {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "%d. %s", i+1, s.Intent)
		if s.Approach != "" {
			b.WriteString("\n   " + s.Approach)
		}
	}
	return b.String()
}

// reviewPayload is the wire shape of a submitted review.
type reviewPayload struct {
	Decision      string `json:"decision"`
	Notes         string `json:"notes"`
	CompletedWork string `json:"completed_work"`
	FinalArtifact string `json:"final_artifact"`
}

func decodeReview(args map[string]any) (reviewPayload, error) {
	var r reviewPayload
	if err := structured.Remarshal(args, &r); err != nil {
		return r, err
	}
	switch r.Decision {
	case "done", "self_iterate", "failed":
	case "":
		// An absent decision is `done`. The alternative — defaulting to
		// self_iterate — spends another whole round on a review that
		// simply forgot a field, and the delivery gate still overturns a
		// `done` that delivered nothing.
		r.Decision = "done"
	default:
		return r, fmt.Errorf("decision must be one of done, self_iterate or failed, got %q", r.Decision)
	}
	if r.Decision == "self_iterate" && strings.TrimSpace(r.Notes) == "" {
		// A loop-back with no correction sends the next round to do
		// exactly what the last one did. The stall guard would eventually
		// catch it, but only after spending the rounds.
		return r, fmt.Errorf("self_iterate needs notes saying what the next round should do differently")
	}
	return r, nil
}

// Package runner drives the three phases against real models and real tools.
//
// It is the wiring between four things that each know nothing about the
// others: the prompt builder, the tool registry, the provider chain, and the
// turn loop's [turn.Phases] contract. Everything here is plumbing plus the two
// rules plumbing cannot express — how a phase's structured answer is
// extracted, and what happens when the model never gives one.
package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/tools"
)

// The names of the two structured-output tools. A phase ENDS by calling its
// own; that is what turns free prose into a decision the engine can act on.
const (
	SubmitPlanTool   = "submit_plan"
	SubmitReviewTool = "submit_review"
)

// submitted is the captured payload of a structured-output call.
//
// The tool does not DO anything: it validates, records, and reports back. The
// phase's answer is the arguments it was called with, so the loop's ordinary
// tool machinery carries it out and nothing needs a side channel.
type submitted[T any] struct {
	name   string
	desc   string
	schema map[string]any
	value  T
	called bool
	decode func(map[string]any) (T, error)
}

func (s *submitted[T]) Name() string               { return s.name }
func (s *submitted[T]) Description() string        { return s.desc }
func (s *submitted[T]) Parameters() map[string]any { return s.schema }

func (s *submitted[T]) Call(_ context.Context, args map[string]any) (tools.Result, error) {
	v, err := s.decode(args)
	if err != nil {
		// A failed validation goes BACK TO THE MODEL rather than ending
		// the phase. It is the one tool failure the model can reliably
		// fix, and refusing the turn over a malformed submission throws
		// away everything the phase already did.
		//nolint:nilerr // Deliberate: see the paragraph above.
		return tools.Result{Output: "Invalid submission: " + err.Error(), Failed: true}, nil
	}
	// LAST WRITE WINS, and the call is still accepted. A model that
	// submits twice has corrected itself; rejecting the second submission
	// leaves the engine acting on the draft the model just replaced.
	s.value = v
	s.called = true
	return tools.Result{Output: "submitted"}, nil
}

// Value returns the captured submission and whether one arrived.
func (s *submitted[T]) Value() (T, bool) { return s.value, s.called }

// planPayload is the wire shape of a submitted plan.
type planPayload struct {
	// TYPED, not a bare string. It was converted to turn.PlanDecision at
	// six call sites, one of them a value method a caller can forget —
	// and a comparison against an untyped literal is exactly what survives
	// a constant being renamed.
	Decision        turn.PlanDecision `json:"decision"`
	Reasoning       string            `json:"reasoning"`
	Steps           []step            `json:"steps"`
	ToolsNeeded     []string          `json:"tools_needed"`
	SuccessCriteria []string          `json:"success_criteria"`
}

type step struct {
	Intent    string   `json:"intent"`
	Approach  string   `json:"approach"`
	Tools     []string `json:"tools"`
	OnFailure string   `json:"on_failure"`
}

func decodePlan(args map[string]any) (planPayload, error) {
	var p planPayload
	if err := remarshal(args, &p); err != nil {
		return p, err
	}
	switch {
	case p.Decision == "":
		// An absent decision is `plan`, not an error. It is the common
		// case and the one every other field is written for; failing here
		// would reject a complete plan over its most predictable omission.
		p.Decision = turn.PlanRun
	case !p.Decision.Valid():
		return p, fmt.Errorf("decision must be one of %s, %s or %s, got %q",
			turn.PlanRun, turn.PlanDirect, turn.PlanSkip, p.Decision)
	}
	if p.Decision != turn.PlanSkip && len(p.ToolsNeeded) == 0 && len(p.Steps) == 0 {
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
	if p.Decision == turn.PlanSkip {
		if p.Reasoning != "" {
			return "(skip) " + p.Reasoning
		}
		return "(skip: not addressed to me)"
	}
	if len(p.Steps) == 0 {
		if p.Reasoning != "" {
			return p.Reasoning
		}
		if p.Decision == turn.PlanDirect {
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
	Decision      phase.Decision `json:"decision"`
	Notes         string         `json:"notes"`
	CompletedWork string         `json:"completed_work"`
	FinalArtifact string         `json:"final_artifact"`
}

func decodeReview(args map[string]any) (reviewPayload, error) {
	var r reviewPayload
	if err := remarshal(args, &r); err != nil {
		return r, err
	}
	// AGAINST THE CONSTANTS, never their spelling. This switch listed
	// "done", "self_iterate" and "failed" as literals, so renaming one of
	// them left this accepting a value nothing else produces — and phase
	// owns those names.
	//
	// phase.Skipped is deliberately absent: only a PLAN may conclude that
	// nobody was asking, and a review reaching it would mean the turn ran
	// after deciding not to.
	switch {
	case r.Decision == "":
		// An absent decision is `done`. The alternative — defaulting to
		// self_iterate — spends another whole round on a review that
		// simply forgot a field, and the delivery gate still overturns a
		// `done` that delivered nothing.
		r.Decision = phase.Done
	case r.Decision != phase.Done && r.Decision != phase.SelfIterate && r.Decision != phase.Failed:
		return r, fmt.Errorf("decision must be one of %s, %s or %s, got %q",
			phase.Done, phase.SelfIterate, phase.Failed, r.Decision)
	}
	if r.Decision == phase.SelfIterate && strings.TrimSpace(r.Notes) == "" {
		// A loop-back with no correction sends the next round to do
		// exactly what the last one did. The stall guard would eventually
		// catch it, but only after spending the rounds.
		return r, fmt.Errorf("self_iterate needs notes saying what the next round should do differently")
	}
	return r, nil
}

// remarshal moves a decoded argument map into a typed struct.
//
// Via JSON rather than field-by-field because the arguments already came off
// the wire as JSON and the struct tags are the schema — one definition of the
// mapping instead of two that can disagree. json.Number survives the round
// trip, so a large id in a plan's own arguments stays exact.
func remarshal(args map[string]any, into any) error {
	blob, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("arguments could not be re-encoded: %w", err)
	}
	if err := json.Unmarshal(blob, into); err != nil {
		return fmt.Errorf("arguments do not match the schema: %w", err)
	}
	return nil
}

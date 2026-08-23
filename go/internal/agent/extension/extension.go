// Package extension holds the round-cap extension judge: when a phase runs out
// of tool rounds, a cheap model decides whether it is making progress or
// thrashing, and the engine grants more rounds or falls through to the rescue
// path.
//
// THE ARITHMETIC IS SEPARATED FROM THE JUDGE. How many rounds a decision is
// worth — clamped by the step size, by the headroom under the phase ceiling,
// and by a judge that says extend while asking for nothing — is pure policy,
// and it is the part that is easy to get wrong and impossible to exercise
// through a live model. It is a value type here, so it is tested directly.
package extension

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
)

// Decision is the judge's answer.
type Decision struct {
	// Extend is false for a rescue: the phase is looping or stuck and more
	// rounds would only cost more tokens.
	Extend bool

	// Reason is the judge's own wording, carried into the nudge so the
	// phase learns why it was given more rope.
	Reason string

	// AdditionalRounds is what the judge asked for. Advisory: the engine
	// clamps it, and a judge that says extend while asking for zero gets
	// the step rather than nothing — see [Policy.Grant].
	AdditionalRounds int
}

// Rescue builds a refusal carrying why the engine did not even ask.
func Rescue(reason string) Decision { return Decision{Reason: reason} }

// Request is what a judge is shown.
type Request struct {
	Phase phase.Phase

	Task        string
	PlanSummary string

	// Calls is the phase's tool log — the evidence. A judge shown only the
	// narration cannot tell progress from a model saying it is making
	// progress.
	Calls []ledger.Call

	LastText   string
	RoundsUsed int

	// MaxStep is the most this one call may grant, already clamped to the
	// headroom. Told to the judge so it does not ask for what it cannot
	// have and then read the clamp as the engine ignoring it.
	MaxStep int

	// RemainingUnderCeiling is what is left for the whole phase across all
	// extensions.
	RemainingUnderCeiling int
}

// Judge is the model-facing half.
type Judge interface {
	Decide(ctx context.Context, req Request) (Decision, error)
}

// Policy is the pure half: whether to ask at all, and what an answer is worth.
type Policy struct {
	// Enabled is the master switch. Off means every exhaustion goes
	// straight to rescue.
	Enabled bool

	// RoundStep caps what ONE judge call may grant. Repeated exhaustion
	// during an extended run calls the judge again, so this is per
	// extension rather than per turn — which is what stops a single
	// optimistic answer from spending a phase's whole ceiling at once.
	RoundStep int

	// Ceiling is the phase's hard cap across every extension. Setting it
	// equal to the phase's base round count disables extensions for that
	// phase alone.
	Ceiling int
}

// Headroom is how many rounds remain under the ceiling after used rounds.
// Never negative: a phase that somehow overran its ceiling has no headroom,
// not negative headroom that would flip a comparison somewhere downstream.
func (p Policy) Headroom(used int) int {
	if h := p.Ceiling - used; h > 0 {
		return h
	}
	return 0
}

// Step is the most one judge call may grant right now.
func (p Policy) Step(used int) int {
	headroom := p.Headroom(used)
	if headroom <= 0 {
		return 0
	}
	if p.RoundStep < 1 {
		// A misconfigured step must not mean "grant nothing" — that
		// silently disables extensions while the master switch reads on.
		// One round is the smallest grant that is still a grant.
		return min(1, headroom)
	}
	return min(p.RoundStep, headroom)
}

// ShouldAsk reports whether it is worth calling the judge at all, and why not
// when it is not. Asking costs a model call, so the two conditions that make
// the answer irrelevant are checked first.
func (p Policy) ShouldAsk(used int) (bool, Decision) {
	if !p.Enabled {
		return false, Rescue("extension_disabled")
	}
	if p.Headroom(used) <= 0 {
		return false, Rescue("ceiling_reached")
	}
	return true, Decision{}
}

// Grant turns a judge's answer into a round count.
//
// A judge that chose extend but asked for nothing gets the step. The
// alternative — granting zero — produces an "extension" that runs no rounds,
// exhausts again immediately, and calls the judge a second time to be told the
// same thing: a loop that burns two model calls per round of nothing.
func (p Policy) Grant(d Decision, used int) int {
	if !d.Extend {
		return 0
	}
	step := p.Step(used)
	if step <= 0 {
		return 0
	}
	want := d.AdditionalRounds
	if want < 1 {
		want = step
	}
	return min(want, step)
}

// FinishHint is the closing line of the nudge, and it is phase-specific
// because the phases END differently.
//
// Plan exits by calling its submission tool; Execute exits by returning text
// with no tool calls. A phase told the wrong way to finish spends its granted
// rounds trying to exit through a door it does not have, which is the exact
// failure the extension was granted to avoid.
func FinishHint(ph phase.Phase) string {
	switch ph {
	case phase.Plan:
		return "If you cannot finish in this window, call `submit_plan` with " +
			"whatever you have so far — a partial plan is better than no plan."
	default:
		return "If you cannot finish in this window, stop calling tools and " +
			"return a short plain-text summary of what is done and what is " +
			"outstanding."
	}
}

// Nudge is the message handed to a phase that has been granted more rounds.
func Nudge(ph phase.Phase, granted int, reason string) string {
	if reason == "" {
		reason = "(none)"
	}
	plural := "s"
	if granted == 1 {
		plural = ""
	}
	return fmt.Sprintf(
		"You have been granted %d additional tool-call round%s because the "+
			"extension judge sees you making progress.\nJudge's reason: %s\n"+
			"Use them efficiently to finish the task. %s",
		granted, plural, reason, FinishHint(ph))
}

// Consider runs one judge call and returns what it is worth.
//
// One call, not the loop: the caller owns "extend repeatedly until the phase
// finishes or the ceiling is reached", because only the caller knows what
// finishing looks like for its phase.
//
// A judge that ERRORS rescues rather than propagating. The extension is an
// optimisation on a phase that has already run out of rounds; failing the turn
// because the optional cheap model that decides whether to be generous was
// unreachable would turn a degraded outcome into no outcome.
func Consider(ctx context.Context, j Judge, p Policy, req Request) (granted int, d Decision) {
	if ok, refusal := p.ShouldAsk(req.RoundsUsed); !ok {
		return 0, refusal
	}
	req.MaxStep = p.Step(req.RoundsUsed)
	req.RemainingUnderCeiling = p.Headroom(req.RoundsUsed)
	if j == nil {
		return 0, Rescue("no_judge")
	}
	decision, err := j.Decide(ctx, req)
	if err != nil {
		return 0, Rescue("judge_failed: " + err.Error())
	}
	return p.Grant(decision, req.RoundsUsed), decision
}

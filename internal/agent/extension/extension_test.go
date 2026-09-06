package extension_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/extension"
	"github.com/crewlet/crewlet/internal/agent/phase"
)

type judge struct {
	d    extension.Decision
	err  error
	seen extension.Request
	n    int
}

func (j *judge) Decide(_ context.Context, req extension.Request) (extension.Decision, error) {
	j.n++
	j.seen = req
	return j.d, j.err
}

func policy() extension.Policy {
	return extension.Policy{Enabled: true, RoundStep: 5, Ceiling: 40}
}

func TestHeadroomNeverGoesNegative(t *testing.T) {
	t.Parallel()
	// A phase that somehow overran its ceiling has NO headroom. Negative
	// headroom would flip a comparison downstream and read as a grant.
	p := policy()
	if got := p.Headroom(20); got != 20 {
		t.Errorf("headroom = %d, want 20", got)
	}
	for _, used := range []int{40, 41, 500} {
		if got := p.Headroom(used); got != 0 {
			t.Errorf("headroom at %d used = %d, want 0", used, got)
		}
	}
}

func TestTheStepIsClampedByBothTheConfigAndTheHeadroom(t *testing.T) {
	t.Parallel()
	p := policy()
	if got := p.Step(20); got != 5 {
		t.Errorf("step = %d, want the configured 5", got)
	}
	// Near the ceiling the headroom wins, or one grant overshoots the cap
	// the ceiling exists to enforce.
	if got := p.Step(38); got != 2 {
		t.Errorf("step near the ceiling = %d, want the remaining 2", got)
	}
	if got := p.Step(40); got != 0 {
		t.Errorf("step at the ceiling = %d, want 0", got)
	}
}

func TestAMisconfiguredStepStillGrantsSomething(t *testing.T) {
	t.Parallel()
	// A zero step must not silently disable extensions while the master
	// switch still reads on — that is a setting that does nothing and says
	// nothing, which is the worst kind.
	p := extension.Policy{Enabled: true, RoundStep: 0, Ceiling: 40}
	if got := p.Step(10); got != 1 {
		t.Errorf("step = %d, want 1", got)
	}
	// But never past the ceiling even so.
	if got := (extension.Policy{Enabled: true, Ceiling: 5}).Step(5); got != 0 {
		t.Errorf("step at the ceiling = %d, want 0", got)
	}
}

func TestTheJudgeIsNotAskedWhenTheAnswerCannotMatter(t *testing.T) {
	t.Parallel()
	// Asking costs a model call.
	j := &judge{d: extension.Decision{Extend: true, AdditionalRounds: 5}}

	off := extension.Policy{Enabled: false, RoundStep: 5, Ceiling: 40}
	granted, d := extension.Consider(context.Background(), j, off, extension.Request{RoundsUsed: 10})
	if granted != 0 || d.Extend {
		t.Errorf("a disabled policy granted %d rounds", granted)
	}
	if d.Reason != "extension_disabled" {
		t.Errorf("reason = %q, want extension_disabled", d.Reason)
	}

	granted, d = extension.Consider(context.Background(), j, policy(), extension.Request{RoundsUsed: 40})
	if granted != 0 {
		t.Errorf("a consumed ceiling granted %d rounds", granted)
	}
	if d.Reason != "ceiling_reached" {
		t.Errorf("reason = %q, want ceiling_reached", d.Reason)
	}

	if j.n != 0 {
		t.Errorf("the judge was called %d times when the answer could not matter", j.n)
	}
}

func TestAGrantIsClampedToTheStep(t *testing.T) {
	t.Parallel()
	// A single optimistic answer must not spend the phase's whole ceiling.
	// Repeated exhaustion calls the judge again, which is the point.
	j := &judge{d: extension.Decision{Extend: true, AdditionalRounds: 999}}
	granted, _ := extension.Consider(context.Background(), j, policy(), extension.Request{RoundsUsed: 10})
	if granted != 5 {
		t.Errorf("granted = %d, want the step of 5", granted)
	}
	// And a modest ask is honoured rather than rounded up.
	j.d = extension.Decision{Extend: true, AdditionalRounds: 2}
	if granted, _ := extension.Consider(context.Background(), j, policy(), extension.Request{RoundsUsed: 10}); granted != 2 {
		t.Errorf("granted = %d, want the requested 2", granted)
	}
}

func TestExtendWithNoNumberGetsTheStep(t *testing.T) {
	t.Parallel()
	// Granting zero produces an "extension" that runs no rounds, exhausts
	// immediately, and calls the judge again to be told the same thing — a
	// loop burning two model calls per round of nothing.
	j := &judge{d: extension.Decision{Extend: true, Reason: "clear progress"}}
	granted, d := extension.Consider(context.Background(), j, policy(), extension.Request{RoundsUsed: 10})
	if granted != 5 {
		t.Errorf("granted = %d, want the step", granted)
	}
	if !d.Extend {
		t.Error("the decision was not carried back")
	}
}

func TestARescueGrantsNothing(t *testing.T) {
	t.Parallel()
	// The counterfactual. Without it every assertion above passes for a
	// policy that grants regardless of what the judge said.
	j := &judge{d: extension.Decision{Reason: "looping on the same call"}}
	granted, d := extension.Consider(context.Background(), j, policy(), extension.Request{RoundsUsed: 10})
	if granted != 0 {
		t.Errorf("a rescue granted %d rounds", granted)
	}
	if d.Reason != "looping on the same call" {
		t.Errorf("the judge's reason was lost: %q", d.Reason)
	}
	// Even an explicit round count on a rescue is ignored — a judge that
	// says stop does not get to also say how far.
	j.d = extension.Decision{AdditionalRounds: 10}
	if granted, _ := extension.Consider(context.Background(), j, policy(), extension.Request{RoundsUsed: 10}); granted != 0 {
		t.Errorf("a rescue carrying a round count granted %d", granted)
	}
}

func TestABrokenJudgeRescuesRatherThanFailingTheTurn(t *testing.T) {
	t.Parallel()
	// The extension is an optimisation on a phase that already ran out of
	// rounds. Failing the turn because the optional cheap model that
	// decides whether to be generous was unreachable turns a degraded
	// outcome into no outcome.
	j := &judge{err: errors.New("provider down")}
	granted, d := extension.Consider(context.Background(), j, policy(), extension.Request{RoundsUsed: 10})
	if granted != 0 {
		t.Errorf("a failed judge granted %d rounds", granted)
	}
	if !strings.Contains(d.Reason, "provider down") {
		t.Errorf("the failure was not reported: %q", d.Reason)
	}
	// And an absent judge is the same shape, not a panic.
	if granted, d := extension.Consider(context.Background(), nil, policy(), extension.Request{RoundsUsed: 10}); granted != 0 || d.Reason != "no_judge" {
		t.Errorf("a nil judge gave %d / %q", granted, d.Reason)
	}
}

func TestTheJudgeIsToldWhatItMayActuallyGrant(t *testing.T) {
	t.Parallel()
	// So it does not ask for what it cannot have and then read the clamp as
	// the engine ignoring it.
	j := &judge{d: extension.Decision{Extend: true}}
	extension.Consider(context.Background(), j, policy(), extension.Request{RoundsUsed: 38})
	if j.seen.MaxStep != 2 {
		t.Errorf("MaxStep = %d, want the real remaining 2", j.seen.MaxStep)
	}
	if j.seen.RemainingUnderCeiling != 2 {
		t.Errorf("RemainingUnderCeiling = %d, want 2", j.seen.RemainingUnderCeiling)
	}
}

func TestTheFinishHintMatchesHowThePhaseEnds(t *testing.T) {
	t.Parallel()
	// Each phase leaves by a different door: the executor through
	// `submit_work`, the reviewer through `submit_review`, onboarding
	// through `mark_onboarded`, and only a sub-agent by returning text with
	// no tool call. A phase told the wrong way to finish spends its granted
	// rounds trying to leave through a door it does not have — the exact
	// failure the extension was granted to avoid.
	//
	// ONBOARDING IS THE CASE THAT WAS MISSING, and the one that was silent:
	// told to stop calling tools, an extended onboarding pass never reaches
	// mark_onboarded, so the marker is never stamped and the pass re-runs on
	// every turn that seat ever takes.
	for _, tc := range []struct {
		ph        phase.Phase
		want, not string
	}{
		{phase.Execute, "submit_work", "submit_review"},
		{phase.Review, "submit_review", "submit_work"},
		{phase.Onboarding, "mark_onboarded", "submit_work"},
		{phase.Subagent, "stop calling tools", "submit_work"},
	} {
		hint := extension.FinishHint(tc.ph)
		if !strings.Contains(hint, tc.want) {
			t.Errorf("the %s hint does not name its exit: %q", tc.ph, hint)
		}
		if strings.Contains(hint, tc.not) {
			t.Errorf("the %s hint names another phase's exit: %q", tc.ph, hint)
		}
	}
}

func TestTheNudgeCarriesTheGrantAndTheReason(t *testing.T) {
	t.Parallel()
	got := extension.Nudge(phase.Execute, 3, "making steady progress")
	for _, want := range []string{"3 additional tool-call rounds", "making steady progress", "submit_work"} {
		if !strings.Contains(got, want) {
			t.Errorf("the nudge is missing %q:\n%s", want, got)
		}
	}
	// One round is one round. "1 rounds" in a prompt reads as a template
	// the engine did not finish.
	if one := extension.Nudge(phase.Execute, 1, ""); !strings.Contains(one, "1 additional tool-call round ") &&
		!strings.Contains(one, "1 additional tool-call round\n") {
		t.Errorf("a single round was pluralised:\n%s", one)
	}
	// A judge that gave no reason still produces a readable line.
	if none := extension.Nudge(phase.Review, 2, ""); !strings.Contains(none, "(none)") {
		t.Errorf("an absent reason left a dangling label:\n%s", none)
	}
}

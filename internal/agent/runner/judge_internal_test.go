package runner

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/extension"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/tools"
)

// fullMeter holds a refusal from the start: every further charge is certain to
// be refused.
type fullMeter struct{ spent atomic.Int32 }

func (m *fullMeter) Spend(context.Context, int) (toolloop.SpendOutcome, error) {
	m.spent.Add(1)
	return toolloop.SpendOutcome{Scope: "agent", Used: 200, Limit: 200}, nil
}

func (m *fullMeter) Refused(context.Context) (toolloop.SpendOutcome, bool) {
	return toolloop.SpendOutcome{Scope: "agent", Used: 200, Limit: 200}, true
}

// askedJudge counts the judgements it was asked for.
type askedJudge struct{ asked atomic.Int32 }

func (j *askedJudge) Decide(context.Context, extension.Request) (extension.Decision, error) {
	j.asked.Add(1)
	return extension.Decision{Extend: true, Reason: "still going", Asked: true, InputTokens: 30}, nil
}

// A JUDGE IS NOT CALLED ON A REFUSAL ALREADY CERTAIN. When a phase runs out of
// rounds with the window already full (a delegated worker or a peer on the
// seat's counter spent it), the judgement would be billed then refused, and the
// only verdict is the rescue. So the meter is asked first, and the answer says
// why.
//
// Mutation: drop the Refusal check in [Runner.consider] and the judge is asked.
func TestTheJudgeIsNotCalledOnARefusalAlreadyCertain(t *testing.T) {
	t.Parallel()
	judge := &askedJudge{}
	meter := &fullMeter{}
	r, err := New(Config{
		Registry: tools.NewRegistry(), Models: &phase.Registry{},
		Judge: judge, Budget: meter, Sessions: testHost(t), Launcher: stubLauncher{},
	})
	if err != nil {
		t.Fatal(err)
	}
	policy := extension.Policy{Enabled: true, RoundStep: 4, Ceiling: 8}
	granted, decision := r.consider(context.Background(), phase.Onboarding, 1, 2, policy,
		extension.Request{Phase: phase.Onboarding, RoundsUsed: 2})
	if granted != 0 || decision.Reason != extension.ReasonBudgetExhausted {
		t.Errorf("consider = %d, %+v; want no rounds, rescued on the budget", granted, decision)
	}
	if n := judge.asked.Load(); n != 0 {
		t.Errorf("the judge was asked %d times on a window the meter already held full", n)
	}
	if n := meter.spent.Load(); n != 0 {
		t.Errorf("%d charges were made for a judgement nobody made", n)
	}
}

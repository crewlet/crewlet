package assistant_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/assistant"
	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/evals"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/proposal/approval"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// answerOf is the answer a turn's frames drew.
func answerOf(frames []wire.Frame) string {
	text, _ := drawn(frames)
	return text
}

// ranTools reports whether model was asked to run a turn: a request that
// offered it tools, as a turn's rounds do and a conversation's title does not.
func ranTools(model *evals.Model) bool {
	return slices.ContainsFunc(model.Requests(), func(r llm.Request) bool { return r.Tools != nil })
}

// deliberateRig is a rig whose company sets a stronger chain, its quick and
// strong models each scripted with rounds of their own.
func deliberateRig(t *testing.T, decisionBriefs bool, quick, strong *evals.Model,
	o evals.RigOptions) *assistanttest.Rig {

	t.Helper()
	company, err := evals.DeliberateCompany(decisionBriefs, "grace", "ada")
	if err != nil {
		t.Fatal(err)
	}
	o.Company = company
	o.Models = &evals.Models{Model: quick, Key: "assistant", Strong: strong, StrongKey: "strong"}
	return assistanttest.NewRig(t, o)
}

// Think harder runs one question on the stronger chain; the next plain
// question runs on the quick chain again.
func TestThinkHarderRunsOneQuestionOnTheStrongerChain(t *testing.T) {
	t.Parallel()
	// The title call runs on the quick chain too, so it gets a round of its own.
	quick := evals.NewModel(evals.Say("Quick answer."), evals.Say("Quick answer."))
	strong := evals.NewModel(evals.Say("Considered answer."))
	f := deliberateRig(t, false, quick, strong, evals.RigOptions{})

	accepted, err := f.Svc.Start(context.Background(), assistant.TurnStart{Person: f.Person,
		Conversation: f.Conversation, Key: evals.Key("k-hard"), Input: "Which way should we route it?",
		Read: f.Grant("k-hard"), Deliberate: true})
	if err != nil {
		t.Fatal(err)
	}
	if answer := answerOf(f.Follow(accepted.TurnID, 0)); answer != "Considered answer." {
		t.Errorf("the question asked to think harder was answered %q", answer)
	}
	if !ranTools(strong) || ranTools(quick) {
		t.Fatalf("the turn ran on the quick chain (strong %v, quick %v)", ranTools(strong), ranTools(quick))
	}

	plain, err := f.Ask("k-plain", "And the next step?", nil)
	if err != nil {
		t.Fatal(err)
	}
	if answer := answerOf(f.Follow(plain.TurnID, 0)); answer != "Quick answer." {
		t.Errorf("the plain question after it was answered %q", answer)
	}
}

// Think harder with no stronger chain is refused, naming the setting, and
// reaches no model.
func TestThinkHarderWithNoStrongerChainIsRefused(t *testing.T) {
	t.Parallel()
	model := evals.NewModel(evals.Say("Quick answer."))
	f := assistanttest.NewRig(t, evals.RigOptions{Models: &evals.Models{Model: model, Key: "assistant"}})
	_, err := f.Svc.Start(context.Background(), assistant.TurnStart{Person: f.Person,
		Conversation: f.Conversation, Key: evals.Key("k-hard"), Input: "Think about it",
		Read: f.Grant("k-hard"), Deliberate: true})
	if !errors.Is(err, assistant.ErrInvalid) || !strings.Contains(err.Error(), "assistant.deliberate") {
		t.Fatalf("the question was not refused naming assistant.deliberate: %v", err)
	}
	if len(model.Requests()) != 0 {
		t.Errorf("a refused question reached the model %d times", len(model.Requests()))
	}
}

// A Think harder turn resumes on the stronger chain after its proposal is
// approved, since the resumed segment continues the same loop.
func TestAThinkHarderTurnResumesOnTheStrongerChain(t *testing.T) {
	t.Parallel()
	create := assistanttest.Tool(t, "create_task")
	quick := evals.NewModel()
	strong := evals.NewModel(
		evals.Call("c1", "create_task", map[string]any{"title": "Route it as a Worker"}),
		evals.Say("Filed."))
	f := deliberateRig(t, false, quick, strong, evals.RigOptions{
		Tools: []catalog.Tool{create}, Acts: []string{create.Act},
		Handler: assistanttest.Routes(map[string]exec.Response{})})
	f.Person = evals.Person("grace", "grace", iam.GrantAdmin)
	accepted, err := f.Svc.Start(context.Background(), assistant.TurnStart{Person: f.Person,
		Conversation: f.Conversation, Key: evals.Key("k-hard"), Input: "File the follow-up",
		Read: f.Grant("k-hard"), Deliberate: true})
	if err != nil {
		t.Fatal(err)
	}
	if done := evals.Last(f.Follow(accepted.TurnID, 0), wire.EventDone); done == nil ||
		done.Done.State != wire.TurnWaitingForApproval {
		t.Fatalf("the proposing turn ended %+v, want waiting_for_approval", done)
	}
	proposed := f.Store.Proposals()
	if len(proposed) != 1 {
		t.Fatalf("the turn proposed %+v", proposed)
	}
	ctx := iam.WithPrincipal(context.Background(), f.Person)
	resumed, err := f.Svc.Resume(ctx, approval.Resumption{Owner: f.Person.ID,
		Conversation: f.Conversation, Turn: accepted.TurnID, Proposal: proposed[0].ID,
		Read: f.Grant("approve"), State: wire.ProposalApplied,
		Receipt: &wire.Receipt{Summary: "Done."}, Answer: []byte(`{"key":"ENG-7"}`),
		Card: cardAt(proposed[0], wire.ProposalApplied)})
	if err != nil {
		t.Fatal(err)
	}
	if answer := answerOf(f.Follow(accepted.TurnID, resumed.AfterSeq)); !strings.Contains(answer, "Filed.") {
		t.Errorf("the resumed segment answered %q", answer)
	}
	if ranTools(quick) {
		t.Error("the resumed segment ran on the quick chain")
	}
}

// A decision brief runs on the stronger chain only where `decision_briefs`
// says so.
func TestABriefOfADecisionRunsOnTheChainTheCompanyChose(t *testing.T) {
	t.Parallel()
	page := itemPage()
	page.Selection.Noun = wire.NounDecision
	for _, tc := range []struct {
		name           string
		decisionBriefs bool
	}{{"decision briefs on the stronger chain", true}, {"decision briefs on the quick chain", false}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			quick, strong := evals.NewModel(), evals.NewModel()
			f := deliberateRig(t, tc.decisionBriefs, quick, strong, evals.RigOptions{})
			accepted, err := f.Brief("k-brief", "Brief me on this decision", page)
			if err != nil {
				t.Fatal(err)
			}
			f.Follow(accepted.TurnID, 0)
			if ranTools(strong) != tc.decisionBriefs || ranTools(quick) == tc.decisionBriefs {
				t.Errorf("the brief ran on strong %v, quick %v; want strong %v",
					ranTools(strong), ranTools(quick), tc.decisionBriefs)
			}
		})
	}
}

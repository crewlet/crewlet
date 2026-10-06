package learning_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// How a post-turn worker's prompt describes the turn it is judging.

// THE LABEL IS NAMED AS WHAT WOKE THE TURN, AND THE ASK IS SHOWN. Every worker
// called the waking event's one-line label "Task:" — "cto asked a colleague on
// ch-1" — so a skill was distilled from, and a lesson drawn from, a turn whose
// task the model was told was that line; what the colleague asked was nowhere
// in the prompt.
func TestTheSynthesisPromptShowsWhatTheTurnWasAsked(t *testing.T) {
	t.Parallel()
	s, p := synthesizerWith(t, newStore(t), &auxProvider{replies: []llm.Completion{{Content: "{}"}}}, learning.SynthesizerOptions{MinToolCalls: 3})
	turn := toolTurn("a", "b", "c")
	turn.Event.TaskSummary = "cto asked a colleague on ch-1"
	turn.Event.Ask = "A colleague (CTO) asks:\n\nHow do we cut a hotfix release?"
	if _, err := s.Reflect(t.Context(), turn); err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	prompt := p.prompt(t, 0)
	for _, want := range []string{
		"- Woken by: cto asked a colleague on ch-1",
		`- Asked: "A colleague (CTO) asks: How do we cut a hotfix release?"`,
		"- What it did: cut, tag, announce",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt is missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "- Task:") || strings.Contains(prompt, "- Plan:") {
		t.Errorf("the prompt still labels the trigger as the task:\n%s", prompt)
	}
}

// THE PERSIST DECIDER IS SHOWN ONLY THE ASK ITS INTERACTIONS DO NOT CARRY:
// each interaction is already rendered with its sender, and a second copy
// unattributed would invite the model to file a fact under nobody.
func TestThePersistPromptCarriesTheAskOnlyWhereNoInteractionDoes(t *testing.T) {
	t.Parallel()
	asked := pdTurn()
	asked.Event.Ask = "A colleague (CTO) asks:\n\nWhich region do we fail over to?"
	p := says(`{"kind":"NOOP"}`)
	mustDecide(t, decider(t, p, &fakeDiary{}), asked)
	if prompt := p.prompt(t, 0); !strings.Contains(prompt,
		`- Asked: "A colleague (CTO) asks: Which region do we fail over to?"`) {
		t.Errorf("a colleague's question did not reach the decider:\n%s", prompt)
	}

	spoken := pdTurn()
	spoken.Event.Interactions = []types.InboundInteraction{{
		Sender: types.CanonicalIdentity{Handle: "miles"}, Body: "fail over to eu-west",
	}}
	p = says(`{"kind":"NOOP"}`)
	mustDecide(t, decider(t, p, &fakeDiary{}), spoken)
	prompt := p.prompt(t, 0)
	if strings.Contains(prompt, "- Asked:") || strings.Count(prompt, "fail over to eu-west") != 1 {
		t.Errorf("an interaction's body was repeated as an unattributed ask:\n%s", prompt)
	}
}

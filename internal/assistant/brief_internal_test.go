package assistant

import (
	"context"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/wire"
)

// A CITED NAME IS A WHOLE NAME: ENG-1 is not cited by an answer that names
// only ENG-12 or XENG-1. Mutation: match a substring and ENG-1 is "read".
func TestACitedNameIsAWholeName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		text, name string
		want       bool
	}{
		{`{"key":"ENG-1"}`, "ENG-1", true},
		{"ENG-12 and ENG-1.", "ENG-1", true},
		{"only ENG-12", "ENG-1", false},
		{"XENG-1", "ENG-1", false},
		{"seat qa_lead", "qa", false},
		{"seat qa, idle", "qa", true},
		{"anything", "", false},
	}
	for _, c := range cases {
		if got := containsName(c.text, c.name); got != c.want {
			t.Errorf("containsName(%q, %q) = %v, want %v", c.text, c.name, got, c.want)
		}
	}
}

// DANGER IS THE ENGINE'S: a seat stopped or whose last turn failed, or a task
// marked urgent, and nothing else. Mutation: read any other field, or trust
// the model's danger, and a case fails.
func TestDangerNeedsAFactTheRecordsReadCarries(t *testing.T) {
	t.Parallel()
	cases := []struct {
		kind wire.ResourceKind
		body string
		want bool
	}{
		{wire.ResourceSeat, `{"live":{"activity":"stopped"}}`, true},
		{wire.ResourceSeat, `{"live":{"activity":"idle","stopped_reason":"budget"}}`, true},
		{wire.ResourceSeat, `{"live":{"activity":"idle","last_turn":{"outcome":"failed"}}}`, true},
		{wire.ResourceSeat, `{"live":{"activity":"working","stopped_reason":null}}`, false},
		{wire.ResourceTask, `{"task":{"priority":"urgent"}}`, true},
		{wire.ResourceTask, `{"task":{"priority":"high"}}`, false},
		{wire.ResourceArtifact, `{"priority":"urgent"}`, false},
		{wire.ResourceSeat, `not json`, false},
	}
	for _, c := range cases {
		if got := urgentIn(c.kind, []byte(c.body)); got != c.want {
			t.Errorf("urgentIn(%s, %s) = %v, want %v", c.kind, c.body, got, c.want)
		}
	}
	if toneOf(wire.BriefDanger, false) != wire.BriefWarning || toneOf(wire.BriefDanger, true) !=
		wire.BriefDanger || toneOf(wire.BriefNeutral, true) != wire.BriefNeutral {
		t.Error("a danger without its fact is not drawn warning, or a chosen tone was moved")
	}
}

// countingMeter records every charge and refuses none.
type countingMeter struct{ spent int }

func (m *countingMeter) Spend(_ context.Context, tokens int) (toolloop.SpendOutcome, error) {
	m.spent += tokens
	return toolloop.SpendOutcome{OK: true}, nil
}

func (m *countingMeter) Refused(context.Context) (toolloop.SpendOutcome, bool) {
	return toolloop.SpendOutcome{}, false
}

// A BRIEF'S CAP REFUSES PAST ITS OWN TOKENS, under its own scope, and every
// round is still charged to the windows. Mutation: charge the cap before the
// windows and the refused round goes unrecorded.
func TestABriefsCapIsItsOwnAndEveryRoundIsCharged(t *testing.T) {
	t.Parallel()
	inner := &countingMeter{}
	m := &briefMeter{inner: inner}
	ctx := context.Background()
	if out, err := m.Spend(ctx, BriefMaxTokens-1); err != nil || !out.OK {
		t.Fatalf("a round under the cap answered %+v, %v", out, err)
	}
	if _, refused := m.Refused(ctx); refused {
		t.Fatal("a standing refusal before the cap is spent")
	}
	out, err := m.Spend(ctx, 10)
	if err != nil || out.OK || out.Scope != BriefBudgetScope || out.Limit != BriefMaxTokens {
		t.Fatalf("the round over the cap answered %+v, %v", out, err)
	}
	if inner.spent != BriefMaxTokens+9 {
		t.Errorf("the windows were charged %d, want every round, %d", inner.spent, BriefMaxTokens+9)
	}
	if _, refused := m.Refused(ctx); !refused {
		t.Error("no standing refusal once the cap is spent")
	}
}

// A BRIEF ONLY READS: it is offered the reads and its answer, never a write, a
// navigation or a page action. Mutation: pass every offered tool through.
func TestABriefIsOfferedOnlyReadsAndItsAnswer(t *testing.T) {
	t.Parallel()
	offered := []catalog.Tool{{Name: "get_task", Kind: catalog.KindRead},
		{Name: "update_task", Kind: catalog.KindWrite},
		{Name: "open_in_app", Kind: catalog.KindNavigate},
		{Name: "propose_page_change", Kind: catalog.KindPageAction}}
	var names []string
	for _, tool := range briefTools(offered) {
		names = append(names, tool.Name)
	}
	if len(names) != 2 || names[0] != "get_task" || names[1] != catalog.AnswerBriefTool {
		t.Errorf("a brief is offered %v, want get_task and %s", names, catalog.AnswerBriefTool)
	}
}

// A BRIEF IS RE-READ WHOLE by the next turn's model, so a follow-up is asked
// of what the person was shown. Mutation: summarise it as "[card]" and this
// fails.
func TestABriefIsReReadWholeByTheNextTurn(t *testing.T) {
	t.Parallel()
	got := cardSummary(wire.Card{Type: wire.CardBrief, Brief: &wire.BriefCard{
		Scope:   wire.BriefScope{Screen: "inbox", Title: "Inbox"},
		Summary: "2 decisions wait.",
		Needs: []wire.BriefNeed{{Ref: wire.ResourceRef{Kind: wire.ResourceTask, ID: "ENG-4"},
			Text: "QA asks", Detail: "2 hours", Tone: wire.BriefWarning}},
		Recommended: &wire.BriefStep{Text: "Answer ENG-4."},
	}})
	for _, want := range []string{"Inbox", "2 decisions wait.", "task ENG-4", "QA asks",
		"2 hours", "Answer ENG-4."} {
		if !strings.Contains(got, want) {
			t.Errorf("the brief's summary %q does not carry %q", got, want)
		}
	}
}

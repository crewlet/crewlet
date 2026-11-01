package assistant

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/words"
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
		wire.BriefDanger || toneOf(wire.BriefWarning, true) != wire.BriefWarning {
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

// A BRIEF IS RE-READ WHOLE by the next turn's model, verdict first, so a
// follow-up is asked of what the person was shown, and a brief of nothing to
// do is re-read as saying so. Mutation: summarise it as "[card]", or drop the
// verdict, and this fails.
func TestABriefIsReReadWholeByTheNextTurn(t *testing.T) {
	t.Parallel()
	got := cardSummary(wire.Card{Type: wire.CardBrief, Brief: &wire.BriefCard{
		Scope: wire.BriefScope{Screen: "inbox", Title: "Inbox"},
		About: "QA asks which build to test.",
		Needs: []wire.BriefNeed{{Ref: wire.ResourceRef{Kind: wire.ResourceTask, ID: "ENG-4"},
			Text: "Answer ENG-4", Detail: "Asked 2 hours ago", Tone: wire.BriefWarning}},
		Context: "The beta ships Thursday.",
	}})
	for _, want := range []string{"Inbox", "1 thing needs you", "QA asks which build to test.",
		"task ENG-4", "Answer ENG-4", "Asked 2 hours ago", "The beta ships Thursday."} {
		if !strings.Contains(got, want) {
			t.Errorf("the brief's summary %q does not carry %q", got, want)
		}
	}
	clear := cardSummary(wire.Card{Type: wire.CardBrief, Brief: &wire.BriefCard{
		Scope: wire.BriefScope{Screen: "home", Title: "Home"}, About: "Every agent is working."}})
	if !strings.Contains(clear, "Nothing needs you") {
		t.Errorf("a brief with no rows is re-read as %q, which does not say nothing needs them",
			clear)
	}
}

// THE BRIEF'S PROMPT STATES EVERY CAP IN PLAIN WORDS, from the constants the
// engine checks, so a model meets each before it writes. Mutation: drop a cap
// from the prompt and this fails.
func TestTheBriefPromptStatesEveryCap(t *testing.T) {
	t.Parallel()
	for _, want := range []string{
		fmt.Sprintf("one line of at most %d characters", wire.BriefAboutMaxRunes),
		fmt.Sprintf("at most %d, most pressing first", wire.BriefMaxNeeds),
		fmt.Sprintf("at most %d characters; its why", wire.BriefTextMaxRunes),
		fmt.Sprintf("one fact, at most %d characters", wire.BriefDetailMaxRunes),
		fmt.Sprintf("one line of at most %d characters.\n- follow_ups", wire.BriefContextMaxRunes),
		fmt.Sprintf("up to %d short questions", wire.BriefMaxFollowUps),
		fmt.Sprintf("each at most %d characters", wire.BriefQuestionMaxRunes),
		"fix everything it names",
	} {
		if !strings.Contains(briefRules, want) {
			t.Errorf("the brief's prompt does not say %q", want)
		}
	}
}

// THE BRIEF'S RULES NAME EVERY RECORD A ROW MAY CITE, in words, from the one
// list the answer's schema takes ([catalog.BriefRefKinds]): written out by
// hand, the rules named five kinds, and a sixth the schema came to take (a
// chat room) was a record the model was never told it could cite. Mutation:
// drop a kind's words, give words to a kind no row may cite, or write the list
// out again by hand, and this fails.
func TestTheBriefRulesNameEveryCitableKind(t *testing.T) {
	t.Parallel()
	listed := briefCitable()
	if !strings.Contains(briefRules, "("+listed+")") {
		t.Errorf("the brief's rules do not name the records a row may cite as %q", listed)
	}
	for _, k := range catalog.BriefRefKinds {
		phrase, ok := briefCiteWords[k]
		if !ok || !strings.Contains(listed, phrase) {
			t.Errorf("the brief's rules have no words for %s, which a row may cite", k)
		}
	}
	for k := range briefCiteWords {
		if !slices.Contains(catalog.BriefRefKinds, k) {
			t.Errorf("the brief's rules name %s, which no row may cite", k)
		}
	}
}

// THE BRIEF'S QUESTION OFFERS EVERY KIND A NEED MAY CITE AS THE ANSWER WRITES
// IT, from the same list ([catalog.BriefRefKinds]), each quoted as the value it
// is and the last joined by "or" ([words.Or]). It said them as "A row may cite
// a task, project, seat, page, artifact, room": unquoted and with no "or", so
// the set a ref_kind comes from read as a list. Mutation: join the kinds by
// hand again, or leave one out, and this fails.
func TestTheBriefQuestionOffersEveryCitableKind(t *testing.T) {
	t.Parallel()
	in := basePrompt()
	in.Page = &wire.PageContext{Screen: "inbox", Title: "Inbox", Params: map[string]string{},
		Query: map[string]string{}, Resources: []wire.Resource{}, Accepts: []string{}}
	want := "A need's ref_kind is one of " + words.Or(catalog.BriefRefKinds) + "."
	if question := briefQuestion(in); !strings.Contains(question, want) {
		t.Errorf("the brief's question does not offer the kinds a need may cite as %q:\n%s",
			want, question)
	}
}

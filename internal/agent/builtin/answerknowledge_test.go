package builtin_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/compact"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// answerSearch ranks a fixed set of pages and records what it was asked.
type answerSearch struct {
	hits    []knowledge.Hit
	nothing bool // nothing ran: the backend could not be searched
	asked   []knowledge.Query
}

func (s *answerSearch) CanSearch(*org.Role, *org.Organization) bool { return true }

func (s *answerSearch) Search(_ context.Context, q knowledge.Query) knowledge.Result {
	s.asked = append(s.asked, q)
	if s.nothing {
		return knowledge.Result{}
	}
	return knowledge.Result{Hits: s.hits, Outcome: knowledge.Outcome{ServedMode: knowledge.ModeHybrid}}
}

// answerItems ranks a fixed set of work items.
type answerItems struct {
	hits  []tracker.Ranked
	err   error
	asked []tracker.SearchQuery
}

func (s *answerItems) Search(_ context.Context, q tracker.SearchQuery) (tracker.SearchAnswer, error) {
	s.asked = append(s.asked, q)
	return tracker.SearchAnswer{Hits: s.hits}, s.err
}

// answerPages serves page bodies by id.
type answerPages struct{ bodies map[string]string }

func (answerPages) List(context.Context, pages.Filter, statelog.Freshness) (pages.Listing, error) {
	return pages.Listing{}, nil
}

func (p answerPages) Get(_ context.Context, ref string, _ statelog.Freshness) (pages.Detail, error) {
	body, ok := p.bodies[ref]
	if !ok {
		return pages.Detail{}, errors.New("no such page")
	}
	return pages.Detail{Page: pages.Page{ID: ref, Body: body}}, nil
}

// answerModel is a model with a known answer and a known cost, behind the
// auxiliary seam — which records every attribution it is handed, since the
// seam is what charges and records each call under it.
type answerModel struct {
	content string
	in, out int
	err     error
	calls   int
	asked   []llm.Request
	role    *org.Role
	uses    []auxspend.Use
	// failRewrite fails the condensing calls only — the ones made with the
	// compactor's instructions — and answers the question as usual.
	failRewrite bool
}

func (m *answerModel) Model() string { return "aux-small" }

func (m *answerModel) Complete(_ context.Context, req llm.Request) (*llm.Completion, error) {
	m.calls++
	m.asked = append(m.asked, req)
	if m.err != nil {
		return nil, m.err
	}
	if m.failRewrite && strings.HasPrefix(req.Messages[0].Content, "You rewrite text") {
		return nil, errors.New("503 from the rewrite")
	}
	return &llm.Completion{Model: "aux-small", Content: m.content,
		InputTokens: m.in, OutputTokens: m.out}, nil
}

func (m *answerModel) Auxiliary(role *org.Role, use auxspend.Use) (chain.Member, error) {
	m.role = role
	m.uses = append(m.uses, use)
	return chain.Member{Key: "aux", Provider: m}, nil
}

// purposes is what each call the seam resolved was for, in order.
func (m *answerModel) purposes() []types.AuxPurpose {
	out := make([]types.AuxPurpose, 0, len(m.uses))
	for _, use := range m.uses {
		out = append(out, use.Purpose)
	}
	return out
}

// answerBudget is the company's windows, as the gate reads them.
type answerBudget struct {
	refusal  builtin.BudgetRefusal
	refusing bool
	err      error
}

func (b *answerBudget) Refusing(context.Context) (builtin.BudgetRefusal, bool, error) {
	return b.refusal, b.refusing, b.err
}

// answerRig is one operator surface serving answer_knowledge.
type answerRig struct {
	search   *answerSearch
	items    *answerItems
	pages    answerPages
	model    *answerModel
	budget   *answerBudget
	position string
	seat     string
}

func newAnswerRig() *answerRig {
	return &answerRig{
		search: &answerSearch{hits: []knowledge.Hit{{
			Title: "Deploy runbook", PageID: "p-1", Backend: pages.Backend,
			Snippet: "how to deploy",
		}, {
			Title: "Rollback", PageID: "p-2", Backend: pages.Backend, Snippet: "undo a deploy",
		}}},
		items: &answerItems{hits: []tracker.Ranked{{
			ID: "t-1", Key: "ENG-7", Title: "Automate the deploy", Snippet: "script it",
		}}},
		pages: answerPages{bodies: map[string]string{
			"p-1": "Run `make deploy` from main.", "p-2": "Revert the tag.",
		}},
		model: &answerModel{content: "Run `make deploy` [1]; it is being automated [3].",
			in: 700, out: 120},
		budget:   &answerBudget{},
		position: "tracker@1:10,pages@1:20,vectors@1:5",
		seat:     "founder",
	}
}

func (r *answerRig) tool(t *testing.T) tools.Callable {
	t.Helper()
	company := &org.Organization{Name: "Acme", Roles: []*org.Role{{Name: "Founder"}}}
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
		Knowledge: r.search,
		Org:       func() *org.Organization { return company },
		Work:      builtin.WorkDeps{Search: r.items},
		Pages:     builtin.PageDeps{Reader: r.pages},
		Answer: builtin.AnswerDeps{
			Models: r.model, Budget: r.budget,
			Corpus: func() (string, bool) { return r.position, r.position != "" },
			Actor: func(context.Context, *turnctx.Turn) (builtin.Actor, error) {
				return builtin.Actor{Handle: "founder-token", Kind: tracker.AuthorOperator,
					OperatorID: "founder-token", Seat: r.seat}, nil
			},
		},
	}) {
		if tool.Name() == builtin.AnswerKnowledgeTool {
			return tool
		}
	}
	t.Fatal("the operator catalogue serves no answer_knowledge with every seam wired")
	return nil
}

func ask(t *testing.T, tool tools.Callable, q string) (tools.Result, builtin.KnowledgeAnswer, map[string]any) {
	t.Helper()
	res, err := tool.Call(t.Context(), map[string]any{"q": q})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var answer builtin.KnowledgeAnswer
	var raw map[string]any
	if !res.Failed {
		if err := json.Unmarshal([]byte(res.Output), &answer); err != nil {
			t.Fatalf("the answer is not JSON: %s", res.Output)
		}
		_ = json.Unmarshal([]byte(res.Output), &raw)
	}
	return res, answer, raw
}

// AN ANSWER IS WRITTEN BY THE PERSON'S AUXILIARY MODEL, RESOLVED THROUGH THE
// SEAM AS THE OPERATOR'S SPEND. The seam is what charges the company's windows
// and records the call for the person, so what this tool owes it is the
// attribution: the operator stage — a person's, on no seat budget — and the
// answer's own purpose, on the asker's own seat. The answer says back what the
// reply reported.
func TestAnAnswerIsResolvedAsThePersonsSpend(t *testing.T) {
	t.Parallel()
	rig := newAnswerRig()
	res, answer, _ := ask(t, rig.tool(t), "How do we deploy?")
	if res.Failed {
		t.Fatalf("refused: %s", res.Output)
	}
	want := auxspend.Use{Stage: types.AuxStageOperator, Purpose: types.AuxAnswerKnowledge}
	if len(rig.model.uses) != 1 || rig.model.uses[0] != want {
		t.Errorf("the seam was handed %+v, want one call attributed %+v", rig.model.uses, want)
	}
	if err := want.Validate(); err != nil {
		t.Errorf("the answer's attribution is one the seam refuses: %v", err)
	}
	if answer.Tokens != (builtin.AnswerTokens{Input: 700, Output: 120}) || answer.Model != "aux-small" ||
		answer.Cached {
		t.Errorf("answered tokens %+v, model %q, cached %v", answer.Tokens, answer.Model, answer.Cached)
	}
	if rig.model.role == nil || rig.model.role.Handle() != "founder" {
		t.Errorf("resolved the model for %v, want the asker's own seat's auxiliary model",
			rig.model.role)
	}
	if !strings.Contains(answer.AnswerMD, "make deploy") {
		t.Errorf("answer_md = %q", answer.AnswerMD)
	}
}

// THE SAME QUESTION AT THE SAME CORPUS SPENDS NOTHING. Asked again — spelled
// with other case and spacing — it is served from the cache and says so; once
// the corpus moves, the old answer is not what the sources say any more, and
// it is asked again.
func TestASecondIdenticalQuestionIsServedFromCache(t *testing.T) {
	t.Parallel()
	rig := newAnswerRig()
	tool := rig.tool(t)
	_, first, _ := ask(t, tool, "How do we deploy?")

	res, again, _ := ask(t, tool, "  how do WE   deploy? ")
	if res.Failed {
		t.Fatalf("refused: %s", res.Output)
	}
	if !again.Cached || again.Tokens != (builtin.AnswerTokens{}) || rig.model.calls != 1 ||
		len(rig.model.uses) != 1 {
		t.Fatalf("second ask: cached %v, tokens %+v, model calls %d, resolutions %d — want a "+
			"cache hit that spent nothing", again.Cached, again.Tokens, rig.model.calls, len(rig.model.uses))
	}
	if again.AnswerMD != first.AnswerMD || len(again.Sources) != len(first.Sources) {
		t.Errorf("the cached answer differs from the one it was cached from")
	}

	rig.position = "tracker@1:11,pages@1:20,vectors@1:5"
	if _, moved, _ := ask(t, tool, "How do we deploy?"); moved.Cached || rig.model.calls != 2 {
		t.Fatalf("after the corpus moved: cached %v, model calls %d — want a fresh answer",
			moved.Cached, rig.model.calls)
	}
}

// NO CORPUS POSITION, NO CACHE: an answer from an external wiki is asked
// every time, because the wiki can change without this node hearing.
func TestWithNoCorpusPositionNothingIsCached(t *testing.T) {
	t.Parallel()
	rig := newAnswerRig()
	rig.position = ""
	tool := rig.tool(t)
	ask(t, tool, "How do we deploy?")
	if _, again, _ := ask(t, tool, "How do we deploy?"); again.Cached || rig.model.calls != 2 {
		t.Fatalf("cached %v after %d calls, want every ask answered afresh", again.Cached, rig.model.calls)
	}
}

// A CACHE HIT IS SERVED EVEN WHEN THE COMPANY IS OUT OF TOKENS: it spends
// none, so there is nothing for the budget to refuse.
func TestACachedAnswerIsServedWhenTheBudgetIsSpent(t *testing.T) {
	t.Parallel()
	rig := newAnswerRig()
	tool := rig.tool(t)
	ask(t, tool, "How do we deploy?")
	rig.budget.refusing = true
	if res, again, _ := ask(t, tool, "How do we deploy?"); res.Failed || !again.Cached {
		t.Fatalf("a spent budget refused a cached answer: %s", res.Output)
	}
}

// THE ANSWER NAMES ITS SOURCES, numbered as the model read them: source [n] is
// the n-th entry of `sources`, pages first and then work items, each with the
// ref a screen links it by — a page's id, an item's key — and each read whole
// where this node holds it rather than as a snippet.
func TestTheAnswerNamesItsSources(t *testing.T) {
	t.Parallel()
	rig := newAnswerRig()
	res, answer, _ := ask(t, rig.tool(t), "How do we deploy?")
	if res.Failed {
		t.Fatalf("refused: %s", res.Output)
	}
	want := []builtin.AnswerSource{
		{Kind: builtin.AnswerSourcePage, Ref: "p-1", Title: "Deploy runbook"},
		{Kind: builtin.AnswerSourcePage, Ref: "p-2", Title: "Rollback"},
		{Kind: builtin.AnswerSourceTask, Ref: "ENG-7", Title: "Automate the deploy"},
	}
	if len(answer.Sources) != len(want) {
		t.Fatalf("sources = %+v, want %+v", answer.Sources, want)
	}
	for i := range want {
		if answer.Sources[i] != want[i] {
			t.Errorf("source [%d] = %+v, want %+v", i+1, answer.Sources[i], want[i])
		}
	}
	user := rig.model.asked[0].Messages[1].Content
	for i, needle := range []string{"[1] page: Deploy runbook\nRun `make deploy` from main.",
		"[2] page: Rollback\nRevert the tag.",
		"[3] work item: ENG-7 Automate the deploy\n" + builtin.SnippetOnly + "script it"} {
		if !strings.Contains(user, needle) {
			t.Errorf("source [%d] reached the model as something other than %q:\n%s", i+1, needle, user)
		}
	}
	if rig.model.asked[0].Messages[0].Content != prompts.KnowledgeAnswerSystem {
		t.Error("the answer ran without its contract")
	}
	// Extraction from the excerpts, with a person waiting: a thinking model
	// at the entry's level spends AnswerMaxTokens reasoning and answers
	// nothing.
	if got := rig.model.asked[0].Effort; got != llm.EffortLow {
		t.Errorf("effort = %q, want a ceiling of low", got)
	}
	if got := rig.search.asked[0]; got.Mode != knowledge.ModeHybrid ||
		got.Limit != builtin.AnswerPageSources {
		t.Errorf("searched pages with %+v, want hybrid for %d", got, builtin.AnswerPageSources)
	}
	if got := rig.items.asked[0]; got.Mode != knowledge.ModeHybrid ||
		got.Limit != builtin.AnswerTaskSources {
		t.Errorf("searched items with %+v, want hybrid for %d", got, builtin.AnswerTaskSources)
	}
}

// TOKENS, NEVER A PRICE. Nothing in the answer converts what it cost into
// money — no key and no value — because the dashboard renders tokens only.
func TestAnAnswerNeverCarriesAPrice(t *testing.T) {
	t.Parallel()
	rig := newAnswerRig()
	res, _, raw := ask(t, rig.tool(t), "How do we deploy?")
	if res.Failed {
		t.Fatalf("refused: %s", res.Output)
	}
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch v := v.(type) {
		case map[string]any:
			for k, inner := range v {
				lower := strings.ToLower(k)
				for _, money := range []string{"cost", "price", "usd", "dollar", "amount", "currency"} {
					if strings.Contains(lower, money) {
						t.Errorf("the answer carries %s.%s", path, k)
					}
				}
				walk(path+"."+k, inner)
			}
		case []any:
			for _, inner := range v {
				walk(path+"[]", inner)
			}
		}
	}
	walk("answer", raw)
	if strings.Contains(res.Output, "$") {
		t.Errorf("the answer renders a currency sign: %s", res.Output)
	}
	if _, has := raw["tokens"]; !has {
		t.Error("the answer does not say what it spent in tokens")
	}
}

// AN UNBOUND TOKEN CANNOT ASK, on either transport: the spend is on a
// person's behalf, and a credential nobody bound is not a person. Nothing is
// searched, nothing asked of a model and nothing charged.
func TestAnUnboundCredentialIsRefusedBeforeAnythingIsSpent(t *testing.T) {
	t.Parallel()
	rig := newAnswerRig()
	rig.seat = ""
	res, _, _ := ask(t, rig.tool(t), "How do we deploy?")
	if !res.Failed || res.Refusal != tools.RefusalForbidden ||
		!strings.Contains(res.Output, "contact.crewlet_operator_id") {
		t.Fatalf("an unbound credential was not refused with the binding to make: %+v", res)
	}
	if rig.model.calls != 0 || len(rig.model.uses) != 0 || len(rig.search.asked) != 0 {
		t.Errorf("refused after spending: model %d, resolutions %d, searches %d",
			rig.model.calls, len(rig.model.uses), len(rig.search.asked))
	}
}

// A SPENT COMPANY WINDOW REFUSES BEFORE THE CALL, naming the window and when
// it resets; an unreadable counter refuses too, rather than letting a call
// through that no ceiling was checked against.
func TestASpentBudgetRefusesTheAnswerBeforeItIsWritten(t *testing.T) {
	t.Parallel()
	rig := newAnswerRig()
	resets := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	rig.budget.refusing = true
	rig.budget.refusal = builtin.BudgetRefusal{Period: "month", Window: "2026-09",
		ResetsAt: resets, Used: 1000, Limit: 1000}
	res, _, _ := ask(t, rig.tool(t), "How do we deploy?")
	if !res.Failed || res.Refusal != tools.RefusalBudgetExhausted ||
		!strings.Contains(res.Output, "2026-09") || !strings.Contains(res.Output, "2026-10-01T00:00:00Z") {
		t.Fatalf("answered %+v, want budget_exhausted naming the month and its reset", res)
	}
	if rig.model.calls != 0 {
		t.Errorf("the model was asked %d times past a spent budget", rig.model.calls)
	}

	rig = newAnswerRig()
	rig.budget.err = errors.New("counter unreachable")
	if res, _, _ := ask(t, rig.tool(t), "How do we deploy?"); !res.Failed ||
		res.Refusal != tools.RefusalUnavailable || rig.model.calls != 0 {
		t.Fatalf("an unreadable counter let the answer through: %+v", res)
	}
}

// NOTHING MATCHED IS AN ANSWER THAT COSTS NOTHING; NOTHING RAN IS NOT AN
// ANSWER. A search that found nothing is said without asking a model, and one
// that could not run is refused rather than reported as an empty company.
func TestNoSourcesIsAnsweredWithoutAModelAndNoSearchIsRefused(t *testing.T) {
	t.Parallel()
	rig := newAnswerRig()
	rig.search.hits, rig.items.hits = nil, nil
	res, answer, _ := ask(t, rig.tool(t), "Who owns billing?")
	if res.Failed || len(answer.Sources) != 0 || answer.Model != "" || rig.model.calls != 0 ||
		len(rig.model.uses) != 0 {
		t.Fatalf("nothing matched: %+v (model calls %d, resolutions %d)", res, rig.model.calls,
			len(rig.model.uses))
	}

	rig = newAnswerRig()
	rig.search.nothing = true
	rig.items.err = errors.New("index unreachable")
	if res, _, _ := ask(t, rig.tool(t), "Who owns billing?"); !res.Failed ||
		res.Refusal != tools.RefusalUnavailable || rig.model.calls != 0 {
		t.Fatalf("a search that never ran was answered: %+v", res)
	}
}

// A FAILED CALL IS NOT AN ANSWER, and a reply that spent tokens and wrote
// nothing was still made through the seam — which charges and records what a
// completion reported whatever its caller makes of it, since the vendor billed
// it (the engine's own suite holds the seam to that).
func TestAnEmptyReplyIsRefusedAfterTheSeamSawIt(t *testing.T) {
	t.Parallel()
	rig := newAnswerRig()
	rig.model.content = "  "
	tool := rig.tool(t)
	res, _, _ := ask(t, tool, "How do we deploy?")
	if !res.Failed || res.Refusal != tools.RefusalUnavailable {
		t.Fatalf("an empty reply was answered: %+v", res)
	}
	if len(rig.model.uses) != 1 || rig.model.uses[0].Purpose != types.AuxAnswerKnowledge {
		t.Errorf("the empty reply was resolved as %+v, want through the seam as the answer",
			rig.model.uses)
	}
	if res, _, _ := ask(t, tool, "How do we deploy?"); !res.Failed || rig.model.calls != 2 {
		t.Error("a refusal was cached")
	}
}

// A REFUSED ANSWER IS REFUSED, and it was made through the seam. A refusal
// returns no completion — its text is not an answer — but the response the
// vendor billed travels in the error, and the seam charges and records it
// from there (llm.Billed; the engine's own suite holds the seam to that).
func TestARefusedAnswerIsRefusedAfterTheSeamSawIt(t *testing.T) {
	t.Parallel()
	rig := newAnswerRig()
	rig.model.err = llm.Refused("anthropic", "aux-small", &llm.Refusal{Category: "bio",
		Completion: &llm.Completion{Model: "aux-small", InputTokens: 640, OutputTokens: 9,
			StopReason: llm.StopRefusal}})
	res, _, _ := ask(t, rig.tool(t), "How do we deploy?")
	if !res.Failed || res.Refusal != tools.RefusalUnavailable {
		t.Fatalf("a refused answer was answered: %+v", res)
	}
	if len(rig.model.uses) != 1 || rig.model.uses[0].Purpose != types.AuxAnswerKnowledge {
		t.Errorf("the refused answer was resolved as %+v, want through the seam as the answer",
			rig.model.uses)
	}
}

// NO QUESTION, NO CALL.
func TestAnAnswerNeedsAQuestion(t *testing.T) {
	t.Parallel()
	rig := newAnswerRig()
	if res, _, _ := ask(t, rig.tool(t), "   "); !res.Failed || res.Refusal != tools.RefusalInvalid {
		t.Fatalf("an empty question was answered: %+v", res)
	}
}

// NOT SERVED WITHOUT A BUDGET: an answer that could spend against no counter
// is the one shape the tool is built to refuse, so it is not offered at all.
func TestAnswerKnowledgeIsOmittedWithNoBudget(t *testing.T) {
	t.Parallel()
	rig := newAnswerRig()
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
		Knowledge: rig.search,
		Org:       func() *org.Organization { return &org.Organization{} },
		Answer: builtin.AnswerDeps{Models: rig.model,
			Actor: func(context.Context, *turnctx.Turn) (builtin.Actor, error) { return builtin.Actor{}, nil }},
	}) {
		if tool.Name() == builtin.AnswerKnowledgeTool {
			t.Fatal("answer_knowledge was served with no budget behind it")
		}
	}
}

// A PAGE LONGER THAN AN ANSWER CAN READ IS CONDENSED FOR THE QUESTION, NOT
// CUT. It used to be cut to its first four kilobytes, unmarked — so the step
// the question was about, past the cut, never reached the model, and the
// answer said the runbook did not have it.
func TestALongSourceIsCondensedForTheQuestion(t *testing.T) {
	t.Parallel()
	rig := newAnswerRig()
	long := strings.Repeat("Preamble about the deploy pipeline. ", 300) + "STEP FOUR: rotate the signing key."
	rig.pages.bodies["p-1"] = long
	res, _, _ := ask(t, rig.tool(t), "How do we rotate the signing key?")
	if res.Failed {
		t.Fatalf("refused: %s", res.Output)
	}
	if len(rig.model.asked) != 2 {
		t.Fatalf("%d model calls, want the rewrite and the answer", len(rig.model.asked))
	}
	rewrite, answer := rig.model.asked[0], rig.model.asked[1]
	if !strings.Contains(rewrite.Messages[1].Content, "STEP FOUR: rotate the signing key.") {
		t.Fatal("the rewrite was not shown the end of the page")
	}
	if !strings.Contains(rewrite.Messages[0].Content, "How do we rotate the signing key?") {
		t.Fatal("the rewrite was not told the question it serves")
	}
	if !strings.Contains(answer.Messages[1].Content, "condensed by a model") {
		t.Fatalf("the answer was not told its source is a rewrite:\n%s", answer.Messages[1].Content)
	}
	if strings.Contains(answer.Messages[1].Content, long[:1000]) {
		t.Fatal("the answering model was handed the raw page as well")
	}
	// THE REWRITE IS THE PERSON'S SPEND TOO, under its own purpose: the
	// seam charges it to the company's windows and records it beside the
	// answer, so the history says what the question cost in full.
	got := rig.model.purposes()
	wantPurposes := []types.AuxPurpose{types.AuxCondense(string(compact.KindSource)), types.AuxAnswerKnowledge}
	if !slices.Equal(got, wantPurposes) {
		t.Fatalf("the seam was handed %v — want the rewrite and the answer, each named", got)
	}
	for _, use := range rig.model.uses {
		if use.Stage != types.AuxStageOperator || use.TurnID != "" || use.Tally != nil {
			t.Errorf("%s was filed as %+v, want the operator's, on no turn", use.Purpose, use)
		}
	}
}

// A SOURCE THAT CANNOT BE CONDENSED IS DROPPED — from what the model reads AND
// from the sources it cites — never handed over cut.
func TestASourceThatCannotBeCondensedIsNotCited(t *testing.T) {
	t.Parallel()
	rig := newAnswerRig()
	rig.model.failRewrite = true
	rig.pages.bodies["p-1"] = strings.Repeat("x", builtin.AnswerSourceBytes*3)
	res, answer, _ := ask(t, rig.tool(t), "How do we deploy?")
	if res.Failed {
		t.Fatalf("refused: %s", res.Output)
	}
	for _, src := range answer.Sources {
		if src.Ref == "p-1" {
			t.Fatal("an answer cited a source its model was never shown")
		}
	}
	if strings.Contains(rig.model.asked[len(rig.model.asked)-1].Messages[1].Content, "xxxx") {
		t.Fatal("a source that could not be condensed reached the model anyway")
	}
}

// A QUESTION PAST ITS LIMIT IS REFUSED, NOT CUT — an answer to the first four
// hundred bytes of a question answers a different question.
func TestALongQuestionIsRefusedNotCut(t *testing.T) {
	t.Parallel()
	rig := newAnswerRig()
	res, err := rig.tool(t).Call(t.Context(), map[string]any{"q": strings.Repeat("why ", 200)})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Failed || !strings.Contains(res.Output, "takes at most") {
		t.Fatalf("a long question was not refused naming the limit: %s", res.Output)
	}
	if rig.model.calls != 0 || len(rig.search.asked) != 0 {
		t.Fatal("a refused question still searched or spent")
	}
}

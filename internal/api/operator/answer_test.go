package operator_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/knowledge"
	crewletmcp "github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
)

// oneRunbook is a knowledge base holding one page.
type oneRunbook struct{}

func (oneRunbook) CanSearch(*org.Role, *org.Organization) bool { return true }

func (oneRunbook) Search(context.Context, knowledge.Query) knowledge.Result {
	return knowledge.Result{
		Hits:    []knowledge.Hit{{Title: "Deploy runbook", PageID: "p-1", Snippet: "make deploy"}},
		Outcome: knowledge.Outcome{ServedMode: knowledge.ModeHybrid},
	}
}

// countedModel answers every question and counts how often it was asked —
// behind the auxiliary seam, recording the attribution each call stated, since
// the seam is what charges and records it.
type countedModel struct {
	mu    sync.Mutex
	calls int
	uses  []auxspend.Use
}

func (m *countedModel) Model() string { return "aux" }

func (m *countedModel) Complete(context.Context, llm.Request) (*llm.Completion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	return &llm.Completion{Model: "aux", Content: "Run `make deploy` [1].",
		InputTokens: 300, OutputTokens: 40}, nil
}

func (m *countedModel) Auxiliary(_ *org.Role, use auxspend.Use) (chain.Member, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.uses = append(m.uses, use)
	return chain.Member{Key: "aux", Provider: m}, nil
}

func (m *countedModel) resolved() []auxspend.Use {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.uses)
}

func (m *countedModel) asked() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// companyWindows is a company budget that can be spent.
type companyWindows struct {
	mu    sync.Mutex
	spent bool
}

func (b *companyWindows) Refusing(context.Context) (builtin.BudgetRefusal, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return builtin.BudgetRefusal{Period: "day", Window: "2026-09-25", Used: 10, Limit: 10}, b.spent, nil
}

// answerSurface is an operator surface serving answer_knowledge, in
// [company].
func answerSurface(t *testing.T, model *countedModel, budget *companyWindows) *operator.Server {
	t.Helper()
	return newSurface(t, operator.Options{
		Halves: fixed(operator.Halves{Knowledge: oneRunbook{}}),
		Org:    company,
		Answer: builtin.AnswerDeps{Models: model, Budget: budget},
	})
}

func askBody() string {
	raw, _ := json.Marshal(map[string]any{
		"args": map[string]any{"q": "How do we deploy?"},
	})
	return string(raw)
}

// ONLY A PERSON BOUND TO A SEAT MAY ASK, ON EVERY TRANSPORT: the answer runs on
// the auxiliary model of the asker's own seat and spends the company's tokens
// on their behalf, so a credential no seat binds has no model to answer on and
// nobody the spend is charged for. It needs an app too — the knowledge base or
// the tracker, each corpus searched only for whoever holds its app — since what
// an answer discloses is what those already show their users. Neither refusal
// asks the model; the bound person beside them is answered, so a surface that
// refused everybody fails here too.
func TestOnlyAPersonBoundToASeatMayAsk(t *testing.T) {
	t.Parallel()
	model, budget := &countedModel{}, &companyWindows{}
	s := answerSurface(t, model, budget)
	h := guarded(s)

	status, answer := act(t, h, "ci", builtin.AnswerKnowledgeTool,
		"application/json", askBody(), newKey())
	if status != http.StatusForbidden || answer["error"] != string(crewletmcp.RefusalForbidden) {
		t.Fatalf("an unbound token over act answered %d %v, want 403 forbidden", status, answer)
	}
	res, err := s.Dispatch(iam.WithPrincipal(t.Context(), pipeline), operator.Call{
		Transport: types.TransportMCP, Tool: builtin.AnswerKnowledgeTool,
		Args: map[string]any{"q": "How do we deploy?"},
	})
	if err != nil || !res.Failed || crewletmcp.RefusalOf(res) != crewletmcp.RefusalForbidden {
		t.Fatalf("an unbound token over MCP was answered: (%+v, %v)", res, err)
	}
	if model.asked() != 0 || len(model.resolved()) != 0 {
		t.Fatalf("an unbound caller spent tokens: %d model calls, %d resolutions",
			model.asked(), len(model.resolved()))
	}
	// A BOUND PERSON HOLDING NEITHER APP is refused by the table, before
	// the tool runs, naming both.
	unread := person("lee.nobody", "jane-founder")
	res, err = s.Dispatch(iam.WithPrincipal(t.Context(), unread), operator.Call{
		Transport: types.TransportMCP, Tool: builtin.AnswerKnowledgeTool,
		Args: map[string]any{"q": "How do we deploy?"},
	})
	var refused *builtin.DecisionRefused
	if err != nil || !res.Failed || model.asked() != 0 || !errors.As(res.Cause, &refused) ||
		!slices.Equal(refused.Decision.Grants, iam.Apps) {
		t.Fatalf("a bound person holding neither app was answered, or refused "+
			"other than by the table naming both apps: (%+v, %v)", res, err)
	}

	status, answer = act(t, h, "founder", builtin.AnswerKnowledgeTool,
		"application/json", askBody(), newKey())
	if status != http.StatusOK {
		t.Fatalf("the bound person was refused an answer: %d %v", status, answer)
	}
	receipt, _ := answer["receipt"].(map[string]any)
	tokens, _ := receipt["tokens"].(map[string]any)
	if !strings.Contains(receipt["answer_md"].(string), "make deploy") ||
		tokens["input"] != float64(300) || tokens["output"] != float64(40) {
		t.Errorf("the bound person's answer = %v, want the model's answer and its 340 tokens", receipt)
	}
	// THE PERSON'S SPEND, as the seam files it: the operator stage, which it
	// charges to the company's windows alone and records for the person.
	want := auxspend.Use{Stage: types.AuxStageOperator, Purpose: types.AuxAnswerKnowledge}
	if uses := model.resolved(); len(uses) != 1 || uses[0] != want {
		t.Errorf("the answer was resolved as %+v, want %+v", uses, want)
	}
}

// IT IS SERVED AS A WRITE: the act transport offers it and refuses nothing as
// `read_only_tool`, because every miss spends — and the dashboard's reads are
// refetched on focus, which would spend again each time.
func TestAnAnswerIsAnActNotARead(t *testing.T) {
	t.Parallel()
	s := answerSurface(t, &countedModel{}, &companyWindows{})
	acts, err := s.Acts(t.Context(), founder)
	if err != nil {
		t.Fatalf("Acts: %v", err)
	}
	if !slices.Contains(acts, builtin.AnswerKnowledgeTool) {
		t.Fatalf("the act transport serves %v and not answer_knowledge", acts)
	}
	if crewletmcp.ReadOnlyProven(s.Annotations(builtin.AnswerKnowledgeTool)) {
		t.Error("answer_knowledge is advertised as a proven read")
	}
}

// A SPENT COMPANY BUDGET IS A 409, and nothing is asked of the model: the
// caller's next move is to wait for the window or raise its ceiling, never to
// retry now. The person reads the catalog's words for it and the remedy naming
// the company's ceiling, which the dashboard opens; the tool's sentence, which
// names the window for a model, is not the answer's (ADR-0060).
func TestASpentBudgetAnswersBudgetExhausted(t *testing.T) {
	t.Parallel()
	model, budget := &countedModel{}, &companyWindows{spent: true}
	status, answer := act(t, guarded(answerSurface(t, model, budget)), "founder",
		builtin.AnswerKnowledgeTool, "application/json", askBody(), newKey())
	if status != http.StatusConflict || answer["error"] != string(crewletmcp.RefusalBudgetExhausted) ||
		answer["message"] != httpjson.CodeBudgetExhausted.Message() {
		t.Fatalf("a spent budget answered %d %v, want 409 budget_exhausted in its "+
			"code's own words", status, answer)
	}
	want := caveat.TokenBudgetSpent(caveat.CompanyTokenBudget())
	if answer["detail"] != want.Person() {
		t.Errorf("a spent budget answered %v, want the person's words %q",
			answer["detail"], want.Person())
	}
	if remedy, _ := answer["remedy"].(map[string]any); remedy["setting"] !=
		string(caveat.SettingCompanyTokenBudget) || len(remedy) != 1 {
		t.Errorf("a spent budget answered the remedy %v, want the company's ceiling",
			answer["remedy"])
	}
	if model.asked() != 0 {
		t.Errorf("the model was asked %d times past a spent budget", model.asked())
	}
}

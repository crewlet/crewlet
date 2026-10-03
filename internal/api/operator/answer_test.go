package operator_test

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/api/operator"
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

// countedModel answers every question and counts how often it was asked.
type countedModel struct {
	mu    sync.Mutex
	calls int
}

func (m *countedModel) Model() string { return "aux" }

func (m *countedModel) Complete(context.Context, llm.Request) (*llm.Completion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	return &llm.Completion{Model: "aux", Content: "Run `make deploy` [1].",
		InputTokens: 300, OutputTokens: 40}, nil
}

func (m *countedModel) Head(*org.Role, phase.Phase) (chain.Member, error) {
	return chain.Member{Key: "aux", Provider: m}, nil
}

func (m *countedModel) asked() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// companyWindows is a company budget that can be spent.
type companyWindows struct {
	mu      sync.Mutex
	spent   bool
	charged int
}

func (b *companyWindows) Refusing(context.Context) (builtin.BudgetRefusal, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return builtin.BudgetRefusal{Period: "day", Window: "2026-09-25", Used: 10, Limit: 10}, b.spent, nil
}

func (b *companyWindows) Charge(_ context.Context, tokens int) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.charged += tokens
	return nil
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
// nobody the spend is charged for. It needs the read grant too, since what an
// answer discloses is what the pages already show a reader. Neither refusal
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
	if model.asked() != 0 || budget.charged != 0 {
		t.Fatalf("an unbound caller spent tokens: %d model calls, %d charged",
			model.asked(), budget.charged)
	}
	// A BOUND PERSON WITHOUT THE READ GRANT is refused by the table, before
	// the tool runs.
	unread := person("lee.nobody", "jane-founder", iam.GrantWorkWrite)
	res, err = s.Dispatch(iam.WithPrincipal(t.Context(), unread), operator.Call{
		Transport: types.TransportMCP, Tool: builtin.AnswerKnowledgeTool,
		Args: map[string]any{"q": "How do we deploy?"},
	})
	if err != nil || !res.Failed || model.asked() != 0 {
		t.Fatalf("a bound person holding no read grant was answered: (%+v, %v)", res, err)
	}

	status, answer = act(t, h, "founder", builtin.AnswerKnowledgeTool,
		"application/json", askBody(), newKey())
	if status != http.StatusOK {
		t.Fatalf("the bound person was refused an answer: %d %v", status, answer)
	}
	receipt, _ := answer["receipt"].(map[string]any)
	if text, _ := receipt["answer_md"].(string); !strings.Contains(text, "make deploy") ||
		budget.charged != 340 {
		t.Errorf("the bound person's answer = %v with %d charged, want the model's answer "+
			"and its 340 tokens", receipt, budget.charged)
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

// A SPENT COMPANY BUDGET IS A 409 NAMING THE WINDOW, and nothing is asked of
// the model: the caller's next move is to wait for the window or raise its
// ceiling, never to retry now.
func TestASpentBudgetAnswersBudgetExhausted(t *testing.T) {
	t.Parallel()
	model, budget := &countedModel{}, &companyWindows{spent: true}
	status, answer := act(t, guarded(answerSurface(t, model, budget)), "founder",
		builtin.AnswerKnowledgeTool, "application/json", askBody(), newKey())
	detail, _ := answer["detail"].(string)
	if status != http.StatusConflict || answer["error"] != string(crewletmcp.RefusalBudgetExhausted) ||
		!strings.Contains(detail, "2026-09-25") {
		t.Fatalf("a spent budget answered %d %v, want 409 budget_exhausted naming the day",
			status, answer)
	}
	if model.asked() != 0 {
		t.Errorf("the model was asked %d times past a spent budget", model.asked())
	}
}

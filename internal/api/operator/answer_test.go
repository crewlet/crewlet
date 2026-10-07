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
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/events/types"
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

// answerSurface is an operator surface serving answer_knowledge, bound by
// [boundChart].
func answerSurface(t *testing.T, model *countedModel, budget *companyWindows) *operator.Server {
	t.Helper()
	return newSurface(t, operator.Options{
		Knowledge: oneRunbook{},
		Org:       boundChart,
		Answer: builtin.AnswerDeps{
			Models: model, Budget: budget,
			Actor: operator.WorkActor(boundChart),
		},
	})
}

func askBody(requestID string) string {
	raw, _ := json.Marshal(map[string]any{
		"request_id": requestID, "args": map[string]any{"q": "How do we deploy?"},
	})
	return string(raw)
}

// AN UNBOUND TOKEN CANNOT ASK, ON EITHER TRANSPORT. The act transport refuses
// it `unbound` as it refuses every write by somebody who is not a person, and
// the MCP transport — which admits an unbound credential for every other verb
// — reaches the tool, which refuses it `forbidden`: an answer spends the
// company's tokens on a person's behalf. Neither asks the model. The bound
// person beside them is answered, so a surface that refused everybody fails
// here too.
func TestAnUnboundTokenCannotAsk(t *testing.T) {
	t.Parallel()
	model, budget := &countedModel{}, &companyWindows{}
	s := answerSurface(t, model, budget)
	h := guarded(s, false)

	status, answer := act(t, h, "ci-secret", builtin.AnswerKnowledgeTool,
		"application/json", askBody(requestA))
	if status != http.StatusForbidden || answer["error"] != string(operator.CodeUnbound) {
		t.Fatalf("an unbound token over act answered %d %v, want 403 unbound", status, answer)
	}
	res, served, err := s.Dispatch(auth.WithOperator(t.Context(), "ci"),
		builtin.AnswerKnowledgeTool, map[string]any{"q": "How do we deploy?"})
	if err != nil || !served || !res.Failed || res.Refusal != crewletmcp.RefusalForbidden {
		t.Fatalf("an unbound token over MCP was answered: (%+v, %v, %v)", res, served, err)
	}
	if model.asked() != 0 || len(model.resolved()) != 0 {
		t.Fatalf("an unbound caller spent tokens: %d model calls, %d resolutions",
			model.asked(), len(model.resolved()))
	}

	status, answer = act(t, h, "founder-secret", builtin.AnswerKnowledgeTool,
		"application/json", askBody(requestB))
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
	if !slices.Contains(s.Acts(), builtin.AnswerKnowledgeTool) {
		t.Fatalf("the act transport serves %v and not answer_knowledge", s.Acts())
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
	status, answer := act(t, guarded(answerSurface(t, model, budget), false), "founder-secret",
		builtin.AnswerKnowledgeTool, "application/json", askBody(requestA))
	if status != http.StatusConflict || answer["error"] != string(crewletmcp.RefusalBudgetExhausted) ||
		!strings.Contains(answer["detail"].(string), "2026-09-25") {
		t.Fatalf("a spent budget answered %d %v, want 409 budget_exhausted naming the day",
			status, answer)
	}
	if model.asked() != 0 {
		t.Errorf("the model was asked %d times past a spent budget", model.asked())
	}
}

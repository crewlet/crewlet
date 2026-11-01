package assistanttest

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/evals"
	"github.com/crewlet/crewlet/internal/assistant/exec"
)

// Tool is the catalogue's own tool of that name, the one a suite drives a
// turn through: its schema, its routes, its card and its preview are the ones
// a running node uses, so a suite cannot pass on a tool of its own invention.
func Tool(t testing.TB, name string) catalog.Tool {
	t.Helper()
	tool, ok := catalog.Lookup(name)
	if !ok {
		t.Fatalf("assistanttest: the catalogue has no tool %q", name)
	}
	return tool
}

// FullMeter is a meter whose company window is already full: every call is
// refused before it is made.
type FullMeter struct {
	ResetsAt time.Time
	// Scope is the scope the full window is the company's or a person's
	// ([assistant.PersonBudgetScope]) in; empty is the company's, "org".
	Scope string
}

// Spend implements [toolloop.BudgetMeter].
func (m FullMeter) Spend(context.Context, int) (toolloop.SpendOutcome, error) {
	return m.outcome(), nil
}

// Refused implements [toolloop.BudgetMeter].
func (m FullMeter) Refused(context.Context) (toolloop.SpendOutcome, bool) {
	return m.outcome(), true
}

func (m FullMeter) outcome() toolloop.SpendOutcome {
	scope := m.Scope
	if scope == "" {
		scope = "org"
	}
	return toolloop.SpendOutcome{Scope: scope, Used: 1000, Limit: 1000, Period: "day",
		Window: "2026-10-24", ResetsAt: m.ResetsAt}
}

// Task is GET /worklet/{id}'s answer for one task, as the route writes it.
func Task(key, title, body string) exec.Response {
	return evals.JSON(map[string]any{"task": map[string]any{"id": evals.ID("task/" + key).String(),
		"key": key, "title": title, "status": "todo", "priority": "none",
		"assignee": "ada", "reporter": "grace", "project": "ENG", "type": "task",
		"body": body, "version": 3, "updated_at": evals.Epoch.Format(time.RFC3339)}})
}

// Routes answers each request by its method and path, "GET /worklet/ENG-7", and
// 404 in the engine's envelope for any other.
func Routes(answers map[string]exec.Response) func(exec.Request) exec.Response {
	return func(r exec.Request) exec.Response {
		if resp, ok := answers[r.Method+" "+r.Path]; ok {
			return resp
		}
		return exec.Response{Status: http.StatusNotFound,
			Body: []byte(`{"error":"not_found","message":"There is no such record here."}`)}
	}
}

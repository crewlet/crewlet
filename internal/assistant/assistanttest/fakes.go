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
	"github.com/crewlet/crewlet/internal/iam"
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

// API is the generic API tools (describe_api, read_api, propose_api_change)
// over routes, offered to p: the tools a running node builds over its route
// table, each route here deciding inside its handler as a suite's fake
// answers it.
func API(t testing.TB, p iam.Principal, routes ...string) []catalog.Tool {
	t.Helper()
	rs := make([]catalog.Route, 0, len(routes))
	for _, r := range routes {
		rs = append(rs, catalog.Route{Pattern: r})
	}
	table, err := catalog.NewRouteTable(rs)
	if err != nil {
		t.Fatalf("assistanttest: %v", err)
	}
	return catalog.GenericTools(table, p, nil)
}

// ReadAPI is the read_api tool over routes ([API]).
func ReadAPI(t testing.TB, routes ...string) catalog.Tool {
	t.Helper()
	for _, tool := range API(t, iam.Principal{}, routes...) {
		if tool.Name == catalog.ReadAPITool {
			return tool
		}
	}
	t.Fatalf("assistanttest: the generic tools hold no %s", catalog.ReadAPITool)
	return catalog.Tool{}
}

// ReadAPIFor is the read_api tool offered to p over routes as a running node's
// table holds them, each with its verb or the apps a route that decides inside
// reads, so a read is tagged with the apps p drew on.
func ReadAPIFor(t testing.TB, p iam.Principal, routes ...catalog.Route) catalog.Tool {
	t.Helper()
	table, err := catalog.NewRouteTable(routes)
	if err != nil {
		t.Fatalf("assistanttest: %v", err)
	}
	for _, tool := range catalog.GenericTools(table, p, nil) {
		if tool.Name == catalog.ReadAPITool {
			return tool
		}
	}
	t.Fatalf("assistanttest: the generic tools hold no %s", catalog.ReadAPITool)
	return catalog.Tool{}
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

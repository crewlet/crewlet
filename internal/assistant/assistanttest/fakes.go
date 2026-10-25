package assistanttest

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/assistant"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// Grant is a grant handle as a suite holds one: an id, an expiry, and a
// sentinel token the suite may plant to prove it never reaches a record, a
// log or a prompt.
type Grant struct {
	GrantID   uuid.UUID
	ExpiresAt time.Time
	Token     string
}

var _ exec.Grant = Grant{}

// ID implements [exec.Grant].
func (g Grant) ID() uuid.UUID { return g.GrantID }

// Expires implements [exec.Grant].
func (g Grant) Expires() time.Time { return g.ExpiresAt }

// Dispatcher answers each request with what its handler says and records
// every request with the grant it rode, so a suite asserts the assistant
// reached its tools as requests and nothing else.
type Dispatcher struct {
	Handler func(exec.Request) exec.Response

	mu   sync.Mutex
	sent []Sent
}

// Sent is one request a dispatcher was handed.
type Sent struct {
	Grant   uuid.UUID
	Request exec.Request
}

var _ exec.Dispatcher = (*Dispatcher)(nil)

// Do implements [exec.Dispatcher].
func (d *Dispatcher) Do(ctx context.Context, g exec.Grant, r exec.Request) (exec.Response, error) {
	if err := ctx.Err(); err != nil {
		return exec.Response{}, err
	}
	d.mu.Lock()
	d.sent = append(d.sent, Sent{Grant: g.ID(), Request: r})
	d.mu.Unlock()
	if d.Handler == nil {
		return exec.Response{Status: http.StatusNotFound,
			Body: []byte(`{"error":"not_found","message":"There is no such record here."}`)}, nil
	}
	return d.Handler(r), nil
}

// Sent is every request the dispatcher was handed.
func (d *Dispatcher) Sent() []Sent {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.sent)
}

// JSON is a 200 answer carrying v.
func JSON(v any) exec.Response {
	raw, _ := json.Marshal(v)
	return exec.Response{Status: http.StatusOK, Header: http.Header{}, Body: raw}
}

// Revoker records every grant and turn revoked.
type Revoker struct {
	mu     sync.Mutex
	grants []uuid.UUID
	turns  []uuid.UUID
}

var _ assistant.Revoker = (*Revoker)(nil)

// Revoke implements [assistant.Revoker].
func (r *Revoker) Revoke(id uuid.UUID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.grants = append(r.grants, id)
}

// RevokeTurn implements [assistant.Revoker].
func (r *Revoker) RevokeTurn(turn uuid.UUID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.turns = append(r.turns, turn)
}

// Revoked reports whether a grant was revoked.
func (r *Revoker) Revoked(id uuid.UUID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Contains(r.grants, id)
}

// Catalogue offers a fixed list of tools to everybody.
type Catalogue []catalog.Tool

// Offered implements [assistant.Catalogue].
func (c Catalogue) Offered(iam.Principal, []string) []catalog.Tool { return slices.Clone(c) }

// Tool is the catalogue's own tool of that name, the one a suite drives a
// turn through: its schema, its routes, its card and its preview are the ones
// a running node uses, so a suite cannot pass on a tool of its own invention.
func Tool(t TB, name string) catalog.Tool {
	t.Helper()
	tool, ok := catalog.Lookup(name)
	if !ok {
		t.Fatalf("assistanttest: the catalogue has no tool %q", name)
	}
	return tool
}

// Models answers every chain with one model: a scripted [Model] in a suite,
// or a real providers.llm entry when `crewlet assistant eval -model` measures
// one.
type Models struct {
	Model llm.Provider
	Key   string
	Err   error
}

// Chain implements [assistant.Models].
func (m Models) Chain(context.Context, iam.Principal, auxspend.Use) (llm.Provider, string, error) {
	if m.Err != nil {
		return nil, "", m.Err
	}
	return m.Model, m.Key, nil
}

// Budget hands every turn one meter, or none.
type Budget struct{ M toolloop.BudgetMeter }

// Meter implements [assistant.Budget].
func (b Budget) Meter(context.Context, iam.Principal) (toolloop.BudgetMeter, error) {
	return b.M, nil
}

// FullMeter is a meter whose company window is already full: every call is
// refused before it is made.
type FullMeter struct{ ResetsAt time.Time }

// Spend implements [toolloop.BudgetMeter].
func (m FullMeter) Spend(context.Context, int) (toolloop.SpendOutcome, error) {
	return m.outcome(), nil
}

// Refused implements [toolloop.BudgetMeter].
func (m FullMeter) Refused(context.Context) (toolloop.SpendOutcome, bool) {
	return m.outcome(), true
}

func (m FullMeter) outcome() toolloop.SpendOutcome {
	return toolloop.SpendOutcome{Scope: "org", Used: 1000, Limit: 1000, Period: "day",
		Window: "2026-10-24", ResetsAt: m.ResetsAt}
}

// Audit keeps every event published.
type Audit struct {
	mu     sync.Mutex
	events []*events.Event
}

// Publish implements [assistant.AuditPublisher].
func (a *Audit) Publish(_ context.Context, _ string, ev *events.Event) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, ev)
	return nil
}

// Events is every event published.
func (a *Audit) Events() []*events.Event {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.events)
}

// Metrics records nothing a suite asserts on.
type Metrics struct{}

// Add implements [assistant.Metrics].
func (Metrics) Add(string, uint64, metrics.Attrs) {}

// Observe implements [assistant.Metrics].
func (Metrics) Observe(string, time.Duration, metrics.Attrs) {}

// Set implements [assistant.Metrics].
func (Metrics) Set(string, float64, metrics.Attrs) {}

// Task is GET /work/{id}'s answer for one task, as the route writes it.
func Task(key, title, body string) exec.Response {
	return JSON(map[string]any{"task": map[string]any{"id": ID("task/" + key).String(),
		"key": key, "title": title, "status": "todo", "priority": "none",
		"assignee": "ada", "reporter": "grace", "project": "ENG", "type": "task",
		"body": body, "version": 3, "updated_at": Epoch.Format(time.RFC3339)}})
}

// Routes answers each request by its method and path, "GET /work/ENG-7", and
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

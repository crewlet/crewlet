package assistanttest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/assistant"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/wire"
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

func (g Grant) ID() uuid.UUID      { return g.GrantID }
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

func (r *Revoker) Revoke(id uuid.UUID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.grants = append(r.grants, id)
}

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

func (c Catalogue) Offered(iam.Principal, []string) []catalog.Tool { return slices.Clone(c) }

// Cards cards every answer as one table of its rows: the answer is a JSON
// array of objects, and the columns are Columns.
type Cards struct {
	Title   string
	Columns []string
	At      time.Time
}

var _ assistant.Cards = Cards{}

func (c Cards) Read(t catalog.Tool, _ map[string]any, r exec.Response, at time.Time) (assistant.Carded, error) {
	var rows []map[string]any
	if err := json.Unmarshal(r.Body, &rows); err != nil {
		return assistant.Carded{}, fmt.Errorf("assistanttest: %s answered no rows: %w", t.Name, err)
	}
	table := &wire.TableCard{Title: c.Title, RowCount: len(rows), AsOf: at.UTC(),
		Rows: [][]any{}}
	for _, col := range c.Columns {
		table.Columns = append(table.Columns, wire.Column{Key: col, Label: col,
			Type: wire.ColumnText})
	}
	var summary string
	for _, row := range rows {
		cells := make([]any, 0, len(c.Columns))
		for _, col := range c.Columns {
			cells = append(cells, row[col])
			summary += fmt.Sprintf("%s=%v ", col, row[col])
		}
		table.Rows = append(table.Rows, cells)
		summary += "\n"
	}
	return assistant.Carded{Cards: []wire.Card{{Type: wire.CardTable, Table: table}},
		Summary: summary}, nil
}

func (c Cards) Navigate(catalog.Tool, map[string]any) (wire.Link, *exec.Request, error) {
	return wire.Link{}, nil, fmt.Errorf("assistanttest: no link")
}

// ReadTool is a read tool on one route: `GET <path>`, whose answer carries
// text somebody wrote when fence is set.
func ReadTool(name, path string, fence bool, apps ...iam.Grant) catalog.Tool {
	return catalog.Tool{
		Name: name, Description: "Reads " + path + ".", Phrase: "read this",
		Parameters: map[string]any{"type": "object", "properties": map[string]any{
			"q": map[string]any{"type": "string"}}},
		Kind: catalog.KindRead, Tier: catalog.TierRead, Routes: []string{"GET " + path},
		Apps: apps, Fence: fence,
		Label: func(map[string]any) string { return "Read " + path },
		Build: func(args map[string]any) (exec.Request, error) {
			return exec.Request{Method: http.MethodGet, Path: path}, nil
		},
	}
}

// Models answers every chain with one model.
type Models struct {
	Model *Model
	Key   string
	Err   error
}

func (m Models) Chain(context.Context, iam.Principal, auxspend.Use) (llm.Provider, string, error) {
	if m.Err != nil {
		return nil, "", m.Err
	}
	return m.Model, m.Key, nil
}

// Budget hands every turn one meter, or none.
type Budget struct{ M toolloop.BudgetMeter }

func (b Budget) Meter(context.Context, iam.Principal) (toolloop.BudgetMeter, error) {
	return b.M, nil
}

// FullMeter is a meter whose company window is already full: every call is
// refused before it is made.
type FullMeter struct{ ResetsAt time.Time }

func (m FullMeter) Spend(context.Context, int) (toolloop.SpendOutcome, error) {
	return m.outcome(), nil
}

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

func (Metrics) Add(string, uint64, metrics.Attrs)            {}
func (Metrics) Observe(string, time.Duration, metrics.Attrs) {}
func (Metrics) Set(string, float64, metrics.Attrs)           {}

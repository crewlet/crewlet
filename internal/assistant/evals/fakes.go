package evals

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
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
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
	resp := exec.Response{Status: http.StatusNotFound,
		Body: []byte(`{"error":"not_found","message":"There is no such record here."}`)}
	if d.Handler != nil {
		resp = d.Handler(r)
	}
	// CUT AS THE REAL DISPATCHER CUTS ([exec.Request.Bound]), so a suite
	// whose handler answers a long record meets the bound a turn would.
	if bound := r.Bound(); len(resp.Body) > bound {
		resp.Body, resp.Truncated = resp.Body[:bound], true
	}
	if resp.Status == http.StatusNotFound && r.Method == http.MethodGet && r.Path == "/org" {
		// THE COMPANY EVERY RIG RUNS IN, for a handler that names no chart
		// of its own: on UTC, with no seats, which is what a task's preview
		// reads its clock and its seat names from.
		return JSON(map[string]any{"timezone": "UTC"}), nil
	}
	if resp.Status == http.StatusNotFound && r.Method == http.MethodGet && r.Path == "/viewer" {
		// AND THE PERSON EVERY RIG ASKS AS, for a handler that names none:
		// a seat whose team owns the RIG project and space, which is where a
		// create's preview resolves a project or container its call omits.
		return JSON(map[string]any{"handle": "rig", "project": "RIG", "space": "RIG"}), nil
	}
	return resp, nil
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

// RevokedTurn reports whether every grant of a turn was ended.
func (r *Revoker) RevokedTurn(turn uuid.UUID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Contains(r.turns, turn)
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

// Unready implements [assistant.Models] the engine's way: the chain's verdict
// over its one member, so a model that knows it would fail says so.
func (m Models) Unready(ctx context.Context) error {
	if m.Model == nil {
		return nil
	}
	return chain.Unready(ctx, []llm.Provider{m.Model})
}

// Budget hands every turn one meter, or none.
type Budget struct{ M toolloop.BudgetMeter }

// Meter implements [assistant.Budget].
func (b Budget) Meter(context.Context, iam.Principal) (toolloop.BudgetMeter, error) {
	return b.M, nil
}

// Audit keeps every event published.
type Audit struct {
	mu     sync.Mutex
	events []*events.Event
	hold   chan struct{}
}

// Hold makes every publish wait until release closes (or its context ends),
// so a suite can act while a runner is still tearing down.
func (a *Audit) Hold(release chan struct{}) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.hold = release
}

// Publish implements [assistant.AuditPublisher].
func (a *Audit) Publish(ctx context.Context, _ string, ev *events.Event) error {
	a.mu.Lock()
	hold := a.hold
	a.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
		}
	}
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

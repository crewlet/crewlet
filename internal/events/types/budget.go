package types

import (
	"fmt"

	"github.com/crewlet/crewlet/internal/events"
)

// Token budgets: the cap being hit, and the live meters behind it.

func init() {
	events.Register[BudgetExhausted]()
	events.Register[BudgetReported]()
}

// BudgetScope is whose budget a figure belongs to.
type BudgetScope string

// The two levels a token cap is kept at: one seat's own allowance, and the
// company-wide pool every seat charges against. A call is checked against both,
// so either can be the scope that refuses it.
const (
	BudgetScopeAgent BudgetScope = "agent"
	BudgetScopeOrg   BudgetScope = "org"
)

// BudgetExhausted fires when an agent or the org exceeds its token budget. Its
// type is in FailureEventTypes: a refused charge is a failure whatever the
// payload says.
type BudgetExhausted struct {
	Agent      string      `json:"agent_id"`
	RoleName   string      `json:"role"`
	BudgetType BudgetScope `json:"budget_type"`
	UsedTokens int         `json:"used_tokens"`
	MaxTokens  int         `json:"max_tokens"`
}

// EventType is the "budget_exhausted" wire type, and one of the four names in
// FailureEventTypes.
func (BudgetExhausted) EventType() string { return "budget_exhausted" }

// Role is the seat whose charge was refused, which is the seat a dashboard
// shows the exhaustion against even for an org-scoped cap.
func (e BudgetExhausted) Role() string    { return e.RoleName }
// AgentID is the instance that made the refused call.
func (e BudgetExhausted) AgentID() string { return e.Agent }

// SummaryFor names the scope, since an org cap and a per-seat cap are refused
// on the same seat and read identically otherwise.
func (e BudgetExhausted) SummaryFor(actor string) string {
	return lead(actor, "exhausted "+string(e.BudgetType)+" token budget")
}

// BudgetMeter is one metered seat inside a BudgetReported snapshot.
//
// Only seats with a per-agent budget appear: the engine seeds a meter solely
// for a non-zero role token budget, so absence means "no cap and no meter" — a
// different fact from a cap of zero, and one a consumer must not draw as an
// empty bar.
type BudgetMeter struct {
	AgentID string `json:"agent_id"`
	// Role rides along because the engine already knows it and every consumer
	// is keyed by role. Re-deriving it from AgentID would be a second identity
	// path, and the two diverge after a live handle edit.
	Role       string `json:"role"`
	UsedTokens int    `json:"used_tokens"`
	MaxTokens  int    `json:"max_tokens"`
	// RefusedAt is when the cap last turned a charge away (ISO 8601). That, not
	// UsedTokens >= MaxTokens, is what "exhausted" means: a refused charge
	// increments nothing, so the counter stops short of the cap by the size of
	// the round that would not fit.
	RefusedAt string `json:"refused_at"`
}

// BudgetReported is a snapshot of every live token meter, for the dashboard.
//
// Published on a fixed tick whenever a counter moved. It exists because the
// counters live nowhere but in the engine's own budget manager: they are not
// derivable from any other event, and the two figures a dashboard already has —
// the 7-day per-agent total and the 24-hour spend rollup — cover different spans
// and cannot substitute.
//
// Deliberately NOT persisted. It is a LIVE meter, so replaying one out of
// history would show a dead engine's counters as the current ones. Being an
// ordinary event anyway is what makes it work on both deployment shapes, with
// no second transport.
type BudgetReported struct {
	// MeterID identifies the reporting meter's RUN. UsedTokens is comparable
	// only within one MeterID: a new id means the engine restarted and every
	// prior figure is dead, so consumers must REPLACE what they hold, never
	// merge or take a maximum, or a restart pins a phantom high-water mark
	// forever.
	MeterID string `json:"meter_id"`
	// Seq is monotonic within MeterID, and the reorder guard. Broker ordering
	// holds only within a topic and a broadcast subscription reads across all
	// of them, so an older report can arrive after a newer one and walk the
	// meter backwards.
	Seq           int           `json:"seq"`
	OrgUsedTokens int           `json:"org_used_tokens"`
	OrgMaxTokens  int           `json:"org_max_tokens"`
	OrgRefusedAt  string        `json:"org_refused_at"`
	Agents        []BudgetMeter `json:"agents,omitempty"`
}

// EventType is the "budget_reported" wire type.
func (BudgetReported) EventType() string { return "budget_reported" }

// Summary counts the metered seats rather than leading with an actor: the
// snapshot is the engine's, not any one seat's.
func (e BudgetReported) Summary() string {
	return fmt.Sprintf("Token meters reported for %d agents", len(e.Agents))
}

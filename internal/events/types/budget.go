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

// BudgetExhausted fires when a token budget stops a seat's work: a charge the
// gate refused, a round not sent because a scope was already at or past its
// cap, or an answer a detached coding run's resume holds until the budget has
// room. Its type is in FailureEventTypes: a refused charge is a failure
// whatever the payload says.
type BudgetExhausted struct {
	Agent    string `json:"agent_id"`
	RoleName string `json:"role"`
	// TurnID is the RUN whose work was stopped, and WorkKey the unit of
	// work behind it. Both, because this is one of the three types a seat
	// goes AFK on, and a failure record that could be attributed to no turn
	// would be the one an operator opens the turn to understand. See
	// ADR-0017.
	TurnID     string      `json:"turn_id,omitempty"`
	WorkKey    string      `json:"work_key,omitempty"`
	BudgetType BudgetScope `json:"budget_type"`
	UsedTokens int         `json:"used_tokens"`
	MaxTokens  int         `json:"max_tokens"`
}

// EventType is the "budget_exhausted" wire type, and one of the four names in
// FailureEventTypes.
func (BudgetExhausted) EventType() string { return "budget_exhausted" }

// Role is the seat whose charge was refused, which is the seat a dashboard
// shows the exhaustion against even for an org-scoped cap.
func (e BudgetExhausted) Role() string { return e.RoleName }

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
	// RefusedAt is when the cap last turned a charge away, as RFC 3339 in UTC,
	// and empty for a scope with no refusal on record. It is the shared
	// counter's own stamp (coord.Usage.RefusedAt), so every node reports the
	// same one, and it is cleared by the scope's next ADMITTED charge or by a
	// reset of the counter — by nothing else.
	//
	// IT DATES A REFUSAL; IT DOES NOT SAY THE SCOPE IS REFUSING NOW. A
	// revision that raises the cap leaves it standing until the scope's next
	// admitted charge, and the gate never reads it: a scope is exhausted
	// when UsedTokens is at or past MaxTokens, and only then. Refused or
	// not, the counter can read past the cap — a round the cap refuses has
	// been billed, because it is charged once its model call has answered,
	// and it is counted all the same; and spend counted after it happened,
	// a coding run's, can take a counter past its cap with no refusal at
	// all. While it reads at or past the cap, the next round the seat asks
	// for is refused before it is sent.
	RefusedAt string `json:"refused_at"`
}

// BudgetReported is a snapshot of every live token meter, for the dashboard.
//
// Published on a fixed tick by every node, from the fleet's SHARED counter.
// It exists because the dashboard's header renders the company's headroom
// from a websocket push: a screen open while a company works has to move as
// the company spends, and the spend rollups it already has cover windows of
// time rather than the life of the counter, so they cannot substitute.
// `GET /budgets` answers the same counters on demand, for a screen that is
// read rather than watched.
//
// Deliberately NOT persisted. It is a snapshot of a counter that moves every
// round, published every few seconds by every node, so a durable row per
// report would fill the log with readings the next report supersedes, and a
// copy replayed from history would show figures the counter left behind as
// the current ones. Being an ordinary event anyway is what makes it reach
// every node's dashboard with no second transport.
type BudgetReported struct {
	// MeterID identifies the reporting node's INCARNATION. Every node reports
	// the same shared counter, so reports under different ids describe one
	// set of figures read at different moments. A report is a complete
	// snapshot, so consumers REPLACE what they hold, never merge or take a
	// maximum, or a reset would leave a high-water mark no later report
	// could clear.
	MeterID string `json:"meter_id"`
	// Seq is monotonic within MeterID, and the reorder guard between one
	// node's reports. Broker ordering holds only within a topic and a
	// broadcast subscription reads across all of them, so an older report can
	// arrive after a newer one and walk the meter backwards. Two meters'
	// sequences are unrelated, so between nodes the envelope's timestamp,
	// which is when the counter was read, is the guard instead.
	Seq           int `json:"seq"`
	OrgUsedTokens int `json:"org_used_tokens"`
	OrgMaxTokens  int `json:"org_max_tokens"`
	// OrgRefusedAt is [BudgetMeter.RefusedAt] for the company-wide scope,
	// which is exhausted when OrgUsedTokens is at or past a non-zero
	// OrgMaxTokens — a zero OrgMaxTokens is no company-wide cap.
	OrgRefusedAt string        `json:"org_refused_at"`
	Agents       []BudgetMeter `json:"agents,omitempty"`
}

// EventType is the "budget_reported" wire type.
func (BudgetReported) EventType() string { return "budget_reported" }

// Summary counts the metered seats rather than leading with an actor: the
// snapshot is the engine's, not any one seat's.
func (e BudgetReported) Summary() string {
	return fmt.Sprintf("Token meters reported for %d agents", len(e.Agents))
}

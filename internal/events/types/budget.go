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
// type is a failure BY TYPE: a refused charge is a failure whatever the
// payload says.
type BudgetExhausted struct {
	Agent    string `json:"agent_id"`
	RoleName string `json:"role"`
	// TurnID is the RUN whose charge was refused, and WorkKey the unit of
	// work behind it. Neither was here, and this is one of the three types
	// a seat goes AFK on — so the only failure record that could be
	// attributed to no turn at all was the one an operator opens the turn
	// to understand. See ADR-0017.
	TurnID     string      `json:"turn_id,omitempty"`
	WorkKey    string      `json:"work_key,omitempty"`
	BudgetType BudgetScope `json:"budget_type"`
	UsedTokens int         `json:"used_tokens"`
	MaxTokens  int         `json:"max_tokens"`
	// Period, Window and ResetsAt are the calendar window that refused:
	// its period (`day`, `week` or `month`), its label on the company's
	// clock (`2026-09-23`, `2026-W39`, `2026-09`) and the instant it turns
	// over, as RFC 3339 in UTC. UsedTokens and MaxTokens are that window's
	// spend and ceiling. Where several windows refused it is the one that
	// ends last, which is when the scope next has room without a ceiling
	// being raised.
	//
	// ADDITIVE, and omitted rather than empty: a record from a build that
	// counted one lifetime figure has no window, and a consumer must read
	// that absence as "not stated" rather than as a window with no name.
	Period   string `json:"period,omitempty"`
	Window   string `json:"window,omitempty"`
	ResetsAt string `json:"resets_at,omitempty"`
}

// EventType is the "budget_exhausted" wire type, and one of the four
// [FailureEventNames].
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
// for a seat whose `token_budget` caps a window, so absence means "no cap and
// no meter" — a different fact from a cap of zero, and one a consumer must not
// draw as an empty bar.
//
// ONE WINDOW per meter: the scope's binding window (coord.Usage.Binding) — a
// window that is refusing, else the capped window with the least room left —
// so UsedTokens, MaxTokens and RefusedAt always describe the same calendar
// window and a cap is never drawn over another window's spend.
type BudgetMeter struct {
	AgentID string `json:"agent_id"`
	// Role rides along because the engine already knows it and every consumer
	// is keyed by role. Re-deriving it from AgentID would be a second identity
	// path, and the two diverge after a live handle edit.
	Role       string `json:"role"`
	UsedTokens int    `json:"used_tokens"`
	MaxTokens  int    `json:"max_tokens"`
	// RefusedAt is when the window last turned a charge away, as RFC 3339 in
	// UTC, and empty while it is not refusing. That, not UsedTokens >=
	// MaxTokens, is what "exhausted" means: a refused charge increments
	// nothing, so the counter stops short of the cap by the size of the round
	// that would not fit. It is the shared counter's own stamp
	// (coord.WindowUsage.RefusedAt), cleared by the scope's next admitted
	// charge and by the window turning over, so every node reports the same
	// one.
	RefusedAt string `json:"refused_at"`
}

// BudgetReported is a snapshot of every live token meter, for the dashboard.
//
// Published on a fixed tick by every node, from the fleet's SHARED counter.
// It exists because the dashboard's header renders the company's headroom
// from a websocket push: a screen open while a company works has to move as
// the company spends, and the spend rollups it already has cover windows of
// time chosen by the reader rather than the calendar window a cap is written
// for, so they cannot substitute.
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
	// maximum, or a window turning over would leave a high-water mark no
	// later report could clear.
	MeterID string `json:"meter_id"`
	// Seq is monotonic within MeterID, and the reorder guard between one
	// node's reports. Broker ordering holds only within a topic and a
	// broadcast subscription reads across all of them, so an older report can
	// arrive after a newer one and walk the meter backwards. Two meters'
	// sequences are unrelated, so between nodes the envelope's timestamp,
	// which is when the counter was read, is the guard instead.
	Seq int `json:"seq"`
	// OrgUsedTokens and OrgMaxTokens are the company's binding window, as
	// a [BudgetMeter]'s are; for a company that caps no window, its month's
	// spend under a max of 0.
	OrgUsedTokens int `json:"org_used_tokens"`
	OrgMaxTokens  int `json:"org_max_tokens"`
	// OrgRefusedAt is [BudgetMeter.RefusedAt] for the company-wide scope.
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

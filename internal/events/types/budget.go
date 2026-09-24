package types

import (
	"fmt"

	"github.com/crewlet/crewlet/internal/events"
)

// Token budgets: the cap being hit, and the live meters behind it.

func init() {
	events.Register[BudgetExhausted]()
	events.Register[BudgetMeters]()
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

// BudgetState is what a capped calendar window is doing, as the engine
// judges it. The engine computes it ONCE and every surface renders it: the
// dashboard used to hold three "nearly spent" thresholds of its own (75% on one
// screen, 90% on another and the component kit's own on a third), so one
// window read as healthy, near and full at once depending on where it was
// drawn.
type BudgetState string

// The three states a window is in.
const (
	// BudgetOK is a window with room to spare, and every window no ceiling
	// caps: with no ceiling nothing can be near it or refused by it.
	BudgetOK BudgetState = "ok"
	// BudgetNear is a capped window whose spend has reached the engine's
	// near fraction of its ceiling (engine.BudgetNearFraction, served
	// beside every answer that carries a state).
	BudgetNear BudgetState = "near"
	// BudgetRefusing is a capped window the gate is turning charges away
	// in: it has refused one since it began, or it has no room left for a
	// single token. The same predicate the budget park waits on, so a
	// seat is never parked under a meter that reads as merely near.
	BudgetRefusing BudgetState = "refusing"
)

// Valid reports whether s is one of the three states.
func (s BudgetState) Valid() bool {
	switch s {
	case BudgetOK, BudgetNear, BudgetRefusing:
		return true
	}
	return false
}

// BudgetWindow is one calendar window of one scope's token counter: what it
// has spent, against which ceiling, and what that means.
type BudgetWindow struct {
	// Period is `day`, `week` or `month`; Window is its label on the
	// company's clock (`2026-09-23`, `2026-W39`, `2026-09`), which is the
	// window's identity on every node.
	Period string `json:"period"`
	Window string `json:"window"`
	// StartsAt and ResetsAt are the window's half-open span, RFC 3339 in
	// UTC: ResetsAt is when its allowance comes back without a ceiling
	// being raised. Sent rather than left to a client to derive from the
	// label, because a client would cut it on the browser's zone.
	StartsAt string `json:"starts_at"`
	ResetsAt string `json:"resets_at"`
	Used     int    `json:"used"`
	// Limit is the window's ceiling, and ABSENT where nothing caps it —
	// never 0, which would state a range of nothing that is already full.
	Limit *int `json:"limit,omitempty"`
	// RefusedAt is when the window last turned a charge away, RFC 3339 in
	// UTC, and absent while it has not. That, not Used against Limit, is
	// the gate's own record of saying no: a refused charge increments
	// nothing, so a counter charged in rounds stops short of its ceiling
	// by the round that did not fit. The shared counter's stamp
	// (coord.WindowUsage.RefusedAt), cleared by the scope's next admitted
	// charge and by the window turning over, so every node reports the
	// same one. Carried only for a capped window: a stamp left on a window
	// whose ceiling has since been removed is a refusal by a ceiling that
	// no longer exists, and the next charge clears it.
	RefusedAt string `json:"refused_at,omitempty"`
	// State is the engine's judgement of the window; see [BudgetState].
	State BudgetState `json:"state"`
}

// BudgetScopeMeter is one scope's capped windows.
type BudgetScopeMeter struct {
	// Windows lists only the CAPPED windows, in day, week, month order,
	// and is empty rather than absent for a scope that caps none: "no
	// ceiling, no bar" is a different fact from a ceiling of zero, and a
	// consumer must not draw it as an empty bar.
	Windows []BudgetWindow `json:"windows"`
}

// BudgetSeatMeter is one metered seat inside a [BudgetMeters] snapshot.
//
// Only seats whose own `token_budget` caps a window appear, so absence means
// "no ceiling and no meter".
type BudgetSeatMeter struct {
	AgentID string `json:"agent_id"`
	// Role and Handle ride along because the engine already knows them
	// and every consumer is keyed by one or the other. Re-deriving them
	// from AgentID would be a second identity path, and the two diverge
	// after a live handle edit.
	Role    string         `json:"role"`
	Handle  string         `json:"handle"`
	Windows []BudgetWindow `json:"windows"`
}

// BudgetMeters is a snapshot of every live token meter, for the dashboard.
//
// Published on a fixed tick by every node, from the fleet's SHARED counters.
// It exists because the dashboard's header renders the company's headroom
// from a websocket push: a screen open while a company works has to move as
// the company spends, and the spend rollups it already has cover windows of
// time chosen by the reader rather than the calendar windows a ceiling is
// written for, so they cannot substitute. The `budgets` query answers the
// same counters on demand, for a screen that is read rather than watched.
//
// EVERY CAPPED WINDOW, not one figure per scope. Its predecessor
// (`budget_reported`, deleted with this type's arrival — no release carried
// it) sent each scope's single binding window, so a seat capped by the day
// and by the month showed one bar that jumped between them, and the live
// projection dropped the refusal stamp on the floor on its way to the push.
//
// Deliberately NOT persisted. It is a snapshot of a counter that moves every
// round, published every few seconds by every node, so a durable row per
// report would fill the log with readings the next report supersedes, and a
// copy replayed from history would show figures the counter left behind as
// the current ones. Being an ordinary event anyway is what makes it reach
// every node's dashboard with no second transport.
type BudgetMeters struct {
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
	// Timezone is the company's clock the windows were cut on (ADR-0018),
	// the IANA name or `UTC`.
	Timezone string `json:"timezone"`
	// Org is the company-wide scope's capped windows.
	Org   BudgetScopeMeter  `json:"org"`
	Seats []BudgetSeatMeter `json:"seats,omitempty"`
}

// EventType is the "budget_meters" wire type.
func (BudgetMeters) EventType() string { return "budget_meters" }

// Summary counts the metered seats rather than leading with an actor: the
// snapshot is the engine's, not any one seat's.
func (e BudgetMeters) Summary() string {
	return fmt.Sprintf("Token meters reported for %d agents", len(e.Seats))
}

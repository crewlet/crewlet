// The caps the engine holds the company's work to, as it enforces them.

package queries

import (
	"context"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
)

// LimitGroup is which part of the company's work a limit bounds.
type LimitGroup string

// The groups, in the order a screen draws them.
const (
	LimitTurn          LimitGroup = "turn"
	LimitDelegation    LimitGroup = "delegation"
	LimitNotifications LimitGroup = "notifications"
	LimitSchedules     LimitGroup = "schedules"
	// LimitDelegated is what every delegate of a delegated decision is held
	// to (delegated.Limits), answered on `delegated_decisions` beside the
	// rules it bounds, never on `limits`.
	LimitDelegated LimitGroup = "delegated"
	// LimitInbox, LimitAtlas and LimitCode are the settings the Worklet,
	// Atlas and Gitlet screens change in place, answered on `limits` and
	// drawn there rather than on Settings › Limits.
	LimitInbox LimitGroup = "inbox"
	LimitAtlas LimitGroup = "atlas"
	LimitCode  LimitGroup = "code"
	// LimitChat is Chatlet's company-wide message horizon, which its own
	// screen changes in place.
	LimitChat LimitGroup = "chat"
	// LimitSignIn is the company's sign-in policy, answered on
	// `sign_in_policy` with each range narrowed to the node's limits.
	LimitSignIn LimitGroup = "sign_in"
)

// LimitGroups are every group, in drawing order. Settings › Limits draws the
// first four; `delegated_decisions` answers the delegated rows.
var LimitGroups = []LimitGroup{LimitTurn, LimitDelegation, LimitNotifications, LimitSchedules, LimitDelegated,
	LimitInbox, LimitAtlas, LimitCode, LimitChat, LimitSignIn}

// LimitUnit is what a limit's number counts.
type LimitUnit string

// The units.
const (
	// LimitCount is a whole number of things: iterations, rounds, workers,
	// tasks, events.
	LimitCount LimitUnit = "count"
	// LimitSeconds is wall-clock time.
	LimitSeconds LimitUnit = "seconds"
	// LimitFraction is a share between 0 and 1.
	LimitFraction LimitUnit = "fraction"
	// LimitPerSecond is a rate.
	LimitPerSecond LimitUnit = "per_second"
	// LimitDays is a whole number of days: a retention, an idle time.
	LimitDays LimitUnit = "days"
)

// LimitUnits are the five.
var LimitUnits = []LimitUnit{LimitCount, LimitSeconds, LimitFraction, LimitPerSecond, LimitDays}

// LimitRow is one cap: where the company document sets it, the value the
// engine enforces, and the engine's default for it.
//
// THE VALUE IS THE ENFORCED ONE. A company document is decoded over
// [config.DefaultCompany], so a cap it leaves out is the default in the applied
// company. A screen draws `value` and never re-derives a default: a dashboard
// copy of [config.DefaultTurnEngine] is the drift this answer prevents.
//
// THE DEFAULT IS BESIDE IT, never a "set" flag: a document writing the default
// is indistinguishable from one writing nothing, so a screen can only say
// whether the value differs from the engine's default.
type LimitRow struct {
	// Key is the setting's path in the company document, as
	// `crewlet validate` and `PATCH /config` name it.
	Key   string     `json:"key"`
	Group LimitGroup `json:"group"`
	Unit  LimitUnit  `json:"unit"`
	// Value is what the engine enforces.
	Value float64 `json:"value"`
	// Default is what it enforces when the document says nothing.
	Default float64 `json:"default"`
	// Off is whether Value switches the limit off rather than bounding
	// anything: a `notification_rate_limit` of 0 is no rate limit, an
	// `onboarding_max_tool_rounds` of 0 is no onboarding pass, and a token
	// floor of 0 is no floor.
	Off bool `json:"off"`
	// DefaultOff is the same question asked of Default, so a screen that
	// says which default a set value replaced says Off where the default is
	// no limit, rather than keeping its own copy of which zeros are off.
	DefaultOff bool `json:"default_off"`
	// Settable is whether Key is a setting of the company document, so a
	// write of it (`PATCH /config`) changes Value. False for a cap the
	// engine holds as a constant and reports under a path no document
	// carries: the schedule timeout, which each schedule sets for itself.
	// A screen offers a change exactly where this is true, rather than
	// keeping its own list of which keys the document has.
	Settable bool `json:"settable"`
	// Range is the values the engine's validator admits for Key
	// ([config.RangeOf]), so a screen changing the cap says a value outside
	// it beside the field before anything is sent ("At least 1."), rather
	// than sending it for the engine to refuse in its operator's words. The
	// validator reads the same declaration, so the two cannot disagree.
	// Present on every settable row; absent on a constant, which no write
	// changes.
	Range *config.Range `json:"range,omitempty"`
}

// LimitsAnswer is what `limits` answers.
type LimitsAnswer struct {
	Limits []LimitRow `json:"limits"`
}

// limit declares one row: how to read it off a company, and whether zero
// switches it off.
type limit struct {
	key       string
	group     LimitGroup
	unit      LimitUnit
	read      func(*config.Company) float64
	zeroIsOff bool
	// constant marks a row read from the engine itself rather than from
	// the company: no document key sets it.
	constant bool
}

// limits is every cap the answer reports, in the order a screen draws them.
//
// ONE TABLE, each row read through one getter for both the applied company
// and [config.DefaultCompany], so a row's value and its default can never be
// read from two different fields.
var limits = []limit{
	{key: "turn_engine.max_iterations", group: LimitTurn, unit: LimitCount,
		read: func(c *config.Company) float64 { return float64(c.TurnEngine.MaxIterations) }},
	{key: "turn_engine.onboarding_max_tool_rounds", group: LimitTurn, unit: LimitCount,
		zeroIsOff: true,
		read: func(c *config.Company) float64 {
			return float64(c.TurnEngine.OnboardingRounds())
		}},
	{key: "turn_engine.extension_round_step", group: LimitTurn, unit: LimitCount,
		read: func(c *config.Company) float64 { return float64(c.TurnEngine.ExtensionRoundStep) }},
	{key: "turn_engine.delegation_depth_limit", group: LimitTurn, unit: LimitCount,
		read: func(c *config.Company) float64 { return float64(c.TurnEngine.DelegationDepthLimit) }},
	{key: "turn_engine.executor_min_budget_tokens", group: LimitTurn, unit: LimitCount,
		zeroIsOff: true,
		read:      func(c *config.Company) float64 { return float64(c.TurnEngine.ExecutorFloor()) }},
	{key: "turn_engine.delegation.max_parallel", group: LimitDelegation, unit: LimitCount,
		read: func(c *config.Company) float64 { return float64(c.TurnEngine.Delegation.MaxParallel) }},
	{key: "turn_engine.delegation.max_tasks_per_call", group: LimitDelegation, unit: LimitCount,
		read: func(c *config.Company) float64 {
			return float64(c.TurnEngine.Delegation.MaxTasksPerCall)
		}},
	{key: "turn_engine.delegation.max_turns", group: LimitDelegation, unit: LimitCount,
		read: func(c *config.Company) float64 { return float64(c.TurnEngine.Delegation.MaxTurns) }},
	// THE CEILING "Turns per worker" is refused above, drawn beside it so a
	// person told "can be at most N" can see and raise N.
	{key: "turn_engine.delegation.max_turns_ceiling", group: LimitDelegation, unit: LimitCount,
		read: func(c *config.Company) float64 { return float64(c.TurnEngine.Delegation.MaxTurnsCeiling) }},
	{key: "turn_engine.delegation.min_tokens_per_task", group: LimitDelegation, unit: LimitCount,
		zeroIsOff: true,
		read:      func(c *config.Company) float64 { return float64(c.TurnEngine.Delegation.PerTaskFloor()) }},
	{key: "turn_engine.delegation.budget_fraction", group: LimitDelegation, unit: LimitFraction,
		read: func(c *config.Company) float64 { return c.TurnEngine.Delegation.BudgetFraction }},
	{key: "turn_engine.delegation.task_timeout_seconds", group: LimitDelegation, unit: LimitSeconds,
		read: func(c *config.Company) float64 { return c.TurnEngine.Delegation.TaskTimeoutSeconds }},
	{key: "turn_engine.delegation.call_timeout_seconds", group: LimitDelegation, unit: LimitSeconds,
		read: func(c *config.Company) float64 { return c.TurnEngine.Delegation.CallTimeoutSeconds }},
	{key: "notification_rate_limit", group: LimitNotifications, unit: LimitPerSecond,
		zeroIsOff: true,
		read:      func(c *config.Company) float64 { return float64(c.NotificationRateLimit) }},
	{key: "notification_coalesce_max_batch", group: LimitNotifications, unit: LimitCount,
		read: func(c *config.Company) float64 { return float64(c.NotificationCoalesceMaxBatch) }},
	// THE TIMEOUT EVERY SCHEDULE TAKES unless it sets its own
	// `timeout_seconds`. There is no company-wide setting for it, so the
	// value is always the engine's: a screen shows it as the default a
	// schedule inherits, and each schedule's own is on `schedules`.
	{key: "schedules.timeout_seconds", group: LimitSchedules, unit: LimitSeconds,
		constant: true,
		read: func(*config.Company) float64 {
			return float64(org.DefaultScheduleTimeout / time.Second)
		}},
	{key: "inbox.retention_days", group: LimitInbox, unit: LimitDays,
		read: func(c *config.Company) float64 { return float64(c.Inbox.Days()) }},
	{key: "atlas.refresh_interval_seconds", group: LimitAtlas, unit: LimitSeconds,
		read: func(c *config.Company) float64 { return c.Atlas.RefreshInterval().Seconds() }},
	{key: "code.scratch.idle_days", group: LimitCode, unit: LimitDays,
		read: func(c *config.Company) float64 {
			idle, _ := c.Code.Scratch.Resolved()
			return float64(idle)
		}},
	// ZERO LETS NO SEAT CREATE ONE, which is a setting rather than no limit,
	// so it is not Off.
	{key: "code.scratch.max_per_seat", group: LimitCode, unit: LimitCount,
		read: func(c *config.Company) float64 {
			_, most := c.Code.Scratch.Resolved()
			return float64(most)
		}},
	// ZERO KEEPS MESSAGES FOR EVER, so it is Off: no horizon prunes them.
	{key: "chat.chatlet.message_retention_days", group: LimitChat, unit: LimitDays,
		zeroIsOff: true,
		read: func(c *config.Company) float64 {
			horizon, bounded := c.Chat.Chatlet.MessageRetention()
			if !bounded {
				return 0
			}
			return horizon.Hours() / 24
		}},
}

// delegatedLimits are the limits every delegate is held to, the rows of
// `delegated_decisions`' `limits`, read through the one accessor every reader
// of them uses ([config.DelegatedDecisions.Limits]).
var delegatedLimits = []limit{
	{key: "delegated_decisions.deadline_seconds", group: LimitDelegated, unit: LimitSeconds,
		read: func(c *config.Company) float64 { return c.DelegatedDecisions.Limits().Deadline.Seconds() }},
	{key: "delegated_decisions.allows_per_hour", group: LimitDelegated, unit: LimitCount,
		read: func(c *config.Company) float64 { return float64(c.DelegatedDecisions.Limits().AllowsPerHour) }},
	{key: "delegated_decisions.server_allows_per_hour", group: LimitDelegated, unit: LimitCount,
		read: func(c *config.Company) float64 {
			return float64(c.DelegatedDecisions.Limits().ServerAllowsPerHour)
		}},
}

// limitsAnswer is every row read off c, beside the engine's defaults.
func limitsAnswer(c *config.Company) LimitsAnswer {
	return LimitsAnswer{Limits: limitRows(c, limits)}
}

// limitRows is each row of table read off c, beside the engine's defaults.
func limitRows(c *config.Company, table []limit) []LimitRow {
	defaults := config.DefaultCompany()
	out := make([]LimitRow, 0, len(table))
	for _, l := range table {
		value, def := l.read(c), l.read(&defaults)
		row := LimitRow{
			Key: l.key, Group: l.group, Unit: l.unit,
			Value: value, Default: def,
			Off:        l.zeroIsOff && value == 0,
			DefaultOff: l.zeroIsOff && def == 0,
			Settable:   !l.constant,
		}
		if r, ok := config.RangeOf(l.key); ok && row.Settable {
			row.Range = &r
		}
		out = append(out, row)
	}
	return out
}

// companyLimits answers the caps of the company this node runs, and none on a
// node running no company: no turn runs there, so nothing is held to them.
func (s Sources) companyLimits(context.Context, Params) (any, error) {
	company, _ := s.Company()
	if company == nil {
		return LimitsAnswer{Limits: []LimitRow{}}, nil
	}
	return limitsAnswer(company), nil
}

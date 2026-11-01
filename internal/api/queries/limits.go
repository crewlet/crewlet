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
)

// LimitGroups are the four, in that order.
var LimitGroups = []LimitGroup{LimitTurn, LimitDelegation, LimitNotifications, LimitSchedules}

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
)

// LimitUnits are the four.
var LimitUnits = []LimitUnit{LimitCount, LimitSeconds, LimitFraction, LimitPerSecond}

// LimitRow is one cap: where the company document sets it, the value the
// engine enforces, and the engine's default for it.
//
// THE VALUE IS THE ENFORCED ONE. A company document is decoded over
// [config.DefaultCompany], so a cap the document leaves out is the default in
// the applied company, and every caller of these settings reads the applied
// company. A screen therefore draws `value` and never re-derives a default:
// the dashboard keeping its own copy of [config.DefaultTurnEngine] is the
// drift this answer exists to prevent.
//
// THE DEFAULT IS BESIDE IT, never a "set" flag: the applied company does not
// remember whether a value was written or defaulted, and a document that
// writes the default value is indistinguishable from one that writes nothing.
// What a screen can say truthfully is whether the value differs from the
// engine's default.
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
	// anything: a `notification_rate_limit` of 0 is no rate limit, and an
	// `onboarding_max_tool_rounds` of 0 is no onboarding pass.
	Off bool `json:"off"`
	// DefaultOff is the same question asked of Default, so a screen that
	// says which default a set value replaced says Off where the default is
	// no limit, rather than keeping its own copy of which zeros are off.
	DefaultOff bool `json:"default_off"`
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
	{key: "turn_engine.delegation_depth_limit", group: LimitTurn, unit: LimitCount,
		read: func(c *config.Company) float64 { return float64(c.TurnEngine.DelegationDepthLimit) }},
	{key: "turn_engine.delegation.max_parallel", group: LimitDelegation, unit: LimitCount,
		read: func(c *config.Company) float64 { return float64(c.TurnEngine.Delegation.MaxParallel) }},
	{key: "turn_engine.delegation.max_tasks_per_call", group: LimitDelegation, unit: LimitCount,
		read: func(c *config.Company) float64 {
			return float64(c.TurnEngine.Delegation.MaxTasksPerCall)
		}},
	{key: "turn_engine.delegation.max_turns", group: LimitDelegation, unit: LimitCount,
		read: func(c *config.Company) float64 { return float64(c.TurnEngine.Delegation.MaxTurns) }},
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
		read: func(*config.Company) float64 {
			return float64(org.DefaultScheduleTimeout / time.Second)
		}},
}

// limitsAnswer is every row read off c, beside the engine's defaults.
func limitsAnswer(c *config.Company) LimitsAnswer {
	defaults := config.DefaultCompany()
	out := LimitsAnswer{Limits: make([]LimitRow, 0, len(limits))}
	for _, l := range limits {
		value, def := l.read(c), l.read(&defaults)
		out.Limits = append(out.Limits, LimitRow{
			Key: l.key, Group: l.group, Unit: l.unit,
			Value: value, Default: def,
			Off:        l.zeroIsOff && value == 0,
			DefaultOff: l.zeroIsOff && def == 0,
		})
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

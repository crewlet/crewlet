package config

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/words"
)

// Range is the values one numeric setting admits: the bound a validator holds
// the company document to, and the one a screen that changes the setting
// checks a typed value against before it sends it.
//
// # Declared once, for both
//
// A cap's bounds lived inline in its validator and nowhere else, so nothing
// could report them. Settings › Limits checked only a value's shape and sent
// 0, -2 and 999999 to the engine, whose refusal was this package's operator
// line ("turn_engine.max_iterations: value out of range: must be at least 1,
// got 0: …"), and a copy of the bounds kept beside the editor would have
// drifted from the validator the day either moved. So each bound is declared
// in [settingRanges], keyed by the setting's authored path: the validator
// reads it through [problems.within], `queries.LimitRow` reports it beside the
// value, and a fault the validator raises carries it ([Fault.Range]), so a
// screen says an engine refusal in the same words as its own check.
//
// A Range bounds ONE setting. Two settings held to each other (a task's time
// limit under its call's, a round cap under its ceiling) are a rule about
// both, checked by the validator that owns them.
type Range struct {
	// Min is the least value admitted, or, with MinExclusive, the value every
	// admitted value is above.
	Min float64 `json:"min"`
	// MinExclusive refuses Min itself: a timeout of zero expires before the
	// work it bounds starts, while every positive one is a timeout.
	MinExclusive bool `json:"min_exclusive,omitempty"`
	// Max is the greatest value admitted; nil where nothing bounds the
	// setting from above.
	Max *float64 `json:"max,omitempty"`
	// OrZero admits 0 beside Min..Max: a setting whose zero is a choice of
	// its own rather than the low end of its scale (a message horizon of 0
	// keeps messages for ever, while 1 to 29 days is refused).
	OrZero bool `json:"or_zero,omitempty"`
	// OperatorMin and OperatorMax mark a bound this node's operator set in its
	// own file ([SignInLimits.Range]), narrower than the engine's.
	OperatorMin bool `json:"operator_min,omitempty"`
	OperatorMax bool `json:"operator_max,omitempty"`
}

// Admits reports whether v is a value of the range.
func (r Range) Admits(v float64) bool {
	if r.OrZero && v == 0 {
		return true
	}
	if v < r.Min || (r.MinExclusive && v == r.Min) {
		return false
	}
	return r.Max == nil || v <= *r.Max
}

// refusal is what a value outside the range is told, in this package's words
// for an operator: the bound, and the value that broke it.
func (r Range) refusal(v float64) string {
	if r.OrZero {
		// The same bound, with the zero beside it named first.
		scale := r
		scale.OrZero = false
		return "must be 0 or " + strings.TrimPrefix(scale.refusal(v), "must be ")
	}
	got := number(v)
	switch {
	case r.Max != nil && r.MinExclusive:
		return fmt.Sprintf("must be above %s and at most %s, got %s", number(r.Min), number(*r.Max), got)
	case r.Max != nil:
		return fmt.Sprintf("must be between %s and %s, got %s", number(r.Min), number(*r.Max), got)
	case r.MinExclusive && r.Min == 0:
		return "must be positive, got " + got
	case r.MinExclusive:
		return fmt.Sprintf("must be above %s, got %s", number(r.Min), got)
	case r.Min == 0:
		return "must not be negative, got " + got
	default:
		return fmt.Sprintf("must be at least %s, got %s", number(r.Min), got)
	}
}

// number is a value as a document writes it: 3, 0.25, 1000000.
func number(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// secondsWords is a time limit as a person reads it: whole minutes as
// minutes, anything else as seconds.
func secondsWords(seconds float64) string {
	if seconds >= 60 && seconds == float64(int(seconds)) && int(seconds)%60 == 0 {
		return words.Count(int(seconds)/60, "minute", "minutes")
	}
	if seconds == 1 {
		return "1 second"
	}
	return number(seconds) + " seconds"
}

// atMost is a pointer to a range's ceiling, for a literal in [settingRanges].
func atMost(v float64) *float64 { return &v }

// settingRanges is the range of every numeric setting the validators hold to
// a bound of its own, keyed by its authored path in the company document.
//
// THE ONE DECLARATION of each bound ([Range] says why). [problems.within]
// panics on a path it does not find here, so a validator cannot hold a
// setting to a bound this table does not declare, and
// `queries.TestEverySettableLimitReportsTheRangeTheValidatorHolds` holds every
// cap Settings › Limits changes to having one.
var settingRanges = map[string]Range{
	"turn_engine.max_iterations":             {Min: 1},
	"turn_engine.delegation_depth_limit":     {Min: 1},
	"turn_engine.extension_round_step":       {Min: 1},
	"turn_engine.executor_min_budget_tokens": {Min: 0},
	"turn_engine.onboarding_max_tool_rounds": {Min: 0},

	"turn_engine.delegation.max_parallel":         {Min: 1},
	"turn_engine.delegation.max_tasks_per_call":   {Min: 1},
	"turn_engine.delegation.max_turns":            {Min: 1},
	"turn_engine.delegation.max_turns_ceiling":    {Min: 1},
	"turn_engine.delegation.min_tokens_per_task":  {Min: 0},
	"turn_engine.delegation.task_timeout_seconds": {Min: 0, MinExclusive: true},
	"turn_engine.delegation.call_timeout_seconds": {Min: 0, MinExclusive: true},
	// A SHARE of the parent's remaining budget, not a token count: above
	// nothing, and at most all of it.
	"turn_engine.delegation.budget_fraction": {Min: 0, MinExclusive: true, Max: atMost(1)},

	// THE DELEGATED-DECISION LIMITS (delegated.Limits). A deadline under
	// five minutes reaches people before a delegate's turn can answer, and
	// one over sandbox.DefaultPauseTTL (30 minutes) after the paused box
	// waiting on it was let go. One delegate decides one request a turn and a
	// turn takes a minute or more, so an hourly cap over 60 bounds nothing.
	"delegated_decisions.deadline_seconds":       {Min: 300, Max: atMost(1800)},
	"delegated_decisions.allows_per_hour":        {Min: 1, Max: atMost(60)},
	"delegated_decisions.server_allows_per_hour": {Min: 1, Max: atMost(60)},

	// SETTINGS A PAGE CHANGES IN PLACE beside the caps: each bound's reason
	// is at its constant.
	"inbox.retention_days":           {Min: MinInboxRetentionDays, Max: atMost(MaxInboxRetentionDays)},
	"atlas.refresh_interval_seconds": {Min: MinAtlasRefreshInterval.Seconds()},
	"code.scratch.idle_days":         {Min: CodeScratchIdleDaysFloor, Max: atMost(CodeScratchIdleDaysCeiling)},
	"code.scratch.max_per_seat":      {Min: 0, Max: atMost(CodeScratchMaxPerSeatCeiling)},
	// 0 KEEPS MESSAGES FOR EVER, a choice of its own below the shortest
	// horizon (ChatletConfig.MessageRetentionDays says why it is three-valued).
	"chat.chatlet.message_retention_days": {
		Min: MinMessageRetentionDays, Max: atMost(MaxMessageRetentionDays), OrZero: true,
	},

	// THE SIGN-IN POLICY: each bound's reason is at its constant (signin.go),
	// and each node narrows it to its operator's limits (SignInLimits.Range).
	SignInPasswordKey:        {Min: iam.MinPasswordChars, Max: atMost(MaxPasswordLength)},
	SignInSessionLifetimeKey: {Min: SessionLifetimeFloor.Seconds(), Max: atMost(SessionLifetimeCeiling.Seconds())},
	SignInIdleTimeoutKey:     {Min: IdleTimeoutFloor.Seconds(), Max: atMost(IdleTimeoutCeiling.Seconds())},
	SignInStepUpKey:          {Min: StepUpFloor.Seconds(), Max: atMost(StepUpCeiling.Seconds())},

	"notification_rate_limit":         {Min: 0},
	"notification_coalesce_max_batch": {Min: 1, Max: atMost(coalesceMaxBatchMax)},
}

// RangeOf is the range the setting at key admits, as its validator holds it,
// and false for a setting held to no bound of its own.
func RangeOf(key string) (Range, bool) {
	r, ok := settingRanges[key]
	if ok && r.Max != nil {
		r.Max = atMost(*r.Max)
	}
	return r, ok
}

// within records a fault when value is outside the range declared for path,
// with why the bound exists appended for the operator reading it, and the
// range itself on the fault for a screen to word.
func (p *problems) within(path Path, value float64, why string) {
	p.withinSaying(path, value, why, "")
}

// withinSaying is [problems.within] with the refusal in a person's words
// beside the operator's ([Fault.Said]).
func (p *problems) withinSaying(path Path, value float64, why, said string) {
	r, ok := RangeOf(path.String())
	if !ok {
		panic(fmt.Sprintf("config: %s is held to a range settingRanges does not declare", path))
	}
	if r.Admits(value) {
		return
	}
	detail := r.refusal(value)
	if why != "" {
		detail += ": " + why
	}
	*p = append(*p, &Fault{Path: path, Kind: ErrOutOfRange, Detail: detail, Range: &r, Said: said})
}

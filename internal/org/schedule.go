package org

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ScheduleTarget is who runs a UNIT schedule.
//
// The zero value fans out to each member, which is the default a schedule
// with no target means. There is deliberately no "specific role" target: a
// static role pin adds nothing over a role schedule, so to run recurring
// work as one person you define it on that person. Lead survives because it
// is DYNAMIC — it follows whoever leads the unit, inherited leads included,
// so the schedule does not change when leadership does.
type ScheduleTarget string

const (
	// TargetEach fans out one task per DIRECT agent member of the unit —
	// never descendants, never human seats. Each member runs its own turn
	// under its own idempotency key, so a slow or failing member never
	// blocks the others.
	TargetEach ScheduleTarget = "each"

	// TargetLead routes a single task to the unit's effective lead. The
	// lead carries the team roster in its prompt and owns the unit channel,
	// so gather-and-post rituals belong here.
	TargetLead ScheduleTarget = "lead"
)

// DefaultScheduleTimeout caps a single scheduled turn.
//
// Three minutes: a scheduled turn is a ritual — post a standup, compile a
// weekly report — not open-ended work, and the cap exists so a wedged turn
// releases its seat long before the next fire rather than holding it until
// someone notices. It is comfortably above a normal multi-phase turn, so
// hitting it means stuck, not busy.
const DefaultScheduleTimeout = 3 * time.Minute

// Schedule is recurring work a seat or a unit owns, fired on a cron
// expression.
//
// Schedules live in the org config — one source of truth, hot-reloadable —
// and the scheduler publishes an ordinary task into the resolved runner's
// inbox when one is due. Nothing about the agent runtime path differs from
// work that arrived any other way.
type Schedule struct {
	// Name identifies the schedule within its role or unit. It forms part
	// of the idempotency key, so renaming one lets a fire at the same
	// minute run once more.
	Name string `yaml:"name" json:"name"`

	// Cron is a standard 5-field expression (minute hour dom month dow),
	// evaluated in Timezone. Vixie semantics: when both day-of-month and
	// day-of-week are restricted, a day matches if EITHER matches.
	//
	// This package checks the field count only. Full parsing belongs to the
	// cron evaluator that has to agree with itself about ranges and steps;
	// duplicating that grammar here would eventually reject something the
	// evaluator accepts.
	Cron string `yaml:"cron" json:"cron"`

	// Task is the prompt handed to the runner when the schedule fires.
	Task string `yaml:"task" json:"task"`

	// Timezone is the IANA zone Cron is evaluated in. Empty falls back to
	// the system-wide default. Standups are local-time events, so a company
	// that works in one place sets this.
	Timezone string `yaml:"timezone,omitempty" json:"timezone,omitempty"`

	// Target selects the runner of a UNIT schedule. Ignored on a role
	// schedule, where the runner is always that role.
	Target ScheduleTarget `yaml:"target,omitempty" json:"target,omitempty"`

	// Enabled defaults to true; set it false to keep a schedule in config
	// without firing it.
	Enabled Toggle `yaml:"enabled,omitempty" json:"enabled,omitzero"`

	// TimeoutSeconds is a hard wall-clock cap on one scheduled turn; 0
	// takes DefaultScheduleTimeout. Read it through [Schedule.Timeout].
	TimeoutSeconds int `yaml:"timeout_seconds,omitempty" json:"timeout_seconds,omitempty"`

	// Catchup defaults to true: when the engine starts after missing a
	// tick, fire the single most recent missed run if it falls inside the
	// catchup window. Older missed ticks are never backfilled — a company
	// that was down for a day does not want yesterday's standups.
	Catchup Toggle `yaml:"catchup,omitempty" json:"catchup,omitzero"`
}

// IsEnabled reports whether this schedule fires, applying the true default.
func (s Schedule) IsEnabled() bool { return s.Enabled.Or(true) }

// CatchesUp reports whether a missed tick is fired on start, applying the
// true default.
func (s Schedule) CatchesUp() bool { return s.Catchup.Or(true) }

// TargetsLead reports whether a unit schedule routes to the unit lead
// rather than fanning out to its members.
func (s Schedule) TargetsLead() bool { return s.Target == TargetLead }

// Timeout is the wall-clock cap on one turn of this schedule.
func (s Schedule) Timeout() time.Duration {
	if s.TimeoutSeconds <= 0 {
		return DefaultScheduleTimeout
	}
	return time.Duration(s.TimeoutSeconds) * time.Second
}

// cronFields is the number of whitespace-separated fields in the supported
// cron grammar: minute, hour, day-of-month, month, day-of-week.
const cronFields = 5

// Validate reports the ways this schedule could not be evaluated. Every
// check is shape: whether it has a RUNNER is a question about the unit or
// seat that owns it, answered there.
func (s Schedule) Validate(owner string) error {
	var errs []error
	name := strings.TrimSpace(s.Name)
	if name == "" {
		errs = append(errs, fmt.Errorf("%s: %w: name must not be empty", owner, ErrInvalidSchedule))
	}
	if strings.TrimSpace(s.Task) == "" {
		errs = append(errs, fmt.Errorf("%s: schedule %q: %w: task must not be empty", owner, name, ErrInvalidSchedule))
	}
	if len(strings.Fields(s.Cron)) != cronFields {
		errs = append(errs, fmt.Errorf(
			"%s: schedule %q: %w: cron %q needs %d fields (minute hour day-of-month month day-of-week)",
			owner, name, ErrInvalidSchedule, s.Cron, cronFields))
	}
	if s.Timezone != "" {
		if _, err := time.LoadLocation(s.Timezone); err != nil {
			errs = append(errs, fmt.Errorf(
				"%s: schedule %q: %w: unknown timezone %q",
				owner, name, ErrInvalidSchedule, s.Timezone))
		}
	}
	if s.TimeoutSeconds < 0 {
		errs = append(errs, fmt.Errorf(
			"%s: schedule %q: %w: timeout_seconds must be positive",
			owner, name, ErrInvalidSchedule))
	}
	switch s.Target {
	case "", TargetEach, TargetLead:
	default:
		errs = append(errs, fmt.Errorf(
			"%s: schedule %q: %w: target %q must be each or lead — to run recurring work as one specific seat, define the schedule on that seat",
			owner, name, ErrInvalidSchedule, s.Target))
	}
	return errors.Join(errs...)
}

// validateSchedules checks a role's or unit's schedules and their names.
// Names must be unique within their owner because the name is half the
// idempotency key: two schedules sharing one would each suppress the
// other's fire at the same minute.
func validateSchedules(owner string, schedules []Schedule) error {
	var errs []error
	seen := make(map[string]struct{}, len(schedules))
	for _, s := range schedules {
		if err := s.Validate(owner); err != nil {
			errs = append(errs, err)
		}
		if _, dup := seen[s.Name]; dup {
			errs = append(errs, fmt.Errorf("%s: %w: duplicate schedule name %q", owner, ErrInvalidSchedule, s.Name))
		}
		seen[s.Name] = struct{}{}
	}
	return errors.Join(errs...)
}

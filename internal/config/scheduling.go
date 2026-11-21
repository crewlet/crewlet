package config

import "time"

// Scheduling configures the role/unit scheduler: recurring work a seat or a
// unit owns, fired on a cron expression. It also carries [WorkPickup], the
// engine's cadence for handing an idle seat ready work, which runs with or
// without the cron scheduler.
//
// Which CLOCK a schedule fires on is not here: one naming no zone fires on
// the company's `timezone`, the clock every other calendar edge is on too
// (ADR-0018); a scheduler-held default would be a second company clock.
//
// The scheduler starts only when enabled, a store exists (its at-most-once
// ledger) and a schedule is declared, re-evaluated on every config apply
// (see engine.reconcileScheduler).
type Scheduling struct {
	Enabled Toggle `yaml:"enabled,omitempty" json:"enabled,omitzero" desc:"Master switch for the scheduler (default on)."`

	// TickSeconds is the poll interval. Cron fires at minute granularity,
	// so anything under 60 s reliably catches every fire; the default is
	// well under that so a fire is late by seconds, not by a minute.
	TickSeconds int `yaml:"tick_seconds,omitempty" json:"tick_seconds,omitempty" js:"min=1" desc:"Scheduler poll interval; under 60s catches every cron minute."`

	// JitterSeconds spreads schedules that share a popular cron minute
	// (0 9 * * *). 0 fires exactly on the minute; the concurrency
	// controller already queues a burst fairly, so this is an opt-in
	// smoothing knob rather than a correctness requirement.
	JitterSeconds int `yaml:"jitter_seconds,omitempty" json:"jitter_seconds,omitempty" js:"min=0" desc:"Deterministic per-schedule jitter to spread a popular minute."`

	// CatchupMinSeconds and CatchupMaxSeconds clamp the missed-tick window:
	// after a restart the single most recent missed run fires if it falls
	// inside it, and older ticks are never backfilled.
	//
	// The maximum is also the floor for the completion ledger's retention:
	// deleting a row a tick could still evaluate lets that fire run twice.
	//
	// THE MINIMUM IS AT LEAST ONE SECOND. The scheduler reads a zero clamp as
	// unset and puts its own two minutes back (internal/schedule's
	// DefaultCatchupMin), so 0 never meant "no floor"; one second is what "no
	// floor" can honestly be: below it is the schedule's own half-period.
	CatchupMinSeconds int `yaml:"catchup_min_seconds,omitempty" json:"catchup_min_seconds,omitempty" js:"min=1" desc:"Lower clamp on the missed-tick catchup window (at least 1)."`
	CatchupMaxSeconds int `yaml:"catchup_max_seconds,omitempty" json:"catchup_max_seconds,omitempty" js:"min=0" desc:"Upper clamp on the missed-tick catchup window."`

	// WorkPickup is how an idle agent seat is handed its next ready work
	// item (ADR-0112). Independent of Enabled above, which switches the
	// cron scheduler alone.
	WorkPickup WorkPickup `yaml:"work_pickup,omitempty" json:"work_pickup,omitzero" desc:"Hands an idle agent seat its next ready work item (default on)."`
}

// WorkPickup configures the work pickup: the wake the engine derives for an
// agent seat that is idle while it holds assigned work that is ready.
type WorkPickup struct {
	Enabled Toggle `yaml:"enabled,omitempty" json:"enabled,omitzero" desc:"Hand an idle agent seat its next ready work item (default on)."`

	// CooldownSeconds is the least time between two pickups of one seat,
	// across the fleet.
	CooldownSeconds int `yaml:"cooldown_seconds,omitempty" json:"cooldown_seconds,omitempty" js:"min=60;max=1800" desc:"Least time between two pickups of one seat, 60..1800 seconds (default 600)."`
}

// DefaultWorkPickupCooldownSeconds is ten minutes: a seat that declines one
// ready item after another is offered the next at most six times an hour,
// and a seat that finished its item is handed the next within one cooldown.
const DefaultWorkPickupCooldownSeconds = 600

// MaxWorkPickupCooldownSeconds is the fleet claim's own ceiling
// (coord.MaxClaimTTL): the cooldown is a claim held for this long.
const MaxWorkPickupCooldownSeconds = 1800

// minWorkPickupCooldownSeconds is one pickup tick: a shorter cooldown could
// not be observed by the loop that honours it.
const minWorkPickupCooldownSeconds = 60

// Runs reports whether the work pickup runs, applying the true default.
func (w *WorkPickup) Runs() bool { return w.Enabled.Or(true) }

// Cooldown is the least time between two pickups of one seat.
func (w *WorkPickup) Cooldown() time.Duration {
	return time.Duration(w.CooldownSeconds) * time.Second
}

func (w *WorkPickup) validate(path Path) error {
	var p problems
	if w.CooldownSeconds < minWorkPickupCooldownSeconds ||
		w.CooldownSeconds > MaxWorkPickupCooldownSeconds {
		p.add(at(path, "cooldown_seconds"), ErrOutOfRange,
			"must be %d..%d, got %d: the pickup loop ticks once a minute and "+
				"the cooldown is a fleet claim, which lasts at most thirty minutes",
			minWorkPickupCooldownSeconds, MaxWorkPickupCooldownSeconds, w.CooldownSeconds)
	}
	return p.err()
}

// DefaultScheduling is the scheduler's shipped defaults.
func DefaultScheduling() Scheduling {
	return Scheduling{
		TickSeconds: 10,
		// Two minutes to two hours. The floor keeps a restart from
		// re-firing something that just ran; the ceiling is what stops a
		// morning restart from replaying the whole night.
		CatchupMinSeconds: 120,
		CatchupMaxSeconds: 7200,
		WorkPickup:        WorkPickup{CooldownSeconds: DefaultWorkPickupCooldownSeconds},
	}
}

// Runs reports whether the scheduler runs, applying the true default.
func (s *Scheduling) Runs() bool { return s.Enabled.Or(true) }

// Tick is the poll interval as a duration, applying the shipped default.
//
// An accessor rather than a raw read for the same reason CatchupMax is one:
// the field is a number of SECONDS at the config edge and a Duration
// everywhere above it, and converting once here is what keeps a caller from
// handing the scheduler a 10-nanosecond tick.
func (s *Scheduling) Tick() time.Duration {
	if s.TickSeconds <= 0 {
		return 0 // the scheduler's own DefaultTick
	}
	return time.Duration(s.TickSeconds) * time.Second
}

// CatchupMax is the upper clamp as a duration. It is also the retention
// floor for the completion ledger, which is why it has an accessor rather
// than being read raw.
func (s *Scheduling) CatchupMax() time.Duration {
	return time.Duration(s.CatchupMaxSeconds) * time.Second
}

// maxTickSeconds is a minute: cron fires at minute granularity, so a tick
// at or above it can skip a fire entirely rather than merely delaying one.
const maxTickSeconds = 60

func (s *Scheduling) validate(path Path) error {
	var p problems
	if s.TickSeconds < 1 || s.TickSeconds > maxTickSeconds {
		p.add(at(path, "tick_seconds"), ErrOutOfRange,
			"must be 1..%d, got %d: cron fires at minute granularity, so a "+
				"tick at or above a minute misses fires rather than delaying them",
			maxTickSeconds, s.TickSeconds)
	}
	p.wrap(nonNegative(path, "jitter_seconds", s.JitterSeconds))
	p.wrap(positive(path, "catchup_min_seconds", s.CatchupMinSeconds))
	if s.CatchupMaxSeconds < s.CatchupMinSeconds {
		p.add(at(path, "catchup_max_seconds"), ErrOutOfRange,
			"must be at least catchup_min_seconds (%d), got %d",
			s.CatchupMinSeconds, s.CatchupMaxSeconds)
	}
	p.wrap(s.WorkPickup.validate(at(path, "work_pickup")))
	return p.err()
}

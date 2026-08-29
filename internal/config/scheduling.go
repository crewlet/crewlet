package config

import "time"

// Scheduling configures the role/unit scheduler: recurring work a seat or a
// unit owns, fired on a cron expression.
//
// The scheduler starts only when it is enabled, a store exists (it needs
// the at-most-once ledger), and at least one seat or unit declares a
// schedule. A company with no schedules never spins up the tick loop — and
// the three conditions are re-evaluated on every config apply, so a
// founder's FIRST schedule arms the loop without a restart and their last
// one removed releases its fleet duty. See engine.reconcileScheduler.
type Scheduling struct {
	Enabled Toggle `yaml:"enabled,omitempty" json:"enabled,omitzero" desc:"Master switch for the scheduler (default on)."`

	// TickSeconds is the poll interval. Cron fires at minute granularity,
	// so anything under 60 s reliably catches every fire; the default is
	// well under that so a fire is late by seconds, not by a minute.
	TickSeconds int `yaml:"tick_seconds,omitempty" json:"tick_seconds,omitempty" js:"min=1" desc:"Scheduler poll interval; under 60s catches every cron minute."`

	// DefaultTimezone is the zone a schedule that names none is evaluated
	// in. Standups are local-time events, so a company that works in one
	// place sets this.
	DefaultTimezone string `yaml:"default_timezone,omitempty" json:"default_timezone,omitempty" desc:"IANA zone for schedules that name none."`

	// JitterSeconds spreads schedules that share a popular cron minute
	// (0 9 * * *). 0 fires exactly on the minute; the concurrency
	// controller already queues a burst fairly, so this is an opt-in
	// smoothing knob rather than a correctness requirement.
	JitterSeconds int `yaml:"jitter_seconds,omitempty" json:"jitter_seconds,omitempty" js:"min=0" desc:"Deterministic per-schedule jitter to spread a popular minute."`

	// CatchupMinSeconds and CatchupMaxSeconds clamp the missed-tick
	// window: after a restart the single most recent missed run fires if
	// it falls inside it, and older ticks are never backfilled — a company
	// that was down for a day does not want yesterday's standups.
	//
	// The maximum is also the floor for the completion ledger's retention:
	// deleting a row a tick could still evaluate lets that fire run twice.
	CatchupMinSeconds int `yaml:"catchup_min_seconds,omitempty" json:"catchup_min_seconds,omitempty" js:"min=0" desc:"Lower clamp on the missed-tick catchup window."`
	CatchupMaxSeconds int `yaml:"catchup_max_seconds,omitempty" json:"catchup_max_seconds,omitempty" js:"min=0" desc:"Upper clamp on the missed-tick catchup window."`
}

// DefaultScheduling is the scheduler's shipped defaults.
func DefaultScheduling() Scheduling {
	return Scheduling{
		TickSeconds:     10,
		DefaultTimezone: "UTC",
		// Two minutes to two hours. The floor keeps a restart from
		// re-firing something that just ran; the ceiling is what stops a
		// morning restart from replaying the whole night.
		CatchupMinSeconds: 120,
		CatchupMaxSeconds: 7200,
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

func (s *Scheduling) validate(path string) error {
	var p problems
	if s.TickSeconds < 1 || s.TickSeconds > maxTickSeconds {
		p.add(at(path, "tick_seconds"), ErrOutOfRange,
			"must be 1..%d, got %d — cron fires at minute granularity, so a "+
				"tick at or above a minute misses fires rather than delaying them",
			maxTickSeconds, s.TickSeconds)
	}
	p.wrap(nonNegative(path, "jitter_seconds", s.JitterSeconds))
	p.wrap(nonNegative(path, "catchup_min_seconds", s.CatchupMinSeconds))
	if s.CatchupMaxSeconds < s.CatchupMinSeconds {
		p.add(at(path, "catchup_max_seconds"), ErrOutOfRange,
			"must be at least catchup_min_seconds (%d), got %d",
			s.CatchupMinSeconds, s.CatchupMaxSeconds)
	}
	if s.DefaultTimezone != "" {
		if _, err := time.LoadLocation(s.DefaultTimezone); err != nil {
			p.add(at(path, "default_timezone"), ErrUnknownValue,
				"%q is not an IANA timezone", s.DefaultTimezone)
		}
	}
	return p.err()
}

package config

import "time"

// AccessHistory is the company's policy over the record of its seats' access:
// what each seat was told about its own access, and every SSH certificate the
// engine signed and command it ran (internal/accessdomain). It is an audit
// trail, so its horizon is the company's to set and longer than a mailbox's.
type AccessHistory struct {
	// RetentionDays is how long a record is kept. Zero is unset, the default:
	// no horizon below a quarter of a year is an audit trail. Each seat's
	// newest told record stays while the seat exists, since it is what the
	// seat's next turn is diffed against.
	RetentionDays int `yaml:"retention_days,omitempty" json:"retention_days,omitempty" js:"min=0;max=3650" desc:"How long the record of what each agent was told about its access, and of every SSH certificate and command, is kept (default 400, 90..3650). Each agent's newest record stays while the agent exists."`
}

// The access history's default and bounds.
const (
	// DefaultAccessRetentionDays is a year and a month, so a yearly review
	// still finds the whole year it reviews.
	DefaultAccessRetentionDays = 400

	// MinAccessRetentionDays is a quarter of a year, below which an incident
	// found late has no trail left.
	MinAccessRetentionDays = 90

	// MaxAccessRetentionDays is ten years, the longest horizon any other
	// history of the company keeps.
	MaxAccessRetentionDays = 3650
)

// Days is how many days a record is kept, with the default applied.
func (a AccessHistory) Days() int {
	if a.RetentionDays > 0 {
		return a.RetentionDays
	}
	return DefaultAccessRetentionDays
}

// Retention is how long a record is kept, with the default applied.
func (a AccessHistory) Retention() time.Duration {
	return time.Duration(a.Days()) * 24 * time.Hour
}

// validate refuses a horizon outside its bounds.
func (a AccessHistory) validate(path Path) error {
	var p problems
	if d := a.RetentionDays; d != 0 && (d < MinAccessRetentionDays || d > MaxAccessRetentionDays) {
		p.add(at(path, "retention_days"), ErrOutOfRange,
			"%d is outside %d..%d. Under a quarter of a year an incident found late has no "+
				"trail left, and past ten years the field is not a horizon", d,
			MinAccessRetentionDays, MaxAccessRetentionDays)
	}
	return p.err()
}

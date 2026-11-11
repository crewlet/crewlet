package config

import "time"

// Atlas is the company's policy over its atlas, the map and catalogue of what
// it runs on: here, how long what people record in it is kept as history
// (internal/atlasdomain). The atlas itself is never declared here: what the
// company records in it is edited on the atlas, and what this document already
// declares is read from it whole.
type Atlas struct {
	// RetentionDays is how long the atlas's history is kept: a value somebody
	// replaced, and an entry a person retired that nothing has reported since.
	// Zero is unset, the default. What the atlas holds now never expires.
	RetentionDays int `yaml:"retention_days,omitempty" json:"retention_days,omitempty" js:"min=0;max=3650" desc:"How long the atlas keeps its history: a value somebody replaced, and an entry a person retired that nothing has reported since (default 400, 90..3650). What the atlas holds now never expires."`
}

// The atlas history's default and bounds, the access history's and for its
// reasons.
const (
	// DefaultAtlasRetentionDays is a year and a month, so a yearly review
	// of what the company ran on still finds the whole year it reviews.
	DefaultAtlasRetentionDays = 400

	// MinAtlasRetentionDays is a quarter of a year, below which an incident
	// found late cannot see what the company ran on when it happened.
	MinAtlasRetentionDays = 90

	// MaxAtlasRetentionDays is ten years, the longest horizon any other
	// history of the company keeps.
	MaxAtlasRetentionDays = 3650
)

// Days is how many days the history is kept, with the default applied.
func (a Atlas) Days() int {
	if a.RetentionDays > 0 {
		return a.RetentionDays
	}
	return DefaultAtlasRetentionDays
}

// Retention is how long the history is kept, with the default applied.
func (a Atlas) Retention() time.Duration {
	return time.Duration(a.Days()) * 24 * time.Hour
}

// validate refuses a horizon outside its bounds.
func (a Atlas) validate(path Path) error {
	var p problems
	if d := a.RetentionDays; d != 0 && (d < MinAtlasRetentionDays || d > MaxAtlasRetentionDays) {
		p.add(at(path, "retention_days"), ErrOutOfRange,
			"%d is outside %d..%d. Under a quarter of a year an incident found late cannot see what "+
				"the company ran on when it happened, and past ten years the field is not a horizon", d,
			MinAtlasRetentionDays, MaxAtlasRetentionDays)
	}
	return p.err()
}

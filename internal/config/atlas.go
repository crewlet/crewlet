package config

import "time"

// Atlas is the company's policy over its atlas, the map and catalogue of what
// it runs on: how long what people record in it is kept as history
// (internal/atlasdomain), and how often each connected integration's vendor
// is read whole for it (internal/atlas/collect). The atlas itself is never
// declared here: what the company records in it is edited on the atlas, and
// what this document already declares is read from it whole.
type Atlas struct {
	// RetentionDays is how long the atlas's history is kept: a value somebody
	// replaced, and an entry a person retired that nothing has reported since.
	// Zero is unset, the default. What the atlas holds now never expires.
	RetentionDays int `yaml:"retention_days,omitempty" json:"retention_days,omitempty" js:"min=0;max=3650" desc:"How long the atlas keeps its history: a value somebody replaced, and an entry a person retired that nothing has reported since (default 400, 90..3650). What the atlas holds now never expires."`

	// RefreshIntervalSeconds is how often the atlas-collect duty reads each
	// connected integration's vendor whole for the atlas (internal/atlas/
	// collect). Zero is unset, the default of an hour; a value below the
	// floor is refused naming the field, never clamped, for the reason the
	// integrations' check interval refuses one: a value typed in seconds by
	// somebody who meant minutes should say so.
	RefreshIntervalSeconds int `yaml:"refresh_interval_seconds,omitempty" json:"refresh_interval_seconds,omitempty" js:"min=0" desc:"How often each connected integration is read whole for the atlas, in seconds. 0 takes the default of 3600; the floor is 300."`
}

// The atlas refresh interval's default and floor.
const (
	// DefaultAtlasRefreshInterval is an hour: inventory moves at the pace of
	// deploys, and at that cadence the largest read the first source makes
	// (a Cloudflare account of ten thousand records, about 300 calls) spends
	// a quarter of one five-minute window of the token's 1,200 calls, a
	// window it shares with the reconcile loop, once an hour.
	DefaultAtlasRefreshInterval = time.Hour

	// MinAtlasRefreshInterval is five minutes, one rate-limit window: below
	// it a large account's listing would crowd the reconcile loop out of the
	// same token's window, and an unchanged read writes nothing to the log
	// anyway, so reading more often buys at most five minutes of freshness.
	MinAtlasRefreshInterval = 5 * time.Minute
)

// RefreshInterval is how often each integration is read for the atlas, with
// the default applied.
func (a Atlas) RefreshInterval() time.Duration {
	if a.RefreshIntervalSeconds > 0 {
		return time.Duration(a.RefreshIntervalSeconds) * time.Second
	}
	return DefaultAtlasRefreshInterval
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
	if secs := a.RefreshIntervalSeconds; secs != 0 && time.Duration(secs)*time.Second < MinAtlasRefreshInterval {
		p.add(at(path, "refresh_interval_seconds"), ErrOutOfRange,
			"%d is below the floor of %d: a read of a vendor's whole inventory shares the token's "+
				"rate-limit window with the integration reconcile loop, and an unchanged read writes "+
				"nothing anyway. Leave it unset for the default of %d",
			secs, int(MinAtlasRefreshInterval.Seconds()), int(DefaultAtlasRefreshInterval.Seconds()))
	}
	return p.err()
}

package livestate

import (
	"strings"
	"time"
)

// stamp is a timestamp that can still be ORDERED when it did not parse.
//
// Every state transition in this projection is gated on the event's timestamp,
// because the events arrive over a broker that guarantees order only within a
// topic — and different event types are different topics. A state-affecting
// event can therefore arrive out of order relative to another, and an older one
// must never clobber newer state.
//
// The input is whatever the wire carried, which this process does not control.
// A comparison that failed on one malformed value would take the ordering guard
// down with it, so an unparseable stamp degrades to lexicographic ordering
// rather than to an error: worse ordering for one bad row, never a broken guard
// for the rest.
type stamp struct {
	t     time.Time
	raw   string
	valid bool
}

// newStamp parses an ISO-8601 timestamp, keeping the raw text either way.
//
// The raw text matters beyond the fallback: what goes back out on the wire as
// updated_at must be the encoding it arrived in, while every comparison here
// wants the instant. Holding both is what stops a re-serialization from
// changing a value the dashboard already has.
func newStamp(raw string) stamp {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999", "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, raw); err == nil {
			return stamp{t: t.UTC(), raw: raw, valid: true}
		}
	}
	return stamp{raw: raw}
}

// empty reports whether there was no timestamp at all, which is distinct from
// one that failed to parse: an absent stamp skips the guard, a malformed one
// still orders.
func (s stamp) empty() bool { return s.raw == "" }

// compare orders two stamps: by instant when both parsed, lexicographically
// when either did not.
func (s stamp) compare(o stamp) int {
	if s.valid && o.valid {
		return s.t.Compare(o.t)
	}
	return strings.Compare(s.raw, o.raw)
}

func (s stamp) before(o stamp) bool { return s.compare(o) < 0 }
func (s stamp) after(o stamp) bool  { return s.compare(o) > 0 }

// sub reports the age of s relative to now, or false when it cannot be known.
func (s stamp) olderThan(now time.Time, d time.Duration) bool {
	if !s.valid {
		// An entry with no usable timestamp cannot be aged out on time,
		// and dropping it on that basis would be arbitrary.
		return false
	}
	return now.Sub(s.t) > d
}

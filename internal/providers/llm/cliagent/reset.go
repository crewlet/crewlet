package cliagent

import (
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm/cliagent/cliprofile"
)

// resetAt reads the instant a limit lifts out of the vendor's report, or the
// zero time when the report names none this reading can take.
//
// ZERO IS AN ANSWER, not a failure: [Provider.bench] reads it as "the vendor
// did not say" and sits the entry out a bounded, growing window, which is
// honest where an instant read wrongly is not. That includes an instant
// already past: a limit reported after its own reset says the window turned
// over a moment ago, and waiting for it again would wait a day.
func resetAt(said string, r cliprofile.ResetReading, now time.Time) time.Time {
	if r.Separator == "" {
		return time.Time{}
	}
	_, rest, found := strings.Cut(said, r.Separator)
	if !found {
		return time.Time{}
	}
	var at time.Time
	switch r.Unit {
	case cliprofile.ResetEpoch, cliprofile.ResetSeconds:
		value, ok := leadingInt(rest)
		if !ok {
			return time.Time{}
		}
		if r.Unit == cliprofile.ResetEpoch {
			at = time.Unix(value, 0)
		} else {
			at = now.Add(time.Duration(value) * time.Second)
		}
	case cliprofile.ResetClock:
		at = clockReset(rest, now)
	}
	if !at.After(now) {
		return time.Time{}
	}
	return at.UTC()
}

// leadingInt is the run of digits rest opens with: the value ends at the first
// character that cannot be part of it, so a reset embedded in a longer
// sentence still yields its number.
func leadingInt(rest string) (int64, bool) {
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, false
	}
	value, err := strconv.ParseInt(rest[:end], 10, 64)
	return value, err == nil
}

// clockReset reads "4:30pm (America/Vancouver)", "4pm (Europe/Berlin)" or
// "16:30 (Asia/Tokyo)" as the occurrence of that time of day, in that zone,
// NEAREST now: yesterday's, today's or tomorrow's.
//
// THE NEAREST, because the vendor names a time and not a date. A reset named
// at 11 pm for "2am" is the coming night's, three hours away, and one named
// for "4:30pm" a minute after half past four is the one that has just gone
// by (which [resetAt] then reads as no instant at all), not tomorrow's, a
// whole day away. A window that genuinely lasts longer than twelve hours is
// written with a date this reading does not take, and so sits out the bounded
// window an unread reset does.
//
// THE ZONE IS REQUIRED. Claude Code names the zone it rendered the time in,
// which is the zone of the host the CLI ran on: a sandbox's, the engine's, or
// a login's, none of which this process can assume. A time with no zone, or
// one the system's zone database does not know, is no instant.
func clockReset(rest string, now time.Time) time.Time {
	rest = strings.TrimLeft(rest, " ")
	hour, rest, ok := clockNumber(rest, 2)
	if !ok {
		return time.Time{}
	}
	minute := 0
	if after, colon := strings.CutPrefix(rest, ":"); colon {
		var two bool
		minute, rest, two = clockNumber(after, 2)
		if !two || len(after)-len(rest) != 2 || minute > 59 {
			return time.Time{}
		}
	}
	lower := strings.ToLower(rest)
	switch {
	case strings.HasPrefix(lower, "am"), strings.HasPrefix(lower, "pm"):
		if hour < 1 || hour > 12 {
			return time.Time{}
		}
		hour %= 12
		if lower[0] == 'p' {
			hour += 12
		}
		rest = rest[2:]
	case hour > 23:
		return time.Time{}
	}
	open, shut := strings.Index(rest, "("), strings.Index(rest, ")")
	if open < 0 || shut < open || strings.TrimSpace(rest[:open]) != "" {
		return time.Time{}
	}
	zone, err := time.LoadLocation(strings.TrimSpace(rest[open+1 : shut]))
	if err != nil {
		return time.Time{}
	}
	local := now.In(zone)
	nearest := time.Time{}
	for _, day := range []int{-1, 0, 1} {
		at := time.Date(local.Year(), local.Month(), local.Day()+day, hour, minute, 0, 0, zone)
		if nearest.IsZero() || absDuration(at.Sub(now)) < absDuration(nearest.Sub(now)) {
			nearest = at
		}
	}
	return nearest
}

// clockNumber reads up to width digits off the front of s.
func clockNumber(s string, width int) (int, string, bool) {
	end := 0
	for end < len(s) && end < width && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, s, false
	}
	n, err := strconv.Atoi(s[:end])
	return n, s[end:], err == nil
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

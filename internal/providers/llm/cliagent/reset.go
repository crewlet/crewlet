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

// clockResetGrace is how far before now a named time of day still reads as the
// occurrence that has just gone by, rather than tomorrow's.
//
// FIVE MINUTES. The vendor names a time, rounded to the minute or the hour,
// that the window turns over at on its own clock, and this reads it against
// the engine host's clock when the run ended: the two clocks can disagree by a
// minute or two on a host whose time is not disciplined, and the run's exit can
// trail the line it printed. A reset that has passed by less than that is the
// window turning over as the report was read, which [resetAt] answers as no
// instant at all. Anything further back cannot be today's (a vendor does not
// report a limit whose reset passed a quarter of an hour ago), so it is
// tomorrow's.
const clockResetGrace = 5 * time.Minute

// clockReset reads "4:30pm (America/Vancouver)", "4pm (Europe/Berlin)" or
// "16:30 (Asia/Tokyo)" as the NEXT occurrence of that time of day, in that
// zone: the first one no more than [clockResetGrace] before now.
//
// THE NEXT, because the vendor names a time and not a date, and a reset is
// always ahead of the report that names it. A reset named at 11 pm for "2am"
// is the coming night's, three hours away; one named at 4:16 pm for "5am" is
// tomorrow's, thirteen hours away, which the nearest occurrence read as this
// morning's and so as no instant; and one named for "4:30pm" a minute after
// half past four is the one that has just gone by (no instant), not tomorrow's.
// A window that lasts longer than a day is written with a date this reading
// does not take, and so sits out the bounded window an unread reset does.
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
	earliest := now.Add(-clockResetGrace)
	for _, day := range []int{-1, 0, 1} {
		if at := time.Date(local.Year(), local.Month(), local.Day()+day, hour, minute, 0, 0, zone); !at.Before(earliest) {
			return at
		}
	}
	return time.Time{}
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

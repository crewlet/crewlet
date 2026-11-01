package harness

import (
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// resetAt reads the instant a limit lifts out of the vendor's report, or the
// zero time when the report names none this reading can take. zone is the zone
// the CLI ran in ([childZone]), which a time of day named with no zone of its
// own is in; nil reads UTC.
//
// ZERO IS AN ANSWER, not a failure: [Provider.bench] reads it as "the vendor
// did not say" and sits the entry out a bounded, growing window, which is
// honest where an instant read wrongly is not. That includes an instant
// already past: a limit reported after its own reset says the window turned
// over a moment ago, and waiting for it again would wait a day.
//
// THE SEPARATOR IS MATCHED WITHOUT CASE. It is a separator, not a sentinel: it
// classifies nothing, and only says where the reset begins in a line already
// taken as the vendor's report. Codex writes "or try again at" in mid-sentence
// and "Try again at" opening one (both in 0.150.1's own strings), and a
// case-sensitive cut read only one of them.
func resetAt(said string, r harnessprofile.ResetReading, now time.Time, zone *time.Location) time.Time {
	if r.Separator == "" {
		return time.Time{}
	}
	idx := strings.Index(strings.ToLower(said), strings.ToLower(r.Separator))
	if idx < 0 {
		return time.Time{}
	}
	rest := said[idx+len(r.Separator):]
	var at time.Time
	switch r.Unit {
	case harnessprofile.ResetEpoch, harnessprofile.ResetSeconds:
		value, ok := leadingInt(rest)
		if !ok {
			return time.Time{}
		}
		if r.Unit == harnessprofile.ResetEpoch {
			at = time.Unix(value, 0)
		} else {
			at = now.Add(time.Duration(value) * time.Second)
		}
	case harnessprofile.ResetClock:
		if zone == nil {
			zone = time.UTC
		}
		at = clockReset(rest, now, zone)
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

// clockReset reads a reset written as a time of day, a weekday and a time, or a
// date and a time, each optionally followed by the zone it is in:
//
//	4:30pm (America/Vancouver)    4pm    16:30 (Asia/Tokyo)    4:31 PM
//	Mon 9am (Europe/Berlin)       Monday 9:00 am
//	Nov 3rd, 2026 3:45 PM         Nov 3, 9am (America/New_York)
//
// A TIME ALONE is its NEXT occurrence: the first no more than
// [clockResetGrace] before now, because the vendor names a time and not a
// date, and a reset is always ahead of the report that names it. A reset named
// at 11 pm for "2am" is the coming night's; one named at 4:16 pm for "5am" is
// tomorrow's, which the nearest occurrence read as this morning's and so as no
// instant; and one named for "4:30pm" a minute after half past four is the one
// that has just gone by (no instant), not tomorrow's.
//
// A WEEKDAY is that day's next occurrence STRICTLY AFTER the report: a weekly
// window that resets "Mon 9am", reported on a Monday at ten, resets next
// Monday. A DATE carries its own day, and its year where it names one; with
// none, the year whose occurrence is the next.
//
// THE ZONE is the parenthesised one where the sentence names it, else zone,
// the zone the CLI ran in: the engine hands every child, on its host and in a
// box, an explicit TZ ([childZone]), so a sentence without a zone is read in
// the zone it was written in rather than guessed. A parenthesised zone the
// system's zone database does not know is no instant.
func clockReset(rest string, now time.Time, zone *time.Location) time.Time {
	rest = strings.TrimLeft(rest, " ")
	var (
		weekday          = -1
		month, day, year int
	)
	if w, after, ok := readWeekday(rest); ok {
		weekday, rest = w, after
	} else if m, d, y, after, ok := readDate(rest); ok {
		month, day, year, rest = m, d, y, after
	}
	hour, minute, rest, ok := readTimeOfDay(rest)
	if !ok {
		return time.Time{}
	}
	if trimmed := strings.TrimLeft(rest, " "); strings.HasPrefix(trimmed, "(") {
		shut := strings.Index(trimmed, ")")
		if shut < 0 {
			return time.Time{}
		}
		named, err := time.LoadLocation(strings.TrimSpace(trimmed[1:shut]))
		if err != nil {
			return time.Time{}
		}
		zone = named
	}
	local := now.In(zone)
	switch {
	case month != 0:
		years := []int{year}
		if year == 0 {
			years = []int{local.Year(), local.Year() + 1}
		}
		for _, y := range years {
			at := time.Date(y, time.Month(month), day, hour, minute, 0, 0, zone)
			if at.Day() != day {
				// "Feb 30th": time.Date normalises it into March.
				return time.Time{}
			}
			if !at.Before(now.Add(-clockResetGrace)) {
				return at
			}
		}
		return time.Time{}
	case weekday >= 0:
		for offset := range 8 {
			at := time.Date(local.Year(), local.Month(), local.Day()+offset, hour, minute, 0, 0, zone)
			if int(at.Weekday()) == weekday && at.After(now) {
				return at
			}
		}
		return time.Time{}
	}
	earliest := now.Add(-clockResetGrace)
	for _, offset := range []int{-1, 0, 1} {
		if at := time.Date(local.Year(), local.Month(), local.Day()+offset, hour, minute, 0, 0, zone); !at.Before(earliest) {
			return at
		}
	}
	return time.Time{}
}

// readTimeOfDay reads "4:30pm", "4 PM", "16:30" or "9am" off the front of s.
func readTimeOfDay(s string) (hour, minute int, rest string, ok bool) {
	hour, rest, ok = clockNumber(s, 2)
	if !ok {
		return 0, 0, s, false
	}
	if after, colon := strings.CutPrefix(rest, ":"); colon {
		var two bool
		minute, rest, two = clockNumber(after, 2)
		if !two || len(after)-len(rest) != 2 || minute > 59 {
			return 0, 0, s, false
		}
	}
	spaced := strings.TrimLeft(rest, " ")
	lower := strings.ToLower(spaced)
	switch {
	case strings.HasPrefix(lower, "am"), strings.HasPrefix(lower, "pm"):
		if hour < 1 || hour > 12 {
			return 0, 0, s, false
		}
		hour %= 12
		if lower[0] == 'p' {
			hour += 12
		}
		rest = spaced[2:]
	case hour > 23:
		return 0, 0, s, false
	}
	return hour, minute, rest, true
}

// weekdays and months are the names a reset may open with, matched without
// case, the full name before its three-letter form.
var (
	weekdays = []string{"sunday", "monday", "tuesday", "wednesday", "thursday", "friday", "saturday"}
	months   = []string{"january", "february", "march", "april", "may", "june", "july",
		"august", "september", "october", "november", "december"}
)

// readName reads one of names, whole or as its first three letters, off the
// front of s, followed by a space or a comma; its index, and what follows.
func readName(s string, names []string) (int, string, bool) {
	lower := strings.ToLower(s)
	for i, name := range names {
		for _, form := range []string{name, name[:3]} {
			after, ok := strings.CutPrefix(lower, form)
			if !ok || (after != "" && after[0] != ' ' && after[0] != ',' && after[0] != '.') {
				continue
			}
			rest := strings.TrimLeft(s[len(form):], ".,")
			return i, strings.TrimLeft(rest, " "), true
		}
	}
	return 0, s, false
}

// readWeekday reads "Mon" or "Monday" off the front of s.
func readWeekday(s string) (int, string, bool) { return readName(s, weekdays) }

// readDate reads "Nov 3rd, 2026", "Nov 3" or "November 3rd" off the front of s,
// with the separators that follow it: the month (1 to 12), the day, the year
// (0 where it names none) and what follows.
func readDate(s string) (month, day, year int, rest string, ok bool) {
	m, rest, named := readName(s, months)
	if !named {
		return 0, 0, 0, s, false
	}
	day, rest, ok = clockNumber(rest, 2)
	if !ok || day < 1 || day > 31 {
		return 0, 0, 0, s, false
	}
	for _, suffix := range []string{"st", "nd", "rd", "th"} {
		if after, cut := strings.CutPrefix(rest, suffix); cut {
			rest = after
			break
		}
	}
	rest = strings.TrimLeft(strings.TrimPrefix(rest, ","), " ")
	if y, after, four := clockNumber(rest, 4); four && len(rest)-len(after) == 4 {
		year, rest = y, strings.TrimLeft(strings.TrimPrefix(after, ","), " ")
	}
	return m + 1, day, year, rest, true
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

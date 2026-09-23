package period_test

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/period"
)

// Every date below that depends on a zone's rules is in the past, so a newer
// time-zone database on the machine running this cannot move it: history is
// the one part of the database nobody edits.

// zone loads a zone by name, failing rather than skipping: Go ships its own
// copy of the database with the toolchain, so a missing zone is a broken
// machine, not an optional case.
func zone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	return loc
}

// wallAt reads "2025-03-09 12:00 -0400" — a wall clock with its offset, which
// is how a DST case has to be written to say which of two readings it means.
func wallAt(t *testing.T, s string) time.Time {
	t.Helper()
	at, err := time.Parse("2006-01-02 15:04 -0700", s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return at
}

// same compares two windows by what they mean — the period, the label and
// the two instants — rather than by the location pointers == would compare.
func same(a, b period.Window) bool {
	return a.Period == b.Period && a.Label == b.Label &&
		a.Start.Equal(b.Start) && a.End.Equal(b.End)
}

// EACH WINDOW IS ITS CALENDAR SPAN ON THE COMPANY'S CLOCK, whatever the clock
// did that day.
//
// The lengths are the point. A day is 23 hours on the day a zone springs
// forward and 25 on the day it falls back, a week holding either is 167 or
// 169, a month 743 or 721 — and Add(24*time.Hour) gets every one of them
// wrong. The zones that move the clock AT midnight are the cases time.Date
// alone gets wrong: Santiago's day begins at the jump, not at a midnight Go
// resolves to the evening before, and Amman's begins at the first of its two
// midnights, not the second.
func TestEachWindowIsItsCalendarSpanOnTheCompanysClock(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		zone   string
		period period.Period
		at     string
		label  string
		start  string
		length time.Duration
		next   string
	}{
		{"an ordinary day", "UTC", period.Day, "2025-09-23 15:00 +0000",
			"2025-09-23", "2025-09-23 00:00 +0000", 24 * time.Hour, "2025-09-24"},
		{"a leap day", "UTC", period.Day, "2024-02-29 23:59 +0000",
			"2024-02-29", "2024-02-29 00:00 +0000", 24 * time.Hour, "2024-03-01"},
		{"the last day of a year", "UTC", period.Day, "2024-12-31 12:00 +0000",
			"2024-12-31", "2024-12-31 00:00 +0000", 24 * time.Hour, "2025-01-01"},
		{"the company's evening is already tomorrow in UTC", "America/New_York", period.Day,
			"2025-09-23 23:30 -0400", "2025-09-23", "2025-09-23 00:00 -0400", 24 * time.Hour, "2025-09-24"},
		{"New York springs forward", "America/New_York", period.Day, "2025-03-09 12:00 -0400",
			"2025-03-09", "2025-03-09 00:00 -0500", 23 * time.Hour, "2025-03-10"},
		{"New York falls back", "America/New_York", period.Day, "2025-11-02 12:00 -0500",
			"2025-11-02", "2025-11-02 00:00 -0400", 25 * time.Hour, "2025-11-03"},
		{"the company's morning is still yesterday in UTC", "Pacific/Chatham", period.Day,
			"2025-09-22 12:00 +0000", "2025-09-23", "2025-09-23 00:00 +1245", 24 * time.Hour, "2025-09-24"},
		{"Chatham falls back at 03:45", "Pacific/Chatham", period.Day, "2025-04-06 12:00 +1245",
			"2025-04-06", "2025-04-06 00:00 +1345", 25 * time.Hour, "2025-04-07"},
		{"Chatham springs forward at 02:45", "Pacific/Chatham", period.Day, "2025-09-28 12:00 +1345",
			"2025-09-28", "2025-09-28 00:00 +1245", 23 * time.Hour, "2025-09-29"},
		{"Santiago jumps over midnight, and the day begins at the jump",
			"America/Santiago", period.Day, "2024-09-08 12:00 -0300",
			"2024-09-08", "2024-09-08 01:00 -0300", 23 * time.Hour, "2024-09-09"},
		{"the Santiago evening before the jump is still its own day",
			"America/Santiago", period.Day, "2024-09-07 23:30 -0400",
			"2024-09-07", "2024-09-07 00:00 -0400", 24 * time.Hour, "2024-09-08"},
		{"Beirut jumps over midnight too", "Asia/Beirut", period.Day, "2024-03-31 12:00 +0300",
			"2024-03-31", "2024-03-31 01:00 +0300", 23 * time.Hour, "2024-04-01"},
		{"Amman repeats midnight, and the day begins at the first",
			"Asia/Amman", period.Day, "2021-10-29 00:30 +0300",
			"2021-10-29", "2021-10-29 00:00 +0300", 25 * time.Hour, "2021-10-30"},
		{"Havana repeats midnight, asked from the second", "America/Havana", period.Day,
			"2024-11-03 00:30 -0500", "2024-11-03", "2024-11-03 00:00 -0400", 25 * time.Hour, "2024-11-04"},

		{"a week runs Monday to Monday", "UTC", period.Week, "2025-09-28 23:59 +0000",
			"2025-W39", "2025-09-22 00:00 +0000", 7 * 24 * time.Hour, "2025-W40"},
		{"Monday's first instant is the new week's", "UTC", period.Week, "2025-09-29 00:00 +0000",
			"2025-W40", "2025-09-29 00:00 +0000", 7 * 24 * time.Hour, "2025-W41"},
		{"ISO week 53 is whole across New Year", "UTC", period.Week, "2021-01-01 12:00 +0000",
			"2020-W53", "2020-12-28 00:00 +0000", 7 * 24 * time.Hour, "2021-W01"},
		{"week 1 can begin in December", "UTC", period.Week, "2024-12-31 12:00 +0000",
			"2025-W01", "2024-12-30 00:00 +0000", 7 * 24 * time.Hour, "2025-W02"},
		{"2026 is a year of 53 weeks", "UTC", period.Week, "2026-12-31 12:00 +0000",
			"2026-W53", "2026-12-28 00:00 +0000", 7 * 24 * time.Hour, "2027-W01"},
		{"a week holding a spring-forward", "America/New_York", period.Week, "2025-03-05 12:00 -0500",
			"2025-W10", "2025-03-03 00:00 -0500", 167 * time.Hour, "2025-W11"},
		{"a week holding a fall-back", "Pacific/Chatham", period.Week, "2025-04-02 12:00 +1345",
			"2025-W14", "2025-03-31 00:00 +1345", 169 * time.Hour, "2025-W15"},

		{"a leap February", "UTC", period.Month, "2024-02-10 12:00 +0000",
			"2024-02", "2024-02-01 00:00 +0000", 29 * 24 * time.Hour, "2024-03"},
		{"a plain February", "UTC", period.Month, "2025-02-10 12:00 +0000",
			"2025-02", "2025-02-01 00:00 +0000", 28 * 24 * time.Hour, "2025-03"},
		{"December rolls into the next year", "UTC", period.Month, "2025-12-31 23:59 +0000",
			"2025-12", "2025-12-01 00:00 +0000", 31 * 24 * time.Hour, "2026-01"},
		{"a month holding a spring-forward", "America/New_York", period.Month, "2025-03-15 12:00 -0400",
			"2025-03", "2025-03-01 00:00 -0500", 31*24*time.Hour - time.Hour, "2025-04"},
		{"a month holding a fall-back", "Pacific/Chatham", period.Month, "2025-04-15 12:00 +1245",
			"2025-04", "2025-04-01 00:00 +1345", 30*24*time.Hour + time.Hour, "2025-05"},
		{"a month whose midnight jump is not its first day", "America/Santiago", period.Month,
			"2024-09-15 12:00 -0300", "2024-09", "2024-09-01 00:00 -0400", 30*24*time.Hour - time.Hour, "2024-10"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			loc := zone(t, c.zone)
			at := wallAt(t, c.at)
			w := period.At(c.period, at, loc)
			if w.Period != c.period || w.Label != c.label {
				t.Fatalf("At(%s, %s) is %s %q, want %q", c.period, c.at, w.Period, w.Label, c.label)
			}
			if want := wallAt(t, c.start); !w.Start.Equal(want) {
				t.Errorf("%s begins at %v, want %v", w.Label, w.Start, want.In(loc))
			}
			if got := w.End.Sub(w.Start); got != c.length {
				t.Errorf("%s is %v long, want %v", w.Label, got, c.length)
			}
			if !w.Contains(at) {
				t.Errorf("%s [%v, %v) does not contain the moment it was cut for, %v",
					w.Label, w.Start, w.End, at.In(loc))
			}
			if next := w.Next(); next.Label != c.next || !next.Start.Equal(w.End) {
				t.Errorf("after %s comes %q at %v, want %q at %v", w.Label, next.Label,
					next.Start, c.next, w.End)
			}
		})
	}
}

// transitions is every instant between from and to at which loc's clock
// changes.
func transitions(loc *time.Location, from, to time.Time) []time.Time {
	var out []time.Time
	for t := from; t.Before(to); {
		_, end := t.In(loc).ZoneBounds()
		if end.IsZero() || !end.Before(to) {
			break
		}
		out = append(out, end)
		t = end
	}
	return out
}

// expectedLabel is the label a moment's own local date names, written from
// the definition rather than from the package's arithmetic.
func expectedLabel(p period.Period, local time.Time) string {
	switch p {
	case period.Day:
		return local.Format("2006-01-02")
	case period.Month:
		return local.Format("2006-01")
	}
	year, week := local.ISOWeek()
	return fmt.Sprintf("%04d-W%02d", year, week)
}

// EVERY MOMENT FALLS IN THE WINDOW [period.At] CUTS FOR IT, and in no other.
//
// Asked around every clock change a zone made in six years — and on a sweep
// between them — the window contains the moment, carries the label of the
// moment's own local date, is the same window asked from either of its ends,
// and ends exactly where the next begins. The zones are the ones whose
// changes land at midnight, where a boundary taken straight from time.Date
// puts a moment outside the window cut for it, plus two ordinary ones and a
// zone three-quarters of an hour off the hour.
func TestEveryMomentFallsInTheWindowAtCutsForIt(t *testing.T) {
	t.Parallel()
	from := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	offsets := []time.Duration{-25 * time.Hour, -time.Hour, -time.Second, -time.Nanosecond,
		0, time.Nanosecond, time.Second, 30 * time.Minute, time.Hour, 90 * time.Minute, 25 * time.Hour}
	for _, name := range []string{"UTC", "America/New_York", "Pacific/Chatham", "America/Santiago",
		"America/Asuncion", "America/Havana", "Asia/Beirut", "Africa/Cairo", "Asia/Amman", "Asia/Gaza"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			loc := zone(t, name)
			var moments []time.Time
			for _, change := range transitions(loc, from, to) {
				for _, off := range offsets {
					moments = append(moments, change.Add(off))
				}
			}
			// And a sweep between the changes, on a step that drifts
			// through every hour of the day.
			for at := from; at.Before(to); at = at.Add(7*time.Hour + 13*time.Minute) {
				moments = append(moments, at)
			}
			for _, at := range moments {
				for _, p := range period.Periods {
					w := period.At(p, at, loc)
					if !w.Contains(at) {
						t.Fatalf("%s window %s [%v, %v) does not contain %v",
							p, w.Label, w.Start, w.End, at.In(loc))
					}
					if want := expectedLabel(p, at.In(loc)); w.Label != want {
						t.Fatalf("%v falls in %s %q, want %q", at.In(loc), p, w.Label, want)
					}
					if back := period.At(p, w.Start, loc); !same(back, w) {
						t.Fatalf("%s asked from its own start is %s", w.Label, back.Label)
					}
					if back := period.At(p, w.End.Add(-time.Nanosecond), loc); !same(back, w) {
						t.Fatalf("%s asked from its last instant is %s", w.Label, back.Label)
					}
					if next := w.Next(); !next.Start.Equal(w.End) {
						t.Fatalf("%s ends at %v and %s begins at %v", w.Label, w.End,
							next.Label, next.Start)
					}
				}
			}
		})
	}
}

// WALKING THE CALENDAR NEVER LEAVES A GAP OR AN OVERLAP, and a label is a
// value the rest of the engine can use as it stands.
//
// Four hundred steps of each period from 2020 in each zone: every window
// begins exactly where the last ended and is not empty, labels strictly
// increase as strings — which is what makes a retention horizon a string
// comparison — and are spelled only in digits, `-` and `W`, so a label is a
// subject token and a coordination key as it stands. A label parses back to
// the same window, and a shift is the same walk taken in one step, in either
// direction.
func TestWalkingTheCalendarNeverLeavesAGapOrAnOverlap(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"UTC", "America/New_York", "Pacific/Chatham",
		"America/Santiago", "Asia/Amman", "America/Havana", "Asia/Beirut"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			loc := zone(t, name)
			for _, p := range period.Periods {
				w := period.At(p, time.Date(2020, 1, 1, 12, 0, 0, 0, loc), loc)
				for step := range 400 {
					if !w.End.After(w.Start) {
						t.Fatalf("%s %s is empty: [%v, %v)", p, w.Label, w.Start, w.End)
					}
					if strings.Trim(w.Label, "0123456789-W") != "" {
						t.Fatalf("%s label %q spells more than digits, '-' and 'W'", p, w.Label)
					}
					if parsed, err := period.Parse(p, w.Label, loc); err != nil || !same(parsed, w) {
						t.Fatalf("%s %q parses back to %+v, %v", p, w.Label, parsed, err)
					}
					next := w.Next()
					if !next.Start.Equal(w.End) {
						t.Fatalf("step %d: %s ends at %v and %s begins at %v", step,
							w.Label, w.End, next.Label, next.Start)
					}
					if next.Label <= w.Label {
						t.Fatalf("step %d: %q follows %q, and labels must sort in calendar order",
							step, next.Label, w.Label)
					}
					if back := next.Shift(-1); !same(back, w) {
						t.Fatalf("the window before %s is %s, want %s", next.Label, back.Label, w.Label)
					}
					if three := w.Shift(3); !same(three, next.Next().Next()) {
						t.Fatalf("three %ss after %s is %s, want the walk's %s", p, w.Label,
							three.Label, next.Next().Next().Label)
					}
					w = next
				}
			}
		})
	}
}

// A DATE THE ZONE SKIPPED IS AN EMPTY WINDOW, AND NOTHING FALLS IN IT.
//
// Samoa crossed the date line by going from the last second of 29 December
// 2011 straight to 31 December. The walk still passes through the 30th, so
// labels stay the plain calendar and every window still begins where the
// last ended — but the 30th begins and ends at the same instant, and At never
// answers it, because no moment is in it.
func TestADateTheZoneSkippedIsAnEmptyWindow(t *testing.T) {
	t.Parallel()
	apia := zone(t, "Pacific/Apia")
	before := period.At(period.Day, wallAt(t, "2011-12-29 12:00 -1000"), apia)
	skipped := before.Next()
	if skipped.Label != "2011-12-30" {
		t.Fatalf("after %s comes %q", before.Label, skipped.Label)
	}
	if !skipped.Start.Equal(before.End) || !skipped.End.Equal(skipped.Start) {
		t.Fatalf("the skipped day is [%v, %v), want empty at %v", skipped.Start, skipped.End, before.End)
	}
	if skipped.Contains(skipped.Start) {
		t.Fatal("an empty window contains its own start")
	}
	after := skipped.Next()
	if after.Label != "2011-12-31" || !after.Start.Equal(before.End) || after.End.Sub(after.Start) != 24*time.Hour {
		t.Fatalf("after the skipped day comes %s [%v, %v)", after.Label, after.Start, after.End)
	}
	if got := period.At(period.Day, before.End, apia); got.Label != "2011-12-31" {
		t.Fatalf("the first instant after the jump falls in %q", got.Label)
	}
	if got := period.At(period.Week, before.End, apia); got.Label != "2011-W52" ||
		got.End.Sub(got.Start) != 6*24*time.Hour {
		t.Fatalf("the week that lost a day is %s, %v long", got.Label, got.End.Sub(got.Start))
	}
}

// A LABEL THE CALENDAR DOES NOT HAVE IS REFUSED, never rolled into one it
// does.
//
// 2026-02-29 normalised would be 1 March, and week 53 of a 52-week year would
// be the next year's week 1: a date somebody typed that the calendar quietly
// moved is a filter or a budget answering a question nobody asked. Only the
// canonical spelling At writes is read, so one window has one label.
func TestALabelTheCalendarDoesNotHaveIsRefused(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		period period.Period
		label  string
	}{
		{period.Day, "2026-02-29"},
		{period.Day, "2026-04-31"},
		{period.Day, "2026-13-01"},
		{period.Day, "2026-00-10"},
		{period.Day, "2026-09-00"},
		{period.Day, "2026-9-23"},
		{period.Day, " 2026-09-23"},
		{period.Day, "2026-09-23T00:00:00Z"},
		{period.Day, "+026-09-23"},
		{period.Day, "2026/09/23"},
		{period.Day, "2026-W39"},
		{period.Day, ""},
		{period.Week, "2025-W53"},
		{period.Week, "2026-W00"},
		{period.Week, "2026-W54"},
		{period.Week, "2026-w39"},
		{period.Week, "2026-W9"},
		{period.Week, "2026-09"},
		{period.Month, "2026-00"},
		{period.Month, "2026-13"},
		{period.Month, "2026-9"},
		{period.Month, "2026-09-01"},
	} {
		if w, err := period.Parse(c.period, c.label, time.UTC); err == nil {
			t.Errorf("%s %q parsed to %s", c.period, c.label, w.Label)
		} else if !strings.Contains(err.Error(), "2026-W39") {
			t.Errorf("refusing %q does not show the shapes a label takes: %v", c.label, err)
		}
	}
	for _, c := range []struct {
		period period.Period
		label  string
	}{
		{period.Day, "2024-02-29"},
		{period.Day, "0000-01-01"},
		{period.Day, "9999-12-31"},
		{period.Week, "2020-W53"},
		{period.Week, "2026-W53"},
		{period.Week, "2026-W01"},
		{period.Month, "2026-12"},
	} {
		w, err := period.Parse(c.period, c.label, nil)
		if err != nil || w.Label != c.label {
			t.Errorf("%s %q parsed to %q, %v", c.period, c.label, w.Label, err)
		}
	}
	if _, err := period.Parse("fortnight", "2026-09-23", time.UTC); err == nil ||
		!strings.Contains(err.Error(), "day, week or month") {
		t.Errorf("an unknown period is refused naming the three, got %v", err)
	}
}

// THE CALENDAR ENDS WHERE A LABEL CAN NO LONGER SPELL THE YEAR.
//
// Four digits, so 0000 to 9999 — the years RFC 3339 can spell. Outside them
// there is no window rather than a label with a fifth digit or a sign in it,
// which would stop sorting and stop parsing. A shift big enough to overflow
// its own arithmetic is refused before it can wrap round into a year that
// happens to be spellable.
func TestTheCalendarEndsWhereALabelCanNoLongerSpellTheYear(t *testing.T) {
	t.Parallel()
	if w := period.At(period.Day, time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC), time.UTC); w.Period != "" {
		t.Errorf("year 10000 has the window %q", w.Label)
	}
	if w := period.At(period.Day, time.Time{}, nil); w.Label != "0001-01-01" || !w.Start.Equal(time.Time{}) {
		t.Errorf("the zero instant falls in %q from %v", w.Label, w.Start)
	}
	last := period.At(period.Day, time.Date(9999, 12, 31, 12, 0, 0, 0, time.UTC), time.UTC)
	if last.Label != "9999-12-31" {
		t.Fatalf("the last day is %q", last.Label)
	}
	if w := last.Next(); w.Period != "" {
		t.Errorf("after the last spellable day comes %q", w.Label)
	}
	firstDay, err := period.Parse(period.Day, "0000-01-01", time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if w := firstDay.Shift(-1); w.Period != "" {
		t.Errorf("before the first spellable day comes %q", w.Label)
	}
	today := period.At(period.Week, time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC), time.UTC)
	for _, n := range []int{math.MaxInt, math.MinInt, math.MaxInt/7 + 1, 3_652_426, -3_652_426} {
		if w := today.Shift(n); w.Period != "" {
			t.Errorf("a shift of %d weeks lands on %q", n, w.Label)
		}
	}
	if w := today.Shift(-100_000); w.Label != "0110-W11" {
		t.Errorf("a long shift inside the range lands on %q", w.Label)
	}
}

// THE ZERO WINDOW NAMES NOTHING, and an unknown period is a value, not a
// panic.
func TestTheZeroWindowNamesNothing(t *testing.T) {
	t.Parallel()
	for _, p := range []period.Period{"", "Day", "fortnight", "days"} {
		if p.Valid() {
			t.Errorf("%q is valid", p)
		}
		if w := period.At(p, time.Now(), time.UTC); w.Period != "" || w.Label != "" {
			t.Errorf("At(%q) is %+v", p, w)
		}
	}
	var zero period.Window
	if next := zero.Next(); !same(next, zero) {
		t.Errorf("the zero window's next is %+v", next)
	}
	if zero.Contains(time.Time{}) {
		t.Error("the zero window contains the zero instant")
	}
	handmade := period.Window{Period: period.Day, Label: "yesterday"}
	if next := handmade.Next(); next.Period != "" {
		t.Errorf("a window with an unreadable label walks to %q", next.Label)
	}
}

// PERIODS IS EVERY PERIOD, SHORTEST FIRST.
func TestPeriodsIsEveryPeriodShortestFirst(t *testing.T) {
	t.Parallel()
	at := time.Date(2025, 9, 23, 12, 0, 0, 0, time.UTC)
	var longest time.Duration
	seen := map[period.Period]bool{}
	for _, p := range period.Periods {
		if !p.Valid() || seen[p] {
			t.Fatalf("%q is listed twice or is not valid", p)
		}
		seen[p] = true
		w := period.At(p, at, time.UTC)
		if length := w.End.Sub(w.Start); length <= longest {
			t.Errorf("%s (%v) is listed after a period at least as long", p, length)
		} else {
			longest = length
		}
	}
	for _, p := range []period.Period{period.Day, period.Week, period.Month} {
		if !seen[p] {
			t.Errorf("%s is not listed", p)
		}
	}
}

// DAYS IS EVERY DAY THE RANGE TOUCHES, WHOLE, in order.
//
// Half-open like every range here: a `to` exactly at a midnight does not
// bring that day in, and one nanosecond past it does. An empty range has no
// days rather than the one its start falls in.
func TestDaysIsEveryDayTheRangeTouches(t *testing.T) {
	t.Parallel()
	ny := zone(t, "America/New_York")
	from := wallAt(t, "2025-03-08 12:00 -0500")
	midnight := wallAt(t, "2025-03-10 00:00 -0400")
	labels := func(days []period.Window) []string {
		var out []string
		for _, d := range days {
			out = append(out, d.Label)
		}
		return out
	}
	got := period.Days(from, midnight, ny)
	if strings.Join(labels(got), ",") != "2025-03-08,2025-03-09" {
		t.Fatalf("Days is %v", labels(got))
	}
	if !got[0].Start.Equal(wallAt(t, "2025-03-08 00:00 -0500")) {
		t.Errorf("the first day begins at %v, not at its own midnight", got[0].Start)
	}
	if got[1].End.Sub(got[1].Start) != 23*time.Hour {
		t.Errorf("the spring-forward day is %v long", got[1].End.Sub(got[1].Start))
	}
	if got := period.Days(from, midnight.Add(time.Nanosecond), ny); len(got) != 3 {
		t.Errorf("a range one nanosecond past midnight touches %v", labels(got))
	}
	for _, to := range []time.Time{from, from.Add(-time.Hour)} {
		if got := period.Days(from, to, ny); got != nil {
			t.Errorf("an empty range touches %v", labels(got))
		}
	}
	if got := period.Days(from, from.Add(time.Nanosecond), nil); len(got) != 1 || got[0].Label != "2025-03-08" {
		t.Errorf("a nil location is UTC, and one nanosecond of it touches %v", labels(got))
	}
}

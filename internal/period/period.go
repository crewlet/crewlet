// Package period is the company calendar: which day, which week and which
// month a moment falls in, cut on the company's own clock.
//
// A [Window] is one of those spans — the half-open interval of instants from
// the moment it begins to the moment the next one does — together with the
// LABEL that names it: `2026-09-23` for a day, `2026-W39` for a week,
// `2026-09` for a month. [At] finds the window a moment falls in, [Window.Next]
// and [Window.Shift] walk from one to another, [Parse] reads a label back,
// [Days] lists the days a range touches, and [LoadZone] reads the name of the
// clock all of them are cut on — refusing the two names that mean whichever
// host happens to read them.
//
// # Labels, not instants
//
// The label is the window's identity, and every node computes it without
// asking anybody. That is what the things built on this package need: a
// counter keyed on a window rolls over by COMPARING LABELS — a slot holding a
// label that is not the current window's belongs to an earlier window, so the
// roll is a string inequality inside the write that already happens, with no
// scheduled reset and no instant arithmetic — and two nodes racing to roll the
// same slot both write the same label. An instant would not do that job.
// Two hosts whose time-zone databases disagree about a transition compute
// different instants for a window's edge but the same label for every moment
// outside the disputed hour, so a slot keyed on the edge would be rolled back
// and forth by the two of them on every charge in that window, each reading
// the other's edge as a different window.
//
// A label is also a VALUE the rest of the engine can use as it stands. Labels
// of one period sort lexically in calendar order, so a retention horizon over
// a table keyed on them is a string comparison rather than a date parse per
// row. Their alphabet is digits, `-` and `W`, so a label is a valid subject
// token and a valid coordination key without escaping. And the three shapes
// never collide, so a label names its period as well as its span.
//
// # The calendar is walked by date, and the instants follow
//
// A day runs from one local midnight to the next, found with [time.Date]
// rather than by adding 24 hours: on the day a zone springs forward the day is
// 23 hours long and on the day it falls back it is 25, and `Add(24*time.Hour)`
// puts every boundary after either an hour off for half the year. [Window.Next]
// and [Window.Shift] do their arithmetic on the label's DATE — the next day,
// the Monday seven days on, the first of the next month — and only then ask
// what instant that date begins at, so an error in one boundary can never
// carry into the next.
//
// # time.Date is not the whole answer either
//
// Some zones move their clocks AT midnight rather than in the small hours:
// Chile, Cuba, Lebanon and Egypt spring forward from 00:00 to 01:00, and Cuba
// falls back from 01:00 to 00:00, as Jordan and Palestine have. Asked for a
// midnight that did not happen, or that happened twice, [time.Date] promises a
// time "correct in one of the two zones involved in the transition" and does
// not say which — and the choice it makes is the wrong one often enough to
// matter. Measured: `time.Date(2024, 9, 8, 0, 0, 0, 0, America/Santiago)` is
// 23:00 on the 7th, an hour of the PREVIOUS day, so the window it would start
// does not contain the moment it was asked about; and on Asia/Amman's
// 29 October 2021 it is the second of the two midnights, so the day would
// begin an hour late. A day here begins at the FIRST instant whose wall clock
// reads its date — the jump itself when midnight was skipped, the first of
// the two when it was repeated — which is the only reading under which every
// instant falls in exactly one day.
//
// A date a zone skipped outright — Samoa crossed the date line by going
// straight from 29 to 31 December 2011 — is an EMPTY window: it begins and
// ends at the same instant, contains nothing, and [At] never returns it,
// because no moment falls in it. The walk still passes through it, so labels
// stay the plain calendar and [Window.Next] always begins where its window
// ended.
//
// # The week is the ISO week, from Monday
//
// Monday, and a constant rather than a setting: a week label exists so that
// "this week" names the same seven days in a budget, in a spend series, in a
// saved view and in a seat's tool call, and a per-person or per-company week
// start would make one label two different spans depending on who computed it.
// And ISO numbering, because a week has to belong to ONE year: 28 December 2026
// to 3 January 2027 is `2026-W53`, whole, where a week-of-year counted from
// 1 January would split it at New Year into two partial weeks — and a budget
// that reset in the middle of a week would be a fourth period nobody declared.
//
// # The years a label can spell
//
// A label's year is four digits, so the calendar covers the years 0000 to
// 9999 — the years an RFC 3339 timestamp can spell, which every instant this
// engine puts on the wire already is. A moment outside them has no window:
// [At], [Window.Next] and [Window.Shift] answer the zero Window, which names no
// period and contains nothing.
//
// A leaf: it imports nothing from the engine, so any layer may use it —
// `config` included.
package period

import (
	"fmt"
	"time"
)

// Period is the length of a window: a day, a week or a month.
//
// The constants are also their wire spelling, so an unknown value read off
// the wire is a Period that fails [Period.Valid], never a panic.
type Period string

// The three periods.
const (
	Day   Period = "day"
	Week  Period = "week"
	Month Period = "month"
)

// Periods is every period, shortest first.
//
// An ARRAY rather than a slice, so its length is a constant: a caller holding
// one value per period sizes it `[len(period.Periods)]`, and a fourth period
// added here resizes every such array at compile time rather than leaving one
// a slot short.
var Periods = [...]Period{Day, Week, Month}

// Valid reports whether p is one of the three periods.
func (p Period) Valid() bool {
	switch p {
	case Day, Week, Month:
		return true
	}
	return false
}

// Window is one day, week or month on one clock.
//
// Start and End are the half-open interval the window covers — Start is its
// first instant and End is the next window's first — READ IN THE LOCATION IT
// WAS CUT IN. That location is the calendar [Window.Next] and [Window.Shift]
// walk, so a window is not a wire type: put its label and its two instants on
// the wire (converted with UTC, as every timestamp is), never a window whose
// Start has been converted, which would walk the calendar in UTC.
//
// The zero Window names no period, has no label and contains nothing.
type Window struct {
	Period Period

	// Label names the window, and it is the window's identity; see the
	// package doc.
	Label string

	Start time.Time
	End   time.Time
}

// At is the window of period p that t falls in, on the clock loc.
//
// A nil loc is UTC, as it is for [time.Time]. An invalid period, or a moment
// whose year in loc a label cannot spell, answers the zero Window.
func At(p Period, t time.Time, loc *time.Location) Window {
	if loc == nil {
		loc = time.UTC
	}
	y, m, d := t.In(loc).Date()
	return cut(p, first(p, date{y, m, d}), loc)
}

// Next is the window that begins where this one ends.
//
// Always exactly there: the walk is by date, so Next().Start equals End
// whatever the clocks did in between. The zero Window's Next is the zero
// Window.
func (w Window) Next() Window {
	return w.Shift(1)
}

// Shift is the window n periods after this one, or before it when n is
// negative.
//
// Calendar arithmetic on the label's date — n days, n weeks of seven days, or
// n months from the first — and never on the window's instants, so the answer
// is the same window whatever DST did to the ones in between. A window whose
// label does not parse (the zero Window, or one assembled by hand), or a
// shift past the years a label can spell, answers the zero Window.
func (w Window) Shift(n int) Window {
	if n > maxShift || n < -maxShift {
		return Window{}
	}
	loc := w.Start.Location()
	from, ok := parse(w.Period, w.Label)
	if !ok {
		return Window{}
	}
	switch w.Period {
	case Day:
		return cut(w.Period, from.add(0, n), loc)
	case Week:
		return cut(w.Period, from.add(0, 7*n), loc)
	default:
		return cut(w.Period, from.add(n, 0), loc)
	}
}

// Contains reports whether t falls inside the window.
func (w Window) Contains(t time.Time) bool {
	return !t.Before(w.Start) && t.Before(w.End)
}

// Parse reads a label back into its window on the clock loc.
//
// The label must be the canonical one [At] writes — `2026-09-23`, `2026-W39`,
// `2026-09` — and name a date that exists: 2026-02-29 and a week 53 in a year
// of 52 are refused rather than rolled into the next month or year, because a
// date somebody typed that the calendar quietly moved is a filter or a budget
// answering a question nobody asked. A nil loc is UTC.
func Parse(p Period, label string, loc *time.Location) (Window, error) {
	if !p.Valid() {
		return Window{}, fmt.Errorf("period: %q is not a period; name day, week or month", p)
	}
	from, ok := parse(p, label)
	if !ok {
		return Window{}, fmt.Errorf("period: %q is not a %s; a day is written "+
			"2026-09-23, a week 2026-W39 and a month 2026-09", label, p)
	}
	if loc == nil {
		loc = time.UTC
	}
	return cut(p, from, loc), nil
}

// Days is every day from the one from falls in to the last that begins
// before to, in order, on the clock loc.
//
// The days the half-open range [from, to) touches, whole: the first may begin
// before from and the last may end after to. A range that is empty — to at or
// before from — has no days. The walk is one [Window.Next] per day, so its
// cost is the range's length in days, and a caller bounds the range it asks
// for.
func Days(from, to time.Time, loc *time.Location) []Window {
	if !from.Before(to) {
		return nil
	}
	var days []Window
	for w := At(Day, from, loc); w.Period.Valid() && w.Start.Before(to); w = w.Next() {
		days = append(days, w)
	}
	return days
}

// date is a calendar date with no clock and no zone: what a label names.
type date struct {
	y int
	m time.Month
	d int
}

// noon is the date as an instant on a clock with no transitions, which is
// where the calendar's own arithmetic happens: weekday, ISO week, and the
// normalisation of a day or month that ran past the end of its container.
func (c date) noon() time.Time {
	return time.Date(c.y, c.m, c.d, 12, 0, 0, 0, time.UTC)
}

// add moves the date by months and then days, normalised.
func (c date) add(months, days int) date {
	y, m, d := time.Date(c.y, c.m+time.Month(months), c.d+days, 12, 0, 0, 0, time.UTC).Date()
	return date{y, m, d}
}

// first is the first date of the period-p window that c falls in.
func first(p Period, c date) date {
	switch p {
	case Week:
		// Monday is 0: Go counts from Sunday.
		return c.add(0, -((int(c.noon().Weekday()) + 6) % 7))
	case Month:
		return date{c.y, c.m, 1}
	}
	return c
}

// cut is the period-p window whose first date is from, on the clock loc.
func cut(p Period, from date, loc *time.Location) Window {
	var label string
	var year int
	var next date
	switch p {
	case Day:
		year = from.y
		label = fmt.Sprintf("%04d-%02d-%02d", from.y, from.m, from.d)
		next = from.add(0, 1)
	case Week:
		// The ISO year, which is not always the Monday's: 2026-W01 begins
		// on 29 December 2025.
		var week int
		year, week = from.noon().ISOWeek()
		label = fmt.Sprintf("%04d-W%02d", year, week)
		next = from.add(0, 7)
	case Month:
		year = from.y
		label = fmt.Sprintf("%04d-%02d", from.y, from.m)
		next = from.add(1, 0)
	default:
		return Window{}
	}
	if !spellable(year) {
		return Window{}
	}
	return Window{Period: p, Label: label, Start: begins(from, loc), End: begins(next, loc)}
}

// spellable reports whether a year fits the four digits a label gives it.
func spellable(year int) bool {
	return year >= 0 && year <= 9999
}

// maxShift bounds a shift's count before any arithmetic is done with it.
//
// 3 652 425 is the number of days in 10 000 Gregorian years — the whole range
// a label can spell — so a count past it leaves that range whatever the
// period, and refusing it first is what stops `7*n`, or a day number with n
// added to it, overflowing an int and wrapping round into a year that
// happens to be spellable.
const maxShift = 3_652_425

// begins is the first instant whose wall clock in loc reads the date c — the
// moment that day begins there. See the package doc for why this is not
// simply time.Date.
func begins(c date, loc *time.Location) time.Time {
	midnight := time.Date(c.y, c.m, c.d, 0, 0, 0, 0, time.UTC)
	t := time.Date(c.y, c.m, c.d, 0, 0, 0, 0, loc)
	if wall(t).Equal(midnight) {
		// Midnight happened. If the clock was set back across it, it
		// happened twice, and the day began at the first: that one is read
		// on the offset of the zone before the one t is in.
		if start, _ := t.ZoneBounds(); !start.IsZero() {
			_, offset := start.Add(-time.Nanosecond).Zone()
			earlier := midnight.Add(-time.Duration(offset) * time.Second).In(loc)
			if earlier.Before(start) && wall(earlier).Equal(midnight) {
				return earlier
			}
		}
		return t
	}
	// Midnight never happened: the clock jumped over it, and the day began
	// at the jump. t is on one side of it or the other — before it when its
	// wall clock still reads the day before, after it otherwise — and the
	// jump is the edge of t's zone on that side.
	start, end := t.ZoneBounds()
	if wall(t).Before(midnight) {
		return end
	}
	return start
}

// wall is what t's clock reads, as the same numbers on a clock with no zone.
func wall(t time.Time) time.Time {
	y, m, d := t.Date()
	hh, mm, ss := t.Clock()
	return time.Date(y, m, d, hh, mm, ss, t.Nanosecond(), time.UTC)
}

// parse reads a canonical label of period p into the first date of its
// window, refusing any date the calendar does not have.
func parse(p Period, label string) (date, bool) {
	switch p {
	case Day:
		// 2026-09-23
		if len(label) != 10 || label[4] != '-' || label[7] != '-' {
			return date{}, false
		}
		y, okY := digits(label[0:4])
		m, okM := digits(label[5:7])
		d, okD := digits(label[8:10])
		if !okY || !okM || !okD {
			return date{}, false
		}
		c := date{y, time.Month(m), d}
		// A date that normalised to another one (2026-02-30 → 03-02) is
		// not a date the calendar has.
		if c.add(0, 0) != c {
			return date{}, false
		}
		return c, true
	case Week:
		// 2026-W39
		if len(label) != 8 || label[4] != '-' || label[5] != 'W' {
			return date{}, false
		}
		y, okY := digits(label[0:4])
		w, okW := digits(label[6:8])
		if !okY || !okW {
			return date{}, false
		}
		// 4 January is in week 1 of its ISO year, by the standard's own
		// definition, so week 1's Monday is the Monday on or before it.
		monday := first(Week, date{y, time.January, 4}).add(0, 7*(w-1))
		// Week 0, week 54, or week 53 of a year with 52 lands in another
		// ISO year or on another number, and is refused.
		if year, week := monday.noon().ISOWeek(); year != y || week != w {
			return date{}, false
		}
		return monday, true
	case Month:
		// 2026-09
		if len(label) != 7 || label[4] != '-' {
			return date{}, false
		}
		y, okY := digits(label[0:4])
		m, okM := digits(label[5:7])
		if !okY || !okM || m < 1 || m > 12 {
			return date{}, false
		}
		return date{y, time.Month(m), 1}, true
	}
	return date{}, false
}

// digits reads a fixed-width run of ASCII digits — no sign, no space — which
// is the only shape a label's fields take.
func digits(s string) (int, bool) {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		n = n*10 + int(s[i]-'0')
	}
	return n, true
}

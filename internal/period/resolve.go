package period

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// RELATIVE DATES: the tokens a person types for a date ("today", "eow",
// "+7d", a calendar date, an instant), resolved on the company's clock. Here,
// in the calendar's own package, so every reader of a date token (the
// tracker's filters, the act tool that writes a due date, and the assistant's
// preview of that write) resolves it through one function and can never
// disagree about which day a token names.

// Anchor is one end of a resolved date range.
type Anchor struct {
	// At is the resolved instant, in UTC.
	At time.Time

	// AllDay marks a token that named a DAY rather than an instant, so a
	// renderer can show "today" rather than midnight.
	AllDay bool
}

// Resolve turns one relative token into an instant.
//
// The tokens are the ones a person types, and they come in three shapes: three
// day names, an offset in days, and eight calendar boundaries. Anything else is
// an RFC3339 timestamp, which is the caller's job rather than this function's;
// so an unknown token is an ERROR here rather than a silent zero, because a
// zero instant is 1 January year one and would match every task ever written.
//
// # The calendar is this package's
//
// Every day, week and month below is a window [At] cut on the
// company's clock, so a `due=` filter and a due band agree with everything
// else the engine cuts on that calendar about where a day or a week begins:
// Monday, the ISO week, for all of them, and a company whose week genuinely
// starts on Sunday writes the two dates, because there is one calendar
// rather than a copy of it here. The copy this
// replaced cut each boundary with time.Date and then stepped from it with
// AddDate, and in the zones that move their clocks at midnight both steps
// were wrong: in Santiago on 8 September 2024, "today" resolved to 23:00 on
// the 7th, so a task due that last hour of the 7th read as due today; in
// Amman on 29 October 2021 it resolved to the second of two midnights, an
// hour late; and every offset or boundary stepped from either carried the
// error with it.
func Resolve(token string, now time.Time, loc *time.Location) (Anchor, error) {
	if loc == nil {
		loc = time.UTC
	}
	// THE KEYWORDS ARE CASE-BLIND AND AN INSTANT IS NOT. RFC 3339 spells its
	// separators `T` and `Z`, and `time.Parse` matches them exactly, so a
	// timestamp lowercased along with the keywords never parsed at all, and
	// every `due=range:<instant>..<instant>` a machine sent was refused as
	// "not a date". The raw token is kept for the instant branch below.
	raw := strings.TrimSpace(token)
	token = strings.ToLower(raw)
	today := At(Day, now, loc)
	week := At(Week, now, loc)
	month := At(Month, now, loc)

	// begins anchors on the first instant of a window the calendar cut. A
	// window the calendar could not cut, an offset that lands outside the
	// years a date can be written in, is refused rather than resolved to
	// the zero instant, which would match every task ever written; and a
	// date past year 9999 could not be written into a task record at all.
	begins := func(w Window) (Anchor, error) {
		if !w.Period.Valid() {
			return Anchor{}, fmt.Errorf("period: %q lands outside the "+
				"years 0000 to 9999 a date can be written in", token)
		}
		return Anchor{At: w.Start.UTC(), AllDay: true}, nil
	}

	switch token {
	case "":
		return Anchor{}, fmt.Errorf("period: an empty date matches every " +
			"task ever written; name an instant or a relative token")
	case "now":
		return Anchor{At: now.UTC()}, nil
	case "today":
		return begins(today)
	case "yesterday":
		return begins(today.Shift(-1))
	case "tomorrow":
		return begins(today.Next())
	case "sow", "eopw":
		return begins(week)
	case "eow", "sonw":
		return begins(week.Next())
	case "sopw":
		return begins(week.Shift(-1))
	case "eonw":
		return begins(week.Shift(2))
	case "som", "eopm":
		return begins(month)
	case "eom", "sonm":
		return begins(month.Next())
	case "sopm":
		return begins(month.Shift(-1))
	case "eonm":
		return begins(month.Shift(2))
	}

	// ±<n>d, the one token with an argument.
	if days, ok := offsetDays(token); ok {
		return begins(today.Shift(days))
	}

	// An absolute instant. Both spellings, because a person types a date
	// and a machine sends a timestamp, and a date is a DAY LABEL, so it is
	// read by the calendar that writes them.
	if at, err := time.Parse(time.RFC3339, raw); err == nil {
		return Anchor{At: at.UTC()}, nil
	}
	if day, err := Parse(Day, token, loc); err == nil {
		return begins(day)
	}
	return Anchor{}, fmt.Errorf("period: %q is not a date: name an instant, "+
		"a calendar date, an offset like +7d, or one of the relative tokens", token)
}

// EVERY END TOKEN IS THE START OF THE NEXT PERIOD, and that is deliberate.
//
// A range is half-open, `>= start AND < end`, so "this week" is Monday
// midnight to next Monday midnight and contains every instant of Sunday. The
// alternative, an end at 23:59:59, silently drops the last second of the last
// day and drops it differently depending on whether the column stores seconds
// or milliseconds. It is also exactly the shape of a [Window], whose
// End is the next window's Start.

// offsetDays reads a ±<n>d token.
func offsetDays(token string) (int, bool) {
	rest, ok := strings.CutSuffix(token, "d")
	if !ok || rest == "" {
		return 0, false
	}
	if rest[0] == '+' {
		rest = rest[1:]
	}
	n, err := strconv.Atoi(rest)
	if err != nil {
		return 0, false
	}
	return n, true
}

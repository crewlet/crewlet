package sandbox

import "github.com/crewlet/crewlet/internal/period"

// BudgetWindow names one capped token window by its identity on the company's
// clock (internal/period): its period and its label, `2026-09-23` for a day,
// `2026-W39` for a week and `2026-09` for a month.
//
// THE LABEL AND ITS PERIOD, NEVER THE WINDOW'S INSTANTS. A window's instants are
// the clock's that cut it, and never a wire value (period.Window); its label is
// what every node computes alike, and the labels of one period sort in calendar
// order, so a reader on any node, after a restart or a move, tells a window that
// has turned over from the one it is in by comparing two strings.
//
// What it names here is the window an executor launch was refused below its
// floor in, the one its budget hold waits on to turn over ([Refusal.Window]):
// the engine holds that launch's next try, in a later window, to no more than
// the floor a launch with no brief needs, so a launch refused below its brief's
// floor, or a session's, waits for one turnover at most (internal/engine's
// launchfloor.go). The zero BudgetWindow names none: never refused, or refused
// on a record a build without the field wrote, which costs that launch at most
// one more window.
type BudgetWindow struct {
	Period period.Period `json:"period,omitempty"`
	Label  string        `json:"label,omitempty"`
}

// Valid reports whether w names a window: one of the three periods, and a label
// of that period's canonical shape naming a date the calendar has
// ([period.Parse]). Nothing else is written to a record, so a record holds at
// most a day's, the widest pair the three periods spell (3 and 10 bytes; a
// week's 4 and 8, a month's 5 and 7).
func (w BudgetWindow) Valid() bool {
	_, err := period.Parse(w.Period, w.Label, nil)
	return err == nil
}

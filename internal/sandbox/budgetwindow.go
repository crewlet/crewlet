package sandbox

import "github.com/crewlet/crewlet/internal/period"

// BudgetWindow names one capped token window on the company's clock
// (internal/period) by its period and label: `2026-09-23`, `2026-W39`,
// `2026-09`. NEVER ITS INSTANTS, the clock's that cut it and never a wire
// value (period.Window): every node computes a label alike and one period's
// labels sort in calendar order, so any node, after a restart or a move, tells
// a window that has turned over by comparing strings.
//
// It names the window a launch was refused below its floor in
// ([Refusal.Window]) or its seat's park (internal/engine's floorWaits); its
// own next try is held, in a later window, to a briefless launch's floor: one
// turnover at most (internal/engine's launchfloor.go). The zero value names
// none: never refused, or an older build's record, a window more at most.
type BudgetWindow struct {
	Period period.Period `json:"period,omitempty"`
	Label  string        `json:"label,omitempty"`
}

// Valid reports whether w names a window: one of the three periods and a
// label of its canonical shape naming a real date ([period.Parse]). Nothing
// else is written to a record, so a record holds at most a day's pair (3 and
// 10 bytes), the widest the three periods spell.
func (w BudgetWindow) Valid() bool {
	_, err := period.Parse(w.Period, w.Label, nil)
	return err == nil
}

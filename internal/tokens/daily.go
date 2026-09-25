package tokens

import (
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/period"
)

// THE NAMED WINDOW: a run of whole company days, read from the replicated
// `usage` domain rather than from any one node's event log.
//
// Every named window is days and nothing finer, and that is a property of the
// source rather than a limitation to work around: a node publishes one record
// per (node, day, seat), cumulative, so a day is the smallest thing every node
// agrees on. An hour, a turn and a "since 14:05" are not in it — and an answer
// that invented them by reading this node's event log for the edges would be
// the one-node answer this domain replaced, glued to a fleet answer at a seam
// nobody can see.

// MaxSpendRangeDays is the longest window a spend answer is asked for.
//
// NINETY, and it is the offered range rather than a round number: the longest
// window the Spend screen offers, and — doubled, for the window before it, plus
// the one day a moved clock can touch — what the usage domain's 181-day history
// was sized to answer. A longer window would be answered with a silently empty
// previous half, which is the bug the domain replaced; so it is refused, naming
// the parameter.
const MaxSpendRangeDays = 90

// ErrOutOfRange is a window that reaches past what the usage domain holds: its
// first day is older than the horizon's floor, or it is longer than
// [MaxSpendRangeDays].
//
// A REFUSAL rather than a truncated answer: the rows before the floor are gone
// on every node, so an answer "floored" to it would put a heading over figures
// covering fewer days than it names — the previous-window chart at 30 and 90
// days read exactly that way, silently empty, while the event log was its
// source.
var ErrOutOfRange = errors.New("tokens: the window reaches past the spend history")

// Horizon is how far back the usage domain can answer, stated on every named
// answer so a reader can say it rather than discover it.
//
// NAMED horizon, not coverage: a fanned answer carries a coverage of the NODES
// that answered, and this one asked no node — it is read from the replicated
// estate, which every node holds whole. What bounds it is time, not presence.
type Horizon struct {
	// Days is the history the domain keeps, in days (usage.History).
	Days int `json:"days"`

	// Floor is the oldest company day still answerable, as its label.
	Floor string `json:"floor"`
}

// NewHorizon is the horizon as seen on the company day today: the floor is the
// day the usage applier's own horizon keeps last — today minus days.
func NewHorizon(days int, today period.Window) Horizon {
	return Horizon{Days: days, Floor: today.Shift(-days).Label}
}

// Admits reports whether a range lies wholly inside the horizon. Labels of one
// period sort in calendar order, so this is a string comparison.
func (h Horizon) Admits(r Range) bool { return r.First.Label >= h.Floor }

// Range is a run of whole company days, First to Last inclusive, cut on the
// company's clock.
type Range struct {
	First, Last period.Window
}

// LastDays is the n company days ending with the one now falls in.
func LastDays(n int, now time.Time, loc *time.Location) Range {
	today := period.At(period.Day, now, loc)
	return Range{First: today.Shift(-(n - 1)), Last: today}
}

// DaysBetween is the range from one company day to another, both inclusive,
// read from their labels.
func DaysBetween(first, last string, loc *time.Location) (Range, error) {
	a, err := period.Parse(period.Day, first, loc)
	if err != nil {
		return Range{}, err
	}
	b, err := period.Parse(period.Day, last, loc)
	if err != nil {
		return Range{}, err
	}
	if b.Label < a.Label {
		return Range{}, fmt.Errorf("tokens: %s is before %s — a range runs forwards", last, first)
	}
	return Range{First: a, Last: b}, nil
}

// Days is how many company days the range holds.
//
// Counted on the CALENDAR — the two labels as dates — never by dividing the
// instants: a range across a clock change is a whole number of days that is
// not a whole number of 24-hour spans.
func (r Range) Days() int {
	a, errA := time.Parse(time.DateOnly, r.First.Label)
	b, errB := time.Parse(time.DateOnly, r.Last.Label)
	if errA != nil || errB != nil {
		return 0
	}
	return int(b.Sub(a)/(24*time.Hour)) + 1
}

// Previous is the range of the same number of days ending the day before this
// one begins — the compare-to-previous window.
//
// HERE rather than in the browser because "the previous week" is a subtraction
// a client would do on its own clock: a comparison whose two windows are cut on
// different calendars reports a change nobody made.
func (r Range) Previous() Range {
	n := r.Days()
	return Range{First: r.First.Shift(-n), Last: r.First.Shift(-1)}
}

// Windows is every day of the range, in order.
func (r Range) Windows() []period.Window {
	var out []period.Window
	for w := r.First; w.Period.Valid() && w.Label <= r.Last.Label; w = w.Next() {
		out = append(out, w)
	}
	return out
}

// Since is the first instant of the range; Until the first instant after it.
func (r Range) Since() time.Time { return r.First.Start }

// Until is the exclusive end of the range: the first instant after its last
// day.
func (r Range) Until() time.Time { return r.Last.End }

// Cell is one company day's spend for one seat under one (phase, worker,
// model, provider entry), summed over every node that ran the seat that day by
// the caller or left as one row per node — the fold adds either way.
//
// It is the usage domain's `usage_tokens` row with the seat named as that
// day's record named it. Bucket.Calls is the row's own call count, never one
// per cell.
type Cell struct {
	Day     string
	AgentID string
	Handle  string
	Role    string

	Phase       string
	Worker      string
	Model       string
	ProviderKey string

	Bucket
}

// SeatDay is one seat's ended turns on one company day, on one node: the
// usage domain's `usage_turns` head row, as much of it as a spend answer
// reads.
type SeatDay struct {
	Day     string
	AgentID string
	Handle  string
	Role    string
	Turns   int
	Failed  int
}

// DailyOptions tune one fold of a named window.
type DailyOptions struct {
	// Range is the window the caller read. It labels the rollup; the caller
	// did the filtering.
	Range Range

	// Seat is the handle the caller narrowed to, echoed on the answer.
	Seat string

	// Horizon is stated on the answer.
	Horizon Horizon
}

// FoldDaily folds a named window's company-day rows into the breakdown.
//
// THE SAME [Rollup] [Aggregate] answers the live window with, so a reader
// moving between the two compares like with like — with the three differences
// the source forces, each stated on the answer rather than papered over: there
// is no `by_turn` (a day's row holds no turn), no watermark (no instant per
// call), and every seat's row carries how many of its turns ended and failed
// (which the live window cannot count).
//
// A seat is keyed on its AGENT ID — the identity every node derives alike from
// the org name and the handle — and named by the newest row that named it,
// because the cells arrive in day order and a renamed role's newest name is
// the one a reader recognises.
func FoldDaily(cells []Cell, seats []SeatDay, opts DailyOptions) Rollup {
	h := opts.Horizon
	out := Rollup{
		Since:   stamp(opts.Range.Since()),
		Until:   stamp(opts.Range.Until()),
		From:    opts.Range.First.Label,
		To:      opts.Range.Last.Label,
		Days:    opts.Range.Days(),
		Seat:    opts.Seat,
		Horizon: &h,

		ByPhase:  []PhaseRow{},
		ByModel:  []ModelRow{},
		ByWorker: []WorkerRow{},
		ByAgent:  []AgentRow{},
	}

	byPhase := map[string]*Bucket{}
	byModel := map[string]*Bucket{}
	byWorker := map[string]*Bucket{}
	byAgent := map[string]*AgentRow{}
	providers := providerFold{}

	agentFor := func(id, handle, role string) *AgentRow {
		key := orUnknown(id)
		a := byAgent[key]
		if a == nil {
			zero, none := 0, 0
			a = &AgentRow{AgentID: id, ByPhase: map[string]*Bucket{}, Turns: &zero, Failed: &none}
			byAgent[key] = a
		}
		if handle != "" {
			a.Handle = handle
		}
		if role != "" {
			a.Role = role
		}
		return a
	}

	for _, c := range cells {
		phase := orUnknown(c.Phase)
		model := orUnknown(c.Model)

		out.Totals.merge(c.Bucket)
		bucketFor(byPhase, phase).merge(c.Bucket)
		bucketFor(byModel, model).merge(c.Bucket)
		if c.Phase == PhaseAuxiliary && c.Worker != "" {
			bucketFor(byWorker, c.Worker).merge(c.Bucket)
		}
		providers.add(c.ProviderKey, model, seatName(c.Handle, c.Role, c.AgentID), c.Bucket)

		a := agentFor(c.AgentID, c.Handle, c.Role)
		a.merge(c.Bucket)
		bucketFor(a.ByPhase, phase).merge(c.Bucket)
	}
	for _, s := range seats {
		a := agentFor(s.AgentID, s.Handle, s.Role)
		*a.Turns += s.Turns
		*a.Failed += s.Failed
	}

	for phase, b := range byPhase {
		out.ByPhase = append(out.ByPhase, PhaseRow{Phase: phase, Bucket: *b})
	}
	for model, b := range byModel {
		out.ByModel = append(out.ByModel, ModelRow{Model: model, Bucket: *b})
	}
	for worker, b := range byWorker {
		out.ByWorker = append(out.ByWorker, WorkerRow{Worker: worker, Bucket: *b})
	}
	for _, a := range byAgent {
		if a.Role == "" {
			a.Role = orUnknown(a.Handle)
		}
		out.ByAgent = append(out.ByAgent, *a)
	}
	out.ByProvider = providers.rows()

	byTokensThen(out.ByPhase, func(r PhaseRow) (int, string) { return r.TotalTokens, r.Phase })
	byTokensThen(out.ByModel, func(r ModelRow) (int, string) { return r.TotalTokens, r.Model })
	byTokensThen(out.ByWorker, func(r WorkerRow) (int, string) { return r.TotalTokens, r.Worker })
	byTokensThen(out.ByAgent, func(r AgentRow) (int, string) { return r.TotalTokens, r.AgentID })
	return out
}

// seatName is how a seat is named in a "used by" list and on a seat band: its
// handle, which every surface links by; its role where a row carried no
// handle; its agent id past that; "unknown" when a row carried nothing at all.
func seatName(handle, role, agentID string) string {
	switch {
	case handle != "":
		return handle
	case role != "":
		return role
	}
	return orUnknown(agentID)
}

package tokens

import (
	"cmp"
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

// ErrOutOfRange is a window that starts before what the usage domain holds:
// its first day — or the first day of the window before it, when a comparison
// was asked for — is older than the horizon's floor.
//
// A REFUSAL rather than a truncated answer: the rows before the floor are gone
// on every node, so an answer "floored" to it would put a heading over figures
// covering fewer days than it names — the previous-window chart at 30 and 90
// days read exactly that way, silently empty, while the event log was its
// source.
//
// NOT [ErrWindowLength], which it was until a 152-day window lying wholly
// inside the history was refused as one that "reaches past the spend
// history": the reader was told the wrong thing to change.
var ErrOutOfRange = errors.New("tokens: the window starts before the spend history")

// ErrWindowLength is a window of fewer than one or more than
// [MaxSpendRangeDays] company days, wherever in the history it lies.
var ErrWindowLength = fmt.Errorf("tokens: a spend window is 1 to %d company days", MaxSpendRangeDays)

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
// per cell. AgentID is what the fold keys the seat on; Handle and Role are
// only what it was CALLED that day, read for a seat the chart no longer holds.
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

	// AgentID is the seat the caller narrowed to, by its id, echoed on the
	// answer with its handle now ([Rollup.Seat]).
	AgentID string

	// Seats is the chart by agent id, as [Options.Seats] is: what each
	// seat row is called and links to.
	Seats Seats

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
// the org name and the seat's handle — and named from the org ([DailyOptions.Seats]), or, for a seat the chart no longer holds, by the
// newest day that named it, whatever order the rows arrive in.
func FoldDaily(cells []Cell, seats []SeatDay, opts DailyOptions) Rollup {
	h := opts.Horizon
	out := Rollup{
		Since:   stamp(opts.Range.Since()),
		Until:   stamp(opts.Range.Until()),
		From:    opts.Range.First.Label,
		To:      opts.Range.Last.Label,
		Days:    opts.Range.Days(),
		AgentID: opts.AgentID,
		Seat:    opts.Seats.handle(opts.AgentID),
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
	names := map[string]*label{}
	providers := providerFold{}

	// agentFor is the seat row a day's row is filed under, keyed on its id
	// and named from the chart or the newest day — see [label]. A row that
	// carried no role is named by the handle it carried, the one name it
	// has, rather than "unknown".
	agentFor := func(id, handle, role, day string) (*AgentRow, string) {
		key := seatKey(id, role)
		seat, known := opts.Seats.lookup(id)
		a := byAgent[key]
		if a == nil {
			zero, none := 0, 0
			a = &AgentRow{AgentID: id, Handle: seat.Handle, ByPhase: map[string]*Bucket{},
				Turns: &zero, Failed: &none}
			byAgent[key] = a
		}
		a.Role = labelFor(names, key).fold(cmp.Or(role, handle), day, seat, known)
		return a, key
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
		a, key := agentFor(c.AgentID, c.Handle, c.Role, c.Day)
		providers.add(c.ProviderKey, model, key, c.Bucket)
		a.merge(c.Bucket)
		bucketFor(a.ByPhase, phase).merge(c.Bucket)
	}
	for _, s := range seats {
		a, _ := agentFor(s.AgentID, s.Handle, s.Role, s.Day)
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
		out.ByAgent = append(out.ByAgent, *a)
	}
	out.ByProvider = providers.rows(seatNames(opts.Seats, names))

	byTokensThen(out.ByPhase, func(r PhaseRow) (int, string) { return r.TotalTokens, r.Phase })
	byTokensThen(out.ByModel, func(r ModelRow) (int, string) { return r.TotalTokens, r.Model })
	byTokensThen(out.ByWorker, func(r WorkerRow) (int, string) { return r.TotalTokens, r.Worker })
	// Ties on the name and then the id, as the live rollup's, since two
	// seats may share a name.
	byTokensThen(out.ByAgent, func(r AgentRow) (int, string) { return r.TotalTokens, r.Role + "\x00" + r.AgentID })
	return out
}

package tokens

import (
	"cmp"
	"slices"

	"github.com/crewlet/crewlet/internal/period"
)

// THE TIME AXIS, which the rollup does not have.
//
// [Rollup] answers "what did this window cost, split by phase / model /
// provider / seat / worker". It cannot answer "and when", because every one of
// its rows is a sum over the whole window — which is the one question a cost
// explorer is for: spend is only ever read against a shape, and a flat total
// has none.
//
// It is HERE rather than in the browser for the reason this package exists at
// all: an axis folded client-side would be the fourth copy of an aggregation
// that had three.

// Interval is a bucket's width: a company day, or a company ISO week.
//
// TWO, and neither finer than a day: the source is the usage domain's company
// days, and a bucket finer than its rows would have to be invented. An hourly
// bucket was offered while this read one node's event log; it is gone with
// that source, and "what happened this afternoon" is the turn list's question.
type Interval string

// The intervals, finest first.
const (
	IntervalDay  Interval = "day"
	IntervalWeek Interval = "week"
)

// Intervals is the closed set, in the order a control offers them.
var Intervals = []Interval{IntervalDay, IntervalWeek}

// Valid reports whether i is one this build knows — so an unknown value off
// the wire is a value the caller refuses rather than a silent fallback to
// whichever branch the switch happened to end on.
func (i Interval) Valid() bool { return slices.Contains(Intervals, i) }

// Group names the dimension a series is split on.
//
// The five a company day's spend cell answers for ITSELF, plus the one it
// cannot: GroupUnit is resolved through [SeriesOptions.Seats], because which
// unit a seat sits in is a fact about the org chart and this package has never
// seen one. A cell carries no project and no work item at all — that
// attribution is the tracker's own per-item counters, a different read over a
// different estate.
//
// There is no turn: a day's row holds none. Per-turn spend is the turn list
// sorted by tokens.
type Group string

// The dimensions a breakdown may be grouped by.
const (
	GroupPhase    Group = "phase"
	GroupModel    Group = "model"
	GroupProvider Group = "provider"
	GroupSeat     Group = "seat"
	GroupUnit     Group = "unit"
	GroupWorker   Group = "worker"
)

// Groups is the closed set, in the order a control offers them.
var Groups = []Group{GroupPhase, GroupModel, GroupProvider, GroupSeat, GroupUnit, GroupWorker}

// Valid reports whether g is one this build knows.
func (g Group) Valid() bool { return slices.Contains(Groups, g) }

// Band is one of the four bands a phase breakdown is drawn in.
//
// FOUR, folded ONCE, here: the engine has seven phase values (and a retired
// eighth still in the history), a chart has four data hues, and a phase band a
// screen folded itself would be the fold that disagreed with the next screen's.
type Band string

// The four bands, in the order a chart stacks them.
const (
	// BandExecute is the seat's own turn doing its work: the executor, and
	// a detached coding run it launched, which is the same work done
	// somewhere else.
	BandExecute Band = "execute"
	// BandReview is the reviewer's pass over it.
	BandReview Band = "review"
	// BandWorkers is the delegated workers the executor handed tasks to.
	BandWorkers Band = "workers"
	// BandAuxiliary is everything that is not the turn's own work: the
	// learning workers, the round-cap judge and the first-turn onboarding.
	BandAuxiliary Band = "auxiliary"
)

// Bands is the closed set, in stacking order. `contract/spend.ts` `BANDS`
// holds the dashboard's copy against it.
var Bands = []Band{BandExecute, BandReview, BandWorkers, BandAuxiliary}

// PhaseBand is the band a phase value is drawn in.
//
// The retired `plan` phase folds into Execute: it was a leg of the turn itself
// — the frame that decides is the frame that acts now — and a pre-redesign
// node's records still carry it inside the usage horizon. Any other value this
// build does not know (a newer peer's phase, or none at all) is AUXILIARY,
// never dropped: it is spend, and "not the turn's own recognised work" is the
// one honest thing this build can say about it.
func PhaseBand(phase string) Band {
	switch phase {
	case "execute", "sandbox", "plan":
		return BandExecute
	case "review":
		return BandReview
	case "subagent":
		return BandWorkers
	}
	return BandAuxiliary
}

// key is the group a cell falls in, and whether it falls in one at all.
//
// A THREE-VALUED ANSWER flattened to two, and the false is load-bearing:
// GroupWorker leaves out every cell that is not an auxiliary worker's, and a
// cell with no worker is not "the unknown worker" — it is a phase that has
// nothing to do with this grouping and belongs in no band of the chart. Every
// other dimension is present on every cell, so the empty ones become "unknown"
// the way the rollup's do.
//
// A SEAT AND A UNIT ARE BANDED BY IDENTITY — the seat's agent id, the unit's
// key — and never by name, because two seats or two units may share a name and
// a band keyed on it was both of them, while one seat renamed inside the window
// was two bands. [GroupRow.Label] is what a band is called.
func (g Group) key(c Cell, seats Seats) (string, bool) {
	switch g {
	case GroupPhase:
		return string(PhaseBand(c.Phase)), true
	case GroupModel:
		return orUnknown(c.Model), true
	case GroupProvider:
		return orUnknown(c.ProviderKey), true
	case GroupSeat:
		return seatKey(c.AgentID, c.Role), true
	case GroupUnit:
		// A seat at the root of the chart is in no unit, and that is a
		// real placement rather than a missing one — so it groups under
		// its own band rather than under "unknown", which would pool
		// every root seat with every seat the caller's chart happened not
		// to carry.
		if seat, ok := seats.lookup(c.AgentID); ok && seat.UnitKey != "" {
			return seat.UnitKey, true
		}
		return unattachedUnit, true
	case GroupWorker:
		// Keyed on the PAIR: Worker is set only on an auxiliary phase, so
		// a bare non-empty check would fold a stray value on some other
		// phase in.
		if c.Phase == PhaseAuxiliary && c.Worker != "" {
			return c.Worker, true
		}
		return "", false
	}
	return "", false
}

// unattachedUnit names the band holding every seat that sits at the root of
// the chart rather than inside a unit.
//
// Not "unknown", which this package already spends on a dimension the row
// failed to carry: a root seat's unit is not missing, there is none. And it is
// a key no unit can hold: a unit's key on the chart is an address, which
// `chart.NormalizeKey` folds to carry no whitespace, and this has a space in
// it.
const unattachedUnit = "no unit"

// DefaultSeriesGroups is how many bands a stacked chart carries before the
// rest fold into the residual.
//
// FOUR, because that is how many data hues the design system has, plus the
// residual's neutral — and it is exactly the phase breakdown's band count, so
// the default grouping never folds. A fifth band would be drawn in a colour
// the palette does not define.
const DefaultSeriesGroups = 4

// MaxSeriesGroups bounds what a caller may ask for. Past this the chart is a
// wall of bands and the answer is a table, which the rollup already is.
const MaxSeriesGroups = 20

// GroupRow is one band: its share of the whole window, and its identity.
type GroupRow struct {
	// Group is the band's key in each point's map. Empty on — and only on
	// — the residual row, which is what makes the fold unambiguous without
	// reserving a name a real phase or model might carry.
	//
	// By seat it is the seat's agent id and by unit the unit's key — see
	// [Group.key] — which no reader should print: Label is the words.
	Group string `json:"group"`

	// Label is what the band is called when its key is an identity rather
	// than a name: a seat's name, a unit's name. Absent where the key is
	// already the words (a phase, a model, a provider entry, a worker, the
	// unattached band).
	Label string `json:"label,omitempty"`

	// Handle is the seat's handle when the grouping is by seat and the
	// chart holds the seat, so a row can link to the page it names. Absent
	// rather than guessed for a seat the chart no longer holds, which is
	// [Options.Seats]' own rule.
	Handle string `json:"handle,omitempty"`

	// Seats counts the distinct seats — by agent id — that spent in a UNIT
	// band over the window, so a legend can say "Platform · 4 seats" and a
	// reader can tell a busy seat from a busy team.
	Seats int `json:"seats,omitempty"`

	// Other marks the residual row, and Folded counts the distinct groups
	// it stands for — so a legend can say "other (12)" rather than
	// presenting a band whose size no reader can account for.
	Other  bool `json:"other"`
	Folded int  `json:"folded"`

	Bucket
}

// SeriesPoint is one bucket of the series.
type SeriesPoint struct {
	// At is the bucket's START — the first instant of its company day or
	// ISO week, RFC3339 in UTC. A week bucket's start can be before the
	// window's (see Days).
	At string `json:"at"`

	// Window is the bucket's label on the company calendar: `2026-09-23`
	// for a day, `2026-W39` for a week — what a reader's axis says.
	Window string `json:"window"`

	// Days is how many of the window's days fall in this bucket: always 1
	// for a day, and 7 for a week except the first and last, which the
	// window can cut short. A chart comparing a partial week with a whole
	// one has to be able to say so.
	Days int `json:"days"`

	// Bucket is the whole bucket, every band included, so a reader of the
	// total does not have to re-sum the map and reach a different number
	// from the one the residual was computed against.
	Bucket

	// Groups is this bucket's spend per band, keyed as [GroupRow.Group].
	// Bands with nothing in this bucket are ABSENT rather than zero.
	Groups map[string]Bucket `json:"groups"`

	// Other is the residual — every group past the cap — for this bucket.
	Residual Bucket `json:"other"`
}

// Series is the whole time axis.
type Series struct {
	// Group and Interval echo what this was built with, so a rendered
	// chart's axis and legend describe the data rather than the control's
	// current position — which drift the moment a request is in flight.
	Group    Group    `json:"group"`
	Interval Interval `json:"bucket"`

	// Since and Until are the window, RFC3339 in UTC: the first instant of
	// its first company day and the first instant after its last. From, To
	// and Days name the same window by its days.
	Since string `json:"since"`
	Until string `json:"until"`
	From  string `json:"from"`
	To    string `json:"to"`
	Days  int    `json:"days"`

	// AgentID is the seat this series was narrowed to, by its id, and Seat
	// that seat's handle now — see [Rollup.AgentID] and [Rollup.Seat].
	// Both absent for the whole company.
	AgentID string `json:"agent_id,omitempty"`
	Seat    string `json:"seat,omitempty"`

	// Horizon is how far back a window can reach. See [Horizon].
	Horizon Horizon `json:"horizon"`

	// Points is every bucket in the window, INCLUDING the empty ones. A
	// series with holes in it is a chart the client has to fill, which is
	// the bucketing this package exists to have written once.
	Points []SeriesPoint `json:"series"`

	// ByGroup is the legend and the grid: each band's total over the whole
	// window, biggest first, with the residual last when there is one —
	// EXCEPT by phase, where ALL FOUR bands are listed, a band nothing spent
	// in at zero, in stacking order ([Bands]) every time, so a band's colour
	// and place never move with the data.
	ByGroup []GroupRow `json:"by_group"`

	// Totals is the window's whole spend — every cell inside it, including
	// ones this grouping places in no band at all. It is therefore NOT the
	// sum of ByGroup, and the difference is the answer to "why does grouping
	// by worker show less than the rollup": most phases are not a worker's.
	Totals Bucket `json:"totals"`

	// Grouped is what the bands do cover, so the gap above is a number
	// rather than an inference a reader has to make by subtracting.
	Grouped Bucket `json:"grouped"`
}

// SeriesOptions tune one bucketing.
type SeriesOptions struct {
	// Group is the dimension. An invalid one is refused by the caller, not
	// defaulted here — a chart legended "by model" over phase bands is
	// worse than an error.
	Group Group

	// Interval is the bucket width. Anything but IntervalWeek is a day.
	Interval Interval

	// Range is the window, on the company's clock. Cells for a day outside
	// it are not counted.
	Range Range

	// Groups caps the bands. Zero takes DefaultSeriesGroups. A phase
	// grouping is never capped below its four bands.
	Groups int

	// Seats is the chart by agent id, as [Options.Seats] is: a seat band's
	// name and handle, and the unit a GroupUnit band files the seat under.
	// A seat it does not carry lands in the unattached band rather than an
	// invented one.
	Seats Seats

	// AgentID is the seat the caller narrowed to, echoed on the answer with
	// its handle now; Horizon is echoed as it is.
	AgentID string
	Horizon Horizon
}

// BucketDaily folds a named window's company-day cells into a time series.
//
// Order-independent: every bucket is a sum. A cell for a day outside the range
// is left out of everything, totals included — the caller read the range, so
// such a cell is a caller's mistake, and counting it would put spend under a
// heading that does not cover it.
func BucketDaily(cells []Cell, opts SeriesOptions) Series {
	interval := opts.Interval
	if interval != IntervalWeek {
		interval = IntervalDay
	}
	limit := opts.Groups
	switch {
	case limit <= 0:
		limit = DefaultSeriesGroups
	case limit > MaxSeriesGroups:
		limit = MaxSeriesGroups
	}
	if opts.Group == GroupPhase {
		limit = max(limit, len(Bands))
	}

	out := Series{
		Group: opts.Group, Interval: interval,
		Since:   stamp(opts.Range.Since()),
		Until:   stamp(opts.Range.Until()),
		From:    opts.Range.First.Label,
		To:      opts.Range.Last.Label,
		Days:    opts.Range.Days(),
		AgentID: opts.AgentID,
		Seat:    opts.Seats.handle(opts.AgentID),
		Horizon: opts.Horizon,
		Points:  []SeriesPoint{},
		// Never nil: the client does `.by_group.length`, and an empty
		// window marshalled as `null` throws in the browser rather than
		// drawing an empty chart.
		ByGroup: []GroupRow{},
	}

	// THE AXIS FIRST, from the calendar rather than from the cells: every
	// day of the window has a bucket whether or not anything spent in it.
	// A week bucket is the ISO week the day falls in on the company clock,
	// so a window's days map onto weeks by label and never by dividing
	// instants — a week holding a clock change is 167 or 169 hours long.
	index := map[string]int{}
	for _, day := range opts.Range.Windows() {
		bucket := day
		if interval == IntervalWeek {
			bucket = period.At(period.Week, day.Start, day.Start.Location())
		}
		last := len(out.Points) - 1
		if last < 0 || out.Points[last].Window != bucket.Label {
			out.Points = append(out.Points, SeriesPoint{
				At:     stamp(bucket.Start),
				Window: bucket.Label,
				// Never nil, for the reason ByGroup is not: the client
				// indexes it per band per bucket.
				Groups: map[string]Bucket{},
			})
			last++
		}
		out.Points[last].Days++
		index[day.Label] = last
	}

	// Two passes. The first decides WHICH bands survive the cap, over the
	// whole window; the second fills the buckets. One pass cannot do it:
	// which groups are the biggest is a property of the window, and a
	// bucket-at-a-time decision would put a band in the chart for the days
	// it happened to lead and in the residual for the rest.
	total := map[string]*Bucket{}
	if opts.Group == GroupPhase {
		// EVERY BAND, SPENT OR NOT. By phase the legend is the closed set
		// in stacking order, so a window with no delegated worker still
		// answers Workers at zero: a band that came and went with the data
		// is a legend that disagrees with the sentence under it ("review,
		// workers and auxiliary calls are the overhead"), and a reader who
		// cannot find Workers cannot tell "none" from "not measured".
		for _, band := range Bands {
			bucketFor(total, string(band))
		}
	}
	seats := map[string]map[string]bool{}
	named := map[string]*label{}
	handles := map[string]string{}
	for _, c := range cells {
		if _, inside := index[c.Day]; !inside {
			continue
		}
		out.Totals.merge(c.Bucket)
		key, ok := opts.Group.key(c, opts.Seats)
		if !ok {
			continue
		}
		out.Grouped.merge(c.Bucket)
		bucketFor(total, key).merge(c.Bucket)
		seat, known := opts.Seats.lookup(c.AgentID)
		switch opts.Group {
		case GroupSeat:
			labelFor(named, key).fold(cmp.Or(c.Role, c.Handle), c.Day, seat, known)
			handles[key] = seat.Handle
		case GroupUnit:
			// A unit the chart names is labelled by that name; one it
			// does not (and the unattached band) is printed by its key,
			// which is then the only words there are.
			if known && seat.UnitKey == key && seat.UnitName != "" {
				labelFor(named, key).name = seat.UnitName
			}
			if seats[key] == nil {
				seats[key] = map[string]bool{}
			}
			seats[key][seatKey(c.AgentID, c.Role)] = true
		}
	}

	ranked := make([]GroupRow, 0, len(total))
	for key, b := range total {
		row := GroupRow{Group: key, Handle: handles[key], Seats: len(seats[key]), Bucket: *b}
		if l := named[key]; l != nil {
			row.Label = orUnknown(l.name)
		}
		ranked = append(ranked, row)
	}
	if opts.Group == GroupPhase {
		// STACKING ORDER, not size: the four bands are a fixed legend, and a
		// legend whose order follows the data moves Execute's place — and
		// with a positional palette its colour — whenever review had a big
		// week.
		slices.SortFunc(ranked, func(a, b GroupRow) int {
			return slices.Index(Bands, Band(a.Group)) - slices.Index(Bands, Band(b.Group))
		})
	} else {
		// Ties on the words a reader sees and then the key, since two
		// bands may carry the same label.
		byTokensThen(ranked, func(r GroupRow) (int, string) { return r.TotalTokens, r.Label + "\x00" + r.Group })
	}

	kept := map[string]bool{}
	if len(ranked) > limit {
		residual := GroupRow{Other: true, Folded: len(ranked) - limit}
		for _, r := range ranked[limit:] {
			residual.merge(r.Bucket)
		}
		ranked = append(ranked[:limit:limit], residual)
	}
	for _, r := range ranked {
		if !r.Other {
			kept[r.Group] = true
		}
	}
	out.ByGroup = ranked

	for _, c := range cells {
		i, inside := index[c.Day]
		if !inside {
			continue
		}
		point := &out.Points[i]
		point.merge(c.Bucket)
		key, grouped := opts.Group.key(c, opts.Seats)
		switch {
		case !grouped:
		case kept[key]:
			b := point.Groups[key]
			b.merge(c.Bucket)
			point.Groups[key] = b
		default:
			point.Residual.merge(c.Bucket)
		}
	}
	return out
}

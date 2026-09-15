package tokens

import (
	"slices"
	"time"
)

// THE TIME AXIS, which the rollup does not have.
//
// [Rollup] answers "what did this window cost, split by phase / model / seat /
// worker / turn". It cannot answer "and when", because every one of its rows
// is a sum over the whole window — which is the one question a cost explorer
// is for: spend is only ever read against a shape, and a flat total has none.
//
// It is HERE rather than in the browser for the reason this package exists at
// all. The client already holds the records for the live window and could
// bucket them; the store's window it never holds, so a browser-side axis would
// be correct for one window and absent for every other, and it would be the
// fourth copy of an aggregation that had three.

// Interval is a bucket's width.
//
// Two, deliberately, and neither is configurable as a duration: a chart with
// an arbitrary bucket width has an x axis nobody can label, and the two that
// matter are "today, by hour" and "this month, by day". A minute bucket over a
// week is 10,080 points nobody can read.
type Interval string

const (
	IntervalHour Interval = "hour"
	IntervalDay  Interval = "day"
)

// Intervals is the closed set, in the order a control offers them.
var Intervals = []Interval{IntervalHour, IntervalDay}

// Valid reports whether i is one this build knows — so an unknown value off
// the wire is a value the caller refuses rather than a silent fallback to
// whichever branch the switch happened to end on.
func (i Interval) Valid() bool { return slices.Contains(Intervals, i) }

// Step is how far one bucket reaches.
func (i Interval) Step() time.Duration {
	if i == IntervalDay {
		return 24 * time.Hour
	}
	return time.Hour
}

// Start is the beginning of the bucket holding t, in UTC.
//
// Built from the calendar fields rather than by [time.Time.Truncate], which
// truncates the absolute duration since year 1 and therefore lands on a UTC
// boundary only by the accident that year 1 began at midnight. Naming the
// fields says what a day means here, which is "a UTC day" — an offset this
// engine keeps everything else in, and the one a reader in any zone can
// convert from.
func (i Interval) Start(t time.Time) time.Time {
	u := t.UTC()
	if i == IntervalDay {
		return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
	}
	return time.Date(u.Year(), u.Month(), u.Day(), u.Hour(), 0, 0, 0, time.UTC)
}

// Group names the dimension a series is split on.
//
// The five a spend record can answer for ITSELF, plus the one it cannot:
// GroupUnit is resolved through [SeriesOptions.Units], because which unit a
// seat sits in is a fact about the org chart and this package is a leaf that
// has never seen one. A record carries no project and no work item at all —
// that attribution is the tracker's own per-item counters, a different read
// over a different estate, and manufacturing it here would mean inventing a
// join the event never recorded.
type Group string

const (
	GroupPhase  Group = "phase"
	GroupModel  Group = "model"
	GroupSeat   Group = "seat"
	GroupUnit   Group = "unit"
	GroupWorker Group = "worker"
	GroupTurn   Group = "turn"
)

// Groups is the closed set, in the order a control offers them.
var Groups = []Group{GroupPhase, GroupModel, GroupSeat, GroupUnit, GroupWorker, GroupTurn}

// Valid reports whether g is one this build knows.
func (g Group) Valid() bool { return slices.Contains(Groups, g) }

// key is the group a record falls in, and whether it falls in one at all.
//
// A THREE-VALUED ANSWER flattened to two, and the false is load-bearing:
// GroupWorker leaves out every record that is not an auxiliary worker's, and a
// record with no worker is not "the unknown worker" — it is a phase that has
// nothing to do with this grouping and belongs in no band of the chart. Every
// other dimension is present on every record, so the empty ones become
// "unknown" the way the rollup's do.
func (g Group) key(r Record, units map[string]string) (string, bool) {
	switch g {
	case GroupPhase:
		return orUnknown(r.Phase), true
	case GroupModel:
		return orUnknown(r.Model), true
	case GroupSeat:
		return orUnknown(r.AgentRole), true
	case GroupUnit:
		// A seat at the root of the chart is in no unit, and that is a
		// real placement rather than a missing one — so it groups under
		// its own name rather than under "unknown", which would pool
		// every root seat with every seat whose role the caller's map
		// happened not to carry.
		if unit := units[r.AgentRole]; unit != "" {
			return unit, true
		}
		return unattachedUnit, true
	case GroupWorker:
		// Keyed on the PAIR for the reason the rollup's worker bucket is:
		// Worker is set only on an auxiliary phase, so a bare non-empty
		// check would fold a stray value on some other phase in.
		if r.Phase == PhaseAuxiliary && r.Worker != "" {
			return r.Worker, true
		}
		return "", false
	case GroupTurn:
		// A phase with no turn is real spend that cannot be attributed to
		// one — the same judgement [Aggregate] makes, and for the same
		// reason: inventing a key would draw one band per phase.
		if r.TurnID == "" {
			return "", false
		}
		return r.TurnID, true
	}
	return "", false
}

// unattachedUnit names the band holding every seat that sits at the root of
// the chart rather than inside a unit.
//
// Not "unknown", which this package already spends on a dimension the event
// failed to carry: a root seat's unit is not missing, there is none.
const unattachedUnit = "no unit"

// DefaultSeriesGroups is how many bands a stacked chart carries before the
// rest fold into the residual.
//
// Five, because that is how many distinguishable hues the design system has
// (`VIZ` plus the residual): a sixth band would be drawn in a colour the
// palette does not define, and a legend of forty turn ids is not a legend.
const DefaultSeriesGroups = 5

// MaxSeriesGroups bounds what a caller may ask for. Past this the chart is a
// wall of bands and the answer is a table, which the rollup already is.
const MaxSeriesGroups = 20

// MaxSeriesPoints bounds one answer's buckets.
//
// Hourly over the store's own thirty-day ceiling is 720, so nothing reachable
// through the API hits this. It binds on a caller that names two instants
// directly, and what it does then is move `since` FORWARD and report the
// window it actually covered — the same discipline the rollup applies to a
// clamped day count. Dropping the oldest buckets silently would put a month's
// heading over a week of bars.
const MaxSeriesPoints = 1000

// GroupRow is one band: its share of the whole window, and its identity.
type GroupRow struct {
	// Group is the band's key in each point's map. Empty on — and only on
	// — the residual row, which is what makes the fold unambiguous without
	// reserving a name a real phase or model might carry.
	Group string `json:"group"`

	// Handle is the seat's handle when the grouping is by seat, so a row
	// can link to the page it names. Absent leaves it blank rather than
	// guessing, which is [Options.Handles]' own rule.
	Handle string `json:"handle,omitempty"`

	// Other marks the residual row, and Folded counts the distinct groups
	// it stands for — so a legend can say "other (12)" rather than
	// presenting a band whose size no reader can account for.
	Other  bool `json:"other"`
	Folded int  `json:"folded"`

	Bucket
}

// SeriesPoint is one bucket of the series.
type SeriesPoint struct {
	// At is the bucket's START, RFC3339 in UTC — never its middle and never
	// its end. A chart draws a bar from here to here+Step.
	At string `json:"at"`

	// Bucket is the whole bucket, every band included, so a reader of the
	// total does not have to re-sum the map and reach a different number
	// from the one the residual was computed against.
	Bucket

	// Groups is this bucket's spend per band, keyed as [GroupRow.Group].
	// Bands with nothing in this bucket are ABSENT rather than zero: over
	// 720 buckets a zero per band per bucket is most of the payload.
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

	// Since and Until are the window COVERED, RFC3339 in UTC. Since is the
	// first bucket's start and Until the end of the last one, and either
	// may differ from what was asked: see [MaxSeriesPoints].
	Since string `json:"since"`
	Until string `json:"until"`

	// Points is every bucket in the window, INCLUDING the empty ones. A
	// series with holes in it is a chart the client has to fill, which is
	// the bucketing this package exists to have written once.
	Points []SeriesPoint `json:"series"`

	// ByGroup is the legend and the grid: each band's total over the whole
	// window, biggest first, with the residual last when there is one.
	ByGroup []GroupRow `json:"by_group"`

	// Totals is the window's whole spend — every record that fell inside
	// it, including ones this grouping places in no band at all. It is
	// therefore NOT the sum of ByGroup, and the difference is the answer to
	// "why does grouping by worker show less than the rollup": most phases
	// are not a worker's.
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

	// Interval is the bucket width. Anything but IntervalDay is an hour.
	Interval Interval

	// Since and Until bound the window. Until at its zero value is taken
	// from the newest record; Since at its zero value from the oldest, so a
	// caller that has already filtered its records need not restate the
	// window it filtered on.
	Since time.Time
	Until time.Time

	// Groups caps the bands. Zero takes DefaultSeriesGroups.
	Groups int

	// Handles maps a role name to its handle, as [Options.Handles] does.
	Handles map[string]string

	// Units maps a role name to the unit holding it, for GroupUnit. Absent
	// puts every seat in the unattached band rather than inventing one.
	Units map[string]string
}

// Bucketed folds records into a time series.
//
// Order-independent, like [Aggregate] and for the same reason: the live window
// arrives append-ordered and the store's newest-first, and a bucketing that
// depended on which would make the two disagree at exactly the moment a reader
// changed the window.
//
// A record whose timestamp does not parse is DROPPED rather than pooled into
// some bucket, and it is the one thing here that silently loses spend — the
// alternative is a bar at an instant the record does not claim. It cannot
// happen from either producer: both write RFC3339Nano.
func Bucketed(records []Record, opts SeriesOptions) Series {
	interval := opts.Interval
	if interval != IntervalDay {
		interval = IntervalHour
	}
	limit := opts.Groups
	switch {
	case limit <= 0:
		limit = DefaultSeriesGroups
	case limit > MaxSeriesGroups:
		limit = MaxSeriesGroups
	}

	type dated struct {
		Record
		at time.Time
	}
	inside := make([]dated, 0, len(records))
	var oldest, newest time.Time
	for _, r := range records {
		at, err := time.Parse(time.RFC3339Nano, r.Timestamp)
		if err != nil {
			continue
		}
		at = at.UTC()
		if !opts.Since.IsZero() && at.Before(opts.Since) {
			continue
		}
		// EXCLUSIVE at the top, so two adjacent windows asked for
		// back-to-back — which is what compare-to-previous does — neither
		// double-count the instant they share nor lose it.
		if !opts.Until.IsZero() && !at.Before(opts.Until) {
			continue
		}
		if oldest.IsZero() || at.Before(oldest) {
			oldest = at
		}
		if at.After(newest) {
			newest = at
		}
		inside = append(inside, dated{Record: r, at: at})
	}

	step := interval.Step()
	since, until := opts.Since, opts.Until
	if since.IsZero() {
		since = oldest
	}
	if until.IsZero() {
		// The END of the newest record's bucket, so that record is inside
		// the window rather than on its exclusive edge.
		until = interval.Start(newest).Add(step)
	}
	if since.IsZero() && newest.IsZero() {
		// NO WINDOW AT ALL: the caller named none and there is no record
		// to infer one from. The honest answer is an axis with no extent,
		// not one bucket at the beginning of the calendar — which is what
		// truncating the zero time produces, and which renders as a chart
		// of the year 1.
		return Series{
			Group: opts.Group, Interval: interval,
			Points: []SeriesPoint{}, ByGroup: []GroupRow{},
		}
	}
	first := interval.Start(since)
	if !until.After(first) {
		// An empty or inverted window still has a shape: one bucket,
		// nothing in it. Answering with no points at all would render as
		// a chart that failed to load.
		until = first.Add(step)
	}
	// Rounded UP, so a window that ends mid-bucket still draws the bucket it
	// ends in: a request for "the last 90 minutes" covers two hours of axis
	// rather than losing the half hour past the first boundary.
	buckets := int((until.Sub(first) + step - 1) / step)
	if buckets > MaxSeriesPoints {
		// Forward, keeping the NEWEST buckets: a cost explorer is read
		// from the right-hand edge, and the window is reported as what
		// this covers rather than as what was asked.
		//
		// Anchored on the bucket holding the window's LAST instant rather
		// than on `until` itself, so a window ending mid-bucket keeps the
		// partial bucket it ends in — subtracting from `until` and
		// re-truncating drops it.
		buckets = MaxSeriesPoints
		first = interval.Start(until.Add(-time.Nanosecond)).
			Add(-time.Duration(buckets-1) * step)
	}

	out := Series{
		Group: opts.Group, Interval: interval,
		Since:  first.Format(time.RFC3339),
		Until:  first.Add(time.Duration(buckets) * step).Format(time.RFC3339),
		Points: make([]SeriesPoint, 0, buckets),
		// Never nil: the client does `.by_group.length`, and an empty
		// window marshalled as `null` throws in the browser rather than
		// drawing an empty chart.
		ByGroup: []GroupRow{},
	}

	// Two passes. The first decides WHICH bands survive the cap, over the
	// whole window; the second fills the buckets. One pass cannot do it:
	// which groups are the biggest is a property of the window, and a
	// bucket-at-a-time decision would put a band in the chart for the hours
	// it happened to lead and in the residual for the rest.
	total := map[string]*Bucket{}
	for _, d := range inside {
		out.Totals.add(d.Record)
		key, ok := opts.Group.key(d.Record, opts.Units)
		if !ok {
			continue
		}
		out.Grouped.add(d.Record)
		bucketFor(total, key).add(d.Record)
	}

	ranked := make([]GroupRow, 0, len(total))
	for key, b := range total {
		row := GroupRow{Group: key, Bucket: *b}
		if opts.Group == GroupSeat {
			row.Handle = opts.Handles[key]
		}
		ranked = append(ranked, row)
	}
	byTokensThen(ranked, func(r GroupRow) (int, string) { return r.TotalTokens, r.Group })

	kept := map[string]bool{}
	if len(ranked) > limit {
		residual := GroupRow{Other: true, Folded: len(ranked) - limit}
		for _, r := range ranked[limit:] {
			residual.InputTokens += r.InputTokens
			residual.OutputTokens += r.OutputTokens
			residual.TotalTokens += r.TotalTokens
			residual.Calls += r.Calls
			residual.CostUSD += r.CostUSD
			residual.PricedCalls += r.PricedCalls
		}
		ranked = append(ranked[:limit:limit], residual)
	}
	for _, r := range ranked {
		if !r.Other {
			kept[r.Group] = true
		}
	}
	out.ByGroup = ranked

	for i := range buckets {
		at := first.Add(time.Duration(i) * step)
		out.Points = append(out.Points, SeriesPoint{
			At: at.Format(time.RFC3339),
			// Never nil, for the reason ByGroup is not: the client
			// indexes it per band per bucket.
			Groups: map[string]Bucket{},
		})
	}
	for _, d := range inside {
		// ARITHMETIC rather than a map keyed on the instant: two
		// time.Time values comparing equal depends on their Location
		// POINTER as well as their instant, which is a property of how
		// each was constructed. The offset is exact because both sides
		// are bucket starts.
		i := int(interval.Start(d.at).Sub(first) / step)
		if i < 0 || i >= buckets {
			// Outside the clamped window — see MaxSeriesPoints. Its
			// spend is still in Totals, which is what makes the gap
			// between the axis and the total visible.
			continue
		}
		point := &out.Points[i]
		point.Bucket.add(d.Record)
		key, grouped := opts.Group.key(d.Record, opts.Units)
		switch {
		case !grouped:
		case kept[key]:
			b := point.Groups[key]
			b.add(d.Record)
			point.Groups[key] = b
		default:
			point.Residual.add(d.Record)
		}
	}
	return out
}

// PreviousWindow is the window immediately before [since, until), for the
// compare-to-previous overlay.
//
// HERE rather than in the browser because "the previous week" is a subtraction
// a client would do in local time and this engine buckets in UTC: a comparison
// whose two windows are a different number of hours long is the chart lying
// about a change nobody made. Returned as instants the caller passes straight
// back in, so the two requests are the same shape.
func PreviousWindow(since, until time.Time) (time.Time, time.Time) {
	span := until.Sub(since)
	if span <= 0 {
		return since, until
	}
	return since.Add(-span).UTC(), since.UTC()
}

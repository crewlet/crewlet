package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The event log's own time axis.
//
// A COUNT PER BUCKET rather than a fold over whatever the browser is holding,
// for the reason internal/tokens gives for the same shape one screen over: the
// client has at most a page of rows and the store's window it never holds, so
// an axis folded there would be right for one window and absent for every
// other — and a bar counting rows the list below it would not show is worse
// than no bar. Both halves compile their filters through
// [ListQuery.predicate], so they cannot answer about two different sets.

// EventBucket is a bar's width on that axis.
//
// THREE, and a closed set rather than a duration, for the reason
// [tokens.Interval] is one too: a chart with an arbitrary bucket width has an x
// axis nobody can label. A log needs the minute and the hour the spend series
// does not — "what just happened" is the commonest question asked of an event
// log, and an hour is the whole of the answer's window — while the spend
// series is read from company days, which hold nothing finer.
type EventBucket string

// The buckets, finest first.
const (
	BucketMinute EventBucket = "minute"
	BucketHour   EventBucket = "hour"
	BucketDay    EventBucket = "day"
)

// EventBuckets is the closed set, finest first.
var EventBuckets = []EventBucket{BucketMinute, BucketHour, BucketDay}

// Valid reports whether b is one this build knows, so an unknown value off the
// wire is a value the caller refuses rather than a silent fallback to whichever
// branch the switch happened to end on.
func (b EventBucket) Valid() bool {
	for _, known := range EventBuckets {
		if b == known {
			return true
		}
	}
	return false
}

// Step is how far one bucket reaches.
func (b EventBucket) Step() time.Duration {
	switch b {
	case BucketDay:
		return 24 * time.Hour
	case BucketHour:
		return time.Hour
	default:
		return time.Minute
	}
}

// MaxHistogramBuckets bounds one answer.
//
// A minute bucket over the log's whole 30-day retention is 43,200 bars, which
// is neither drawable nor a question anybody asked. The cap is a REFUSAL
// rather than a truncation: dropping the oldest bars silently would put a
// month's heading over a day of them, and coarsening the bucket would answer a
// different question from the one the axis is labelled with. 1,500 is a day of
// minutes with room to spare, and two months of hours.
//
// It counts the bars ONE NODE CUTS ([HistogramQuery.Window]) — a window the
// history clips included its partial first bar, which the asker drops after
// summing — because that is the number every build refuses at, and a cap one
// build applied to a different count would refuse an answer its peers give.
const MaxHistogramBuckets = 1500

// ErrHistogramSpan is returned for a window that would exceed the cap. It names
// the bucket and the window, because the caller's fix is to choose one or the
// other.
var ErrHistogramSpan = errors.New("store: histogram window is too many buckets")

// ErrHistogramBucket is returned for a bucket width this build does not know,
// naming what is accepted — never defaulted, because an axis labelled by one
// width over another's bars is worse than an error.
var ErrHistogramBucket = errors.New("store: unknown histogram bucket")

// ErrHistogramRelated is returned for a histogram asked to filter by a related
// agent.
//
// REFUSED RATHER THAN APPROXIMATED. That filter over-fetches and post-filters,
// pulling in every event sharing a trace with a direct match — see
// [ListQuery.RelatedAgent] — so a count over the predicate alone is a smaller
// set than the list beside it shows. A bar that disagrees with its own rows is
// the one thing this axis exists not to be.
var ErrHistogramRelated = errors.New("store: a histogram cannot filter by related agent")

// HistogramQuery asks for counts per bucket over a window.
//
// The filters are [ListQuery]'s, verbatim and by embedding rather than by
// copy, so a filter added to one is a filter the other already has. `Limit` and
// `Before` are meaningless here and ignored: a page size is about rows and a
// cursor is about where a page resumes, and a histogram has neither.
//
// The instant the window is cut against — and the floor under it — is the
// embedded [ListQuery.At], so the bars and the listing beside them are floored
// by one field rather than by two that could disagree.
type HistogramQuery struct {
	ListQuery

	// Bucket is the bar width. An invalid one is refused naming what is
	// accepted, never defaulted — an axis labelled by one width over
	// another's bars is worse than an error.
	Bucket EventBucket
}

// EventBar is one bucket of the axis.
type EventBar struct {
	// At is the bucket's START, RFC3339 in UTC — never its middle and
	// never its end, so a reader comparing this to a row's timestamp is
	// comparing the same kind of thing.
	At string `json:"at"`

	// Count is how many events fell in it. Zero is a real value and every
	// bucket is present: a quiet hour is a gap of full height rather than
	// a bar the chart squeezed out.
	Count int `json:"count"`

	// Failed is how many of Count reported a failure — by the one rule the
	// turn list and every read of a row apply ([types.Failed]: the payload's
	// own flag, or a type that IS a failure). A SPLIT of Count, never an
	// addition to it, so a bar draws its failed share inside its height.
	//
	// COUNTED HERE rather than folded by a client, for the reason the axis
	// itself is: a browser holds a page of rows and the window it never
	// holds, so a failed share folded there is right for one page and
	// absent for every other.
	Failed int `json:"failed"`
}

// EventHistogram is the whole axis.
type EventHistogram struct {
	// Bucket, Since and Until describe what this covers, so a reader
	// looking at a bar knows what it is a bar of. Until is EXCLUSIVE,
	// matching every other half-open window in this engine, and it is
	// AHEAD OF NOW whenever the bucket in progress is one of the bars —
	// which is every ask with no top edge. The last bar is a whole one
	// like the rest and fills as the bucket does; an edge at `now` would
	// be a bar whose height meant something different from its
	// neighbours'.
	//
	// ONE NODE'S PART, as [EventLog.Histogram] answers it, begins at the
	// bucket the history floor cuts when the floor clips the window, and
	// that first bar counts only what lies above the floor — see
	// [HistogramQuery.Window] for why a node cuts it. The axis a caller is
	// shown has had it dropped ([EventHistogram.InsideHistory]), so every
	// bar of it lies wholly inside the history.
	Bucket EventBucket `json:"bucket"`
	Since  string      `json:"since"`
	Until  string      `json:"until"`

	// Bars is every bucket in the window, INCLUDING the empty ones.
	Bars []EventBar `json:"bars"`

	// Total is the whole window's count, which is the sum of the bars and
	// is stated anyway: a reader comparing a filtered axis to an
	// unfiltered one wants the number rather than a mental sum of forty
	// bars, and the alternative is every caller writing that sum.
	Total int `json:"total"`

	// Failed is the window's failed count — the sum of the bars' Failed,
	// stated for Total's reason.
	Failed int `json:"failed"`

	// ByCategory is how many rows each category would give, counted over the
	// window THAT WAS ASKED FOR — not the snapped one Since and Until report
	// — with every filter applied EXCEPT the category itself.
	//
	// WITHOUT THE CATEGORY, because that is the only meaning a facet count
	// can have: a chip says how many rows CHOOSING IT would show, and one
	// counted through its own filter reads zero on every chip but the
	// selected one. With it, a screen offering the category chips had to
	// count them over the rows the browser happened to be holding — so a
	// log reporting nine thousand events in its window offered a `system`
	// chip reading 0, which is a false statement about somebody's company
	// however carefully the caption hedges it.
	//
	// Every category the window holds is a key; one with no rows is
	// absent, and a caller rendering a closed set reads a missing key as
	// the zero it is.
	ByCategory map[string]int `json:"by_category"`
}

// Window reports the instants one node's part of the axis covers, after the
// history floor.
//
// Total in both edges, like [PhaseTokenQuery.Window] and for the same reason:
// the caller LABELS the answer, and an unbounded top edge is "up to now"
// rather than the zero time. Both edges are snapped OUTWARD to the bucket, so
// the first and last bars are whole ones rather than a partial bar at each end
// whose height means something different from its neighbours'.
//
// THE FLOOR IS SNAPPED DOWN LIKE ANY OTHER BOTTOM EDGE, so a window the history
// clips — the default one of every ask that names no `since` — begins at the
// bucket the floor cuts, and that first bar is a PARTIAL one: it counts only
// the rows above the floor. A node cuts it anyway, because this is the window
// every build cuts from the asker's pinned instant, and a fleet sums its nodes'
// bars INDEX BY INDEX ([eventfan.MergeSeries]). A build that began such a
// window at the next bucket instead cut one its peers could not sum: each side
// named the other's nodes rather than counting them, in both directions, for
// the whole of a rolling upgrade. So the partial bar is dropped by the ASKER,
// after the parts are summed ([EventHistogram.InsideHistory]) — what a caller
// is shown begins at the first whole bucket inside the history, and what one
// node answers is the shape every build sums.
func (q HistogramQuery) Window(now time.Time) (since, until time.Time) {
	step := q.Bucket.Step()
	floor := now.Add(-EventHistory)
	since = q.Since
	if since.IsZero() || since.Before(floor) {
		since = floor
	}
	// THE EDGE THE SNAP IS MEASURED AGAINST, held separately from the
	// query's own field because they are not the same value: an unbounded
	// top edge means `now`, and a snap that compared its truncated result
	// to the ZERO time never fired on exactly that caller. It rounded the
	// top edge DOWN instead — so a series asked for "up to now" ended at
	// the start of the bucket in progress, and every event of the current
	// minute, hour or day was in no bar, out of Total, and absent from an
	// axis whose own heading claimed to reach them.
	top := q.Until
	if top.IsZero() {
		top = now
	}
	if top.Before(since) {
		since = top
	}
	// DOWN for the bottom edge and UP for the top, so the window the bars
	// cover contains the window that was asked for rather than clipping
	// the rows at each end into bars nobody drew.
	since = since.UTC().Truncate(step)
	until = top.UTC().Truncate(step)
	if until.Before(top) || until.Equal(since) {
		until = until.Add(step)
	}
	return since, until
}

// HistoryStart is the first bucket boundary at or after the history floor
// under `at`: where the first bar lying wholly inside the history begins, and
// so where an axis a caller is shown may begin ([EventHistogram.InsideHistory]).
func (b EventBucket) HistoryStart(at time.Time) time.Time {
	step := b.Step()
	floor := at.Add(-EventHistory).UTC()
	start := floor.Truncate(step)
	if start.Before(floor) {
		start = start.Add(step)
	}
	return start
}

// InsideHistory is the axis as a caller is shown it: h, cut at the instant
// `at`, without the bars that begin below the first whole bucket inside the
// history ([EventBucket.HistoryStart]).
//
// A bar beginning below that bucket reaches below the floor, which every count
// stops at, so its height is that of its upper part alone — the partial bar
// the outward snap exists to prevent, labelled with the whole bucket while the
// rows beneath the floor in it are still on disk (retention keeps a day past
// it) and in no bar. [HistogramQuery.Window] still cuts it, for the reason it
// gives; this is where it goes. Since is raised to that bucket, and Total and
// Failed lose what the dropped bars counted, so they stay the sums of the bars.
// A window lying WHOLLY below the bucket comes back empty there — Since and
// Until both on it, no bars — rather than as a bar it cannot fill or the next
// one, which nobody asked for.
//
// ByCategory is left as it is: it counts the window that was asked for, which
// reaches down to the floor itself.
//
// The ASKER'S step, after the parts are summed — never a node's, before it
// answers — because a part is summed with its peers bar for bar, and the
// shape every build sums is the cut one.
func (h EventHistogram) InsideHistory(at time.Time) EventHistogram {
	start := h.Bucket.HistoryStart(at)
	drop := 0
	for drop < len(h.Bars) {
		bar, err := time.Parse(time.RFC3339, h.Bars[drop].At)
		if err != nil || !bar.Before(start) {
			break
		}
		drop++
	}
	out := h
	out.Bars = slices.Clone(h.Bars[drop:])
	for _, b := range h.Bars[:drop] {
		out.Total -= b.Count
		out.Failed -= b.Failed
	}
	edge := start.Format(time.RFC3339)
	if since, err := time.Parse(time.RFC3339, h.Since); err == nil && since.Before(start) {
		out.Since = edge
	}
	if until, err := time.Parse(time.RFC3339, h.Until); err == nil && until.Before(start) {
		out.Until = edge
	}
	return out
}

// Histogram counts the matching events per bucket: this node's part of the
// axis, over the window [HistogramQuery.Window] cuts — which a fleet's asker
// sums with every other node's and then holds inside the history
// ([EventHistogram.InsideHistory]).
func (l *EventLog) Histogram(ctx context.Context, q HistogramQuery) (EventHistogram, error) {
	if !q.Bucket.Valid() {
		return EventHistogram{}, fmt.Errorf("%w: bucket %q is not one of %v",
			ErrHistogramBucket, q.Bucket, EventBuckets)
	}
	if q.RelatedAgent != "" {
		return EventHistogram{}, ErrHistogramRelated
	}
	step := q.Bucket.Step()
	// ONE INSTANT FOR THE WHOLE ANSWER: the window is cut against it, and the
	// bars and the facet counts are floored under it — see [ListQuery.At]
	// for what flooring at a second read of the clock cost.
	at := q.at()
	since, until, query, args := q.barsSQL(at)
	bars := int(until.Sub(since) / step)
	if bars > MaxHistogramBuckets {
		return EventHistogram{}, fmt.Errorf("%w: %s over %s is %d buckets, and the "+
			"most this answers is %d — ask for a coarser bucket or a shorter window",
			ErrHistogramSpan, q.Bucket, until.Sub(since), bars, MaxHistogramBuckets)
	}

	rows, err := l.db.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return EventHistogram{}, fmt.Errorf("store: event histogram: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type cell struct{ count, failed int }
	counts := make(map[int64]cell, bars)
	total, failedTotal := 0, 0
	for rows.Next() {
		var at int64
		var c cell
		if err = rows.Scan(&at, &c.count, &c.failed); err != nil {
			return EventHistogram{}, fmt.Errorf("store: event histogram: %w", err)
		}
		counts[at] = c
		total += c.count
		failedTotal += c.failed
	}
	if err = rows.Err(); err != nil {
		return EventHistogram{}, fmt.Errorf("store: event histogram: %w", err)
	}

	// OVER THE WINDOW THAT WAS ASKED FOR, not the snapped one the bars
	// cover. A facet count's whole job is to say how many rows CHOOSING it
	// would show, and the rows come from the listing — which takes the
	// caller's own edges. Counted over the snapped window it would include
	// up to two buckets the list will never show, and on a busy hour that
	// is thousands of rows a chip claims and the list does not have.
	byCategory, err := l.countBy(ctx, q.ListQuery, "category", at)
	if err != nil {
		return EventHistogram{}, err
	}

	out := EventHistogram{
		Bucket: q.Bucket,
		Since:  since.Format(time.RFC3339),
		Until:  until.Format(time.RFC3339),
		// Never nil: a nil slice marshals to `null` and the client does
		// `.bars.length`, so an empty window would throw in the browser
		// rather than rendering an empty axis. The map is never nil for
		// the same reason — `Object.keys(null)` throws.
		Bars:       make([]EventBar, 0, bars),
		Total:      total,
		Failed:     failedTotal,
		ByCategory: byCategory,
	}
	// EVERY BUCKET, including the ones the GROUP BY had no row for. The
	// engine returns the whole window so a quiet hour is a gap of full
	// width rather than a bar the chart squeezed out, and filling it here
	// is what stops every caller writing the same loop.
	for at := since; at.Before(until); at = at.Add(step) {
		c := counts[EncodeTime(at)]
		out.Bars = append(out.Bars, EventBar{
			At:     at.Format(time.RFC3339),
			Count:  c.count,
			Failed: c.failed,
		})
	}
	return out, nil
}

// barsSQL is the statement [EventLog.Histogram] counts its bars with when it is
// asked at `at`, and its arguments, beside the window the bars cover — a
// function of its own, the window's cut included, so its plan can be read
// back for exactly the statement that runs at every bucket width
// (TestEveryGroupedReadSeeksItsFiltersIndex).
//
// THE WINDOW THE BARS COVER, not the one that was asked for: the edges are
// snapped to the bucket ([HistogramQuery.Window]), and counting rows outside
// them would put events in no bar at all.
//
// It groups over the table itself, unlike [ListQuery.facetSQL], and still
// seeks a filter's index. MEASURED, NOT REASONED: grouped by this expression
// over `event_time`, the planner takes the filter's index range — with the
// window's edges or with the floor alone — where a facet count grouped by a
// column over the table intersected that index with the primary key's floor
// range. Nothing promises a planner keeps either choice, so the gate reads the
// plan back for the statement this returns, at every bucket width.
func (q HistogramQuery) barsSQL(at time.Time) (since, until time.Time, query string, args []any) {
	step := q.Bucket.Step()
	since, until = q.Window(at)
	filters := q.ListQuery
	filters.Since, filters.Until = since, until
	from, where, args, col := filters.predicate(at)
	// Integer arithmetic on the stored microseconds — the column is a
	// UnixMicro (see [EncodeTime]) — so the bucket is a division rather
	// than a date function, and every engine agrees about what it means.
	// The step is a compile-time-formatted constant rather than a bound
	// parameter, because a GROUP BY expression carrying a parameter is one
	// the planner cannot reuse a plan for.
	micros := strconv.FormatInt(step.Microseconds(), 10)
	bucketExpr := "(" + col("event_time") + " / " + micros + ") * " + micros
	// THE FAILED SPLIT IS [failedRow], the turn list's own predicate, so a
	// bar's failed share and a turn's failed mark are one rule rather than
	// two that agree — and the `failed` FILTER is the same rule again
	// ([ListQuery.Failed]), so an axis narrowed to failures has a failed
	// share equal to its height. Qualified through `col` like every other
	// column here, although this read never joins (a related-agent axis is
	// refused by [EventLog.Histogram]).
	failedExpr, failedArgs := failedRow(col)
	query = "SELECT " + bucketExpr + " AS bucket, COUNT(*), " +
		"SUM(CASE WHEN " + failedExpr + " THEN 1 ELSE 0 END) FROM " + from +
		" WHERE " + strings.Join(where, " AND ") + " GROUP BY bucket ORDER BY bucket"
	return since, until, query, append(failedArgs, args...)
}

// countBy counts the window's rows per value of one column, with that column's
// own filter LIFTED.
//
// A facet count says how many rows choosing that value would show, so it cannot
// be counted through the filter it is offering to set: counted with it, every
// chip but the selected one reads zero, which is not a fact about anything.
// Every other filter still applies, because a chip has to answer "how many,
// given what is already narrowed".
//
// `at` is the instant the answer it belongs to is asked at, so the chips are
// floored where the bars beside them are — see [ListQuery.predicate].
func (l *EventLog) countBy(ctx context.Context, filters ListQuery, column string, at time.Time) (map[string]int, error) {
	switch column {
	case "category":
		filters.Category = ""
	default:
		return nil, fmt.Errorf("store: no facet count for %q", column)
	}
	query, args := filters.facetSQL(column, at)

	rows, err := l.db.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: event facet %s: %w", column, err)
	}
	defer func() { _ = rows.Close() }()

	out := map[string]int{}
	for rows.Next() {
		var value string
		var count int
		if err := rows.Scan(&value, &count); err != nil {
			return nil, fmt.Errorf("store: event facet %s: %w", column, err)
		}
		out[value] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: event facet %s: %w", column, err)
	}
	return out, nil
}

// facetSQL is the statement [EventLog.countBy] runs for one column over these
// filters, and its arguments — a function of its own so its plan can be read
// back for exactly the statement that runs
// (TestEveryGroupedReadSeeksItsFiltersIndex). `column` is one of countBy's own
// constants, never a caller's text.
//
// THE ROWS ARE SELECTED IN A DERIVED TABLE and counted outside it — see
// [EventLog]. Grouped over the table itself, a chip count narrowed to a seat,
// a trace, a turn, a unit of work, an item or a channel intersected that
// filter's index with every row id of the thirty-day floor's range, on every
// axis a screen draws.
func (q ListQuery) facetSQL(column string, at time.Time) (string, []any) {
	from, where, args, col := q.predicate(at)
	return "SELECT " + column + ", COUNT(*) FROM (SELECT " + col(column) + " AS " + column +
		" FROM " + from + " WHERE " + strings.Join(where, " AND ") + ") GROUP BY " + column, args
}

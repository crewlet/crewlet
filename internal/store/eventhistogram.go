package store

import (
	"context"
	"errors"
	"fmt"
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
// [tokens.Interval] is two: a chart with an arbitrary bucket width has an x
// axis nobody can label. A log needs the minute the spend series does not —
// "what just happened" is the commonest question asked of an event log, and an
// hour is the whole of the answer's window.
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
type HistogramQuery struct {
	ListQuery

	// Bucket is the bar width. An invalid one is refused naming what is
	// accepted, never defaulted — an axis labelled by one width over
	// another's bars is worse than an error.
	Bucket EventBucket

	// At is the instant the window is cut against — what an unbounded top
	// edge means, and where the history floor sits. Zero is now.
	//
	// A FIELD rather than every log reading its own clock, because a fleet
	// asks several logs for ONE axis (internal/eventfan) and sums their
	// bars index by index: two nodes whose clocks straddle a minute would
	// otherwise snap to windows one bar apart, and every bar of the sum
	// would add one node's minute to the other's next one.
	At time.Time
}

// at is the instant the window is cut against.
func (q HistogramQuery) at() time.Time {
	if q.At.IsZero() {
		return now()
	}
	return q.At
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

// Window reports the instants this query covers, after the retention floor.
//
// Total in both edges, like [PhaseTokenQuery.Window] and for the same reason:
// the caller LABELS the answer, and an unbounded top edge is "up to now"
// rather than the zero time. Both edges are snapped OUTWARD to the bucket, so
// the first and last bars are whole ones rather than a partial bar at each end
// whose height means something different from its neighbours'.
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

// Histogram counts the matching events per bucket.
func (l *EventLog) Histogram(ctx context.Context, q HistogramQuery) (EventHistogram, error) {
	if !q.Bucket.Valid() {
		return EventHistogram{}, fmt.Errorf("%w: bucket %q is not one of %v",
			ErrHistogramBucket, q.Bucket, EventBuckets)
	}
	if q.RelatedAgent != "" {
		return EventHistogram{}, ErrHistogramRelated
	}
	step := q.Bucket.Step()
	since, until := q.Window(q.at())
	bars := int(until.Sub(since) / step)
	if bars > MaxHistogramBuckets {
		return EventHistogram{}, fmt.Errorf("%w: %s over %s is %d buckets, and the "+
			"most this answers is %d — ask for a coarser bucket or a shorter window",
			ErrHistogramSpan, q.Bucket, until.Sub(since), bars, MaxHistogramBuckets)
	}

	// THE WINDOW THE BARS COVER, not the one that was asked for: the edges
	// were snapped to the bucket above, and counting rows outside them
	// would put events in no bar at all.
	filters := q.ListQuery
	filters.Since, filters.Until = since, until
	from, where, args, col := filters.predicate()

	// Integer arithmetic on the stored microseconds — the column is a
	// UnixMicro (see [EncodeTime]) — so the bucket is a division rather
	// than a date function, and every engine agrees about what it means.
	// The step is a compile-time-formatted constant rather than a bound
	// parameter, because a GROUP BY expression carrying a parameter is one
	// the planner cannot reuse a plan for.
	micros := strconv.FormatInt(step.Microseconds(), 10)
	bucketExpr := "(" + col("event_time") + " / " + micros + ") * " + micros
	query := "SELECT " + bucketExpr + " AS bucket, COUNT(*) FROM " + from +
		" WHERE " + strings.Join(where, " AND ") + " GROUP BY bucket ORDER BY bucket"

	rows, err := l.db.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return EventHistogram{}, fmt.Errorf("store: event histogram: %w", err)
	}
	defer func() { _ = rows.Close() }()

	counts := make(map[int64]int, bars)
	total := 0
	for rows.Next() {
		var at int64
		var count int
		if err = rows.Scan(&at, &count); err != nil {
			return EventHistogram{}, fmt.Errorf("store: event histogram: %w", err)
		}
		counts[at] = count
		total += count
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
	byCategory, err := l.countBy(ctx, q.ListQuery, "category")
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
		ByCategory: byCategory,
	}
	// EVERY BUCKET, including the ones the GROUP BY had no row for. The
	// engine returns the whole window so a quiet hour is a gap of full
	// width rather than a bar the chart squeezed out, and filling it here
	// is what stops every caller writing the same loop.
	for at := since; at.Before(until); at = at.Add(step) {
		out.Bars = append(out.Bars, EventBar{
			At:    at.Format(time.RFC3339),
			Count: counts[EncodeTime(at)],
		})
	}
	return out, nil
}

// countBy counts the window's rows per value of one column, with that column's
// own filter LIFTED.
//
// A facet count says how many rows choosing that value would show, so it cannot
// be counted through the filter it is offering to set: counted with it, every
// chip but the selected one reads zero, which is not a fact about anything.
// Every other filter still applies, because a chip has to answer "how many,
// given what is already narrowed".
func (l *EventLog) countBy(ctx context.Context, filters ListQuery, column string) (map[string]int, error) {
	switch column {
	case "category":
		filters.Category = ""
	default:
		return nil, fmt.Errorf("store: no facet count for %q", column)
	}
	from, where, args, col := filters.predicate()
	query := "SELECT " + col(column) + ", COUNT(*) FROM " + from +
		" WHERE " + strings.Join(where, " AND ") + " GROUP BY " + col(column)

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

package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
)

// WHAT BECAME OF THE NOTIFICATIONS, counted per third-party app over a window.
//
// A COUNT rather than a page of the events it counts, because the question is
// "how many in this window" and a page answers "how many among the newest N",
// whose span is its own: the integrations answer used to count its outcomes
// over the newest 400 `notification` events while it named the window its
// deliveries covered, so on a company whose notification events outnumbered
// its deliveries the two numbers in one row described two different stretches
// of time with nothing to say so. Counted here, over the window the caller
// names, the outcomes are bounded by exactly the edges the deliveries beside
// them are.

// OutcomeQuery is the window a count of notification outcomes covers: the
// half-open interval `[Since, At)`.
type OutcomeQuery struct {
	// Since is the window's bottom edge, inclusive. Zero is the history
	// floor, and an edge below the floor is held to it: what lies under
	// [EventHistory] is not history the log serves, whatever its sweep has
	// not deleted yet.
	Since time.Time

	// At is the instant the count is asked at: the window's TOP edge,
	// exclusive, and the instant the history floor sits [EventHistory]
	// below — so the count is floored where every other read of one answer
	// is ([ListQuery.At]). Zero is now.
	//
	// THE TOP EDGE AS WELL as the floor, unlike a listing's, whose unbounded
	// top is the newest rows a feed is read for: a count stated beside a
	// window must not also count what was written after the window was
	// named, or a fleet's sum would be over as many top edges as it has
	// nodes, each its own clock's reading of "now".
	At time.Time
}

// window is the half-open window `[since, until)` the count covers, its bottom
// edge held to the history floor under its instant.
//
// `at` is the instant the query is asked at — [askedAt] read ONCE by the
// caller, so the edge and the floor come from one reading of the clock.
func (q OutcomeQuery) window(at time.Time) (since, until time.Time) {
	since = at.Add(-EventHistory)
	if q.Since.After(since) {
		since = q.Since.UTC()
	}
	return since, at
}

// NotificationOutcomes is how many notifications each third-party app had
// SKIPPED — dropped by the routing gate without waking anybody — and how many
// MERGES coalesced several of its notifications into one turn, keyed by the
// app the events name.
//
// A struct of two maps rather than a list of (app, outcome, count) rows,
// because it is what the one caller reports and what a fleet sums: each map is
// keyed by the third-party app, and summing two answers is adding the counts
// under each key. Never nil maps, so an answer with nothing to count marshals
// as `{}` rather than `null`, which a peer's merge would read the same way but
// a person reading the wire would not.
type NotificationOutcomes struct {
	Skipped   map[string]int `json:"skipped"`
	Coalesced map[string]int `json:"coalesced"`
}

// The event types an outcome count reads, each counted into its own map.
// Spelled through the types' own EventType, so the wire string is the one the
// publisher writes rather than a second copy of it here.
var (
	skippedType   = types.NotificationSkipped{}.EventType()
	coalescedType = types.NotificationsCoalesced{}.EventType()

	outcomeTypes = []string{skippedType, coalescedType}
)

// NotificationOutcomes counts the outcomes in the query's window per
// third-party app.
//
// THE APP IS THE `notification_source` TAG, not the event's source: the source
// of an engine-published event names the engine, and what a count has to line
// up with is one app's deliveries. A row written before that tag existed
// carries none and is not counted — a real discontinuity at that point in the
// timeline (see the tag's entry in [tagKeys]), never a guess at which app it
// concerned. The tag is TRIMMED here rather than in SQL, by the one rule every
// reader of it applies ([strings.TrimSpace]; SQL's TRIM strips spaces alone),
// and tags that trim to one app are counted as that app.
func (l *EventLog) NotificationOutcomes(ctx context.Context, q OutcomeQuery) (NotificationOutcomes, error) {
	out := NotificationOutcomes{Skipped: map[string]int{}, Coalesced: map[string]int{}}
	query, args := q.countSQL(askedAt(q.At))
	rows, err := l.db.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return NotificationOutcomes{}, fmt.Errorf("store: count notification outcomes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var kind, tag string
		var count int
		if err := rows.Scan(&kind, &tag, &count); err != nil {
			return NotificationOutcomes{}, fmt.Errorf("store: count notification outcomes: %w", err)
		}
		app := strings.TrimSpace(tag)
		if app == "" {
			continue
		}
		switch kind {
		case skippedType:
			out.Skipped[app] += count
		case coalescedType:
			out.Coalesced[app] += count
		}
	}
	if err := rows.Err(); err != nil {
		return NotificationOutcomes{}, fmt.Errorf("store: count notification outcomes: %w", err)
	}
	return out, nil
}

// countSQL is the statement [EventLog.NotificationOutcomes] runs and its
// arguments, a function of its own so its plan can be read back for exactly
// the statement that runs (TestEveryGroupedReadSeeksItsFiltersIndex). `at` is
// the instant the query is asked at.
//
// BY TYPE, on the type index, and nothing else: the outcome types are the
// filter, and `crewlet_events_type_time_idx` is (event_type, event_time …), so
// the read is one seek of the window's time range per type — the outcome rows
// in the window and no others. The CATEGORY would select them too, and would
// read every `external_notification` beside them, which is the bulk of the
// category.
//
// ONE SELECT PER TYPE, joined by UNION ALL, because that is the only spelling
// the planner seeks the WINDOW in. `event_type IN (?, ?)` seeks the type alone
// and reads every row of it the log still holds, the window's edges applied as
// a filter afterwards; `(type = ? AND …) OR (type = ? AND …)` intersects two
// index reads (`MULTI-INDEX OR`). Each arm here is `event_type = ?` beside both
// edges, which is a range of the index — read back in the gate.
//
// THE APP IS READ FROM THE ROW'S TAGS, so each counted row costs a lookup of
// its own — and only the counted rows do, which is the cost the gate holds
// every grouped read to. That is why no migration promotes the tag to a column
// with an index covering it: the read is already bounded by the rows it is
// about — the window's outcome events, read a few times a minute by a screen —
// where a column and its index are a write on every event the log ever
// stores, almost none of which carries the tag.
//
// THE ROWS ARE SELECTED IN A DERIVED TABLE and grouped outside it, for the
// reason [EventLog] gives: grouped over the table itself, the floor is a range
// of the primary key the planner reaches for instead.
func (q OutcomeQuery) countSQL(at time.Time) (string, []any) {
	since, until := q.window(at)
	arms := make([]string, 0, len(outcomeTypes))
	args := make([]any, 0, 3*len(outcomeTypes))
	for _, kind := range outcomeTypes {
		arms = append(arms, "SELECT event_type, "+
			"COALESCE(json_extract(tags, '$.notification_source'), '') AS app "+
			"FROM crewlet_events WHERE event_type = ? AND event_time >= ? AND event_time < ?")
		args = append(args, kind, EncodeTime(since), EncodeTime(until))
	}
	return "SELECT event_type, app, COUNT(*) FROM (" + strings.Join(arms, " UNION ALL ") +
		") GROUP BY event_type, app", args
}

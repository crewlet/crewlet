package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
)

// WHAT BECAME OF THE NOTIFICATIONS, counted per third-party app over a window.
//
// A COUNT rather than a page: a page answers "how many among the newest N",
// a span of its own. Counted over the caller's window, outcomes share the
// edges of the deliveries beside them.
//
// # A row a node holds is not always a row it keeps
//
// The count is of rows this node KEEPS; the window's rows of an unsettled
// custody batch it wrote are named beside it, for the asker to count once
// across the fleet — see unsettled.go ([NotificationOutcomes.Unsettled],
// [EventLog.KeptRows]).

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

	// Unsettled is the window's outcome rows this node holds WITHOUT KNOWING
	// IT KEEPS THEM — rows of a custody batch it wrote and has not settled —
	// which the two maps leave out. Another data node may hold the same row,
	// kept or not yet settled either, so they are named by identity rather
	// than counted, and whoever sums the fleet counts each one once. Empty on
	// a node with no batch in flight, which is nearly always, and on an
	// answer that already counted them (a fleet's merge).
	Unsettled []UnsettledRow `json:"unsettled,omitempty"`
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
// third-party app — the rows this node KEEPS — and names the window's rows it
// holds unsettled beside the counts (see the file doc).
//
// THE APP IS THE `notification_source` TAG, not the event's source (which names
// the engine). A row with an empty or blank tag is not counted, never guessed.
// The tag is TRIMMED here, not in SQL, by the rule every reader applies
// ([strings.TrimSpace]; SQL's TRIM strips spaces alone).
//
// ONE SNAPSHOT for the count and the unsettled rows, because the second is
// taken off the first: read apart, a batch settled between the two would be
// counted as kept and named as unsettled, or neither.
func (l *EventLog) NotificationOutcomes(ctx context.Context, q OutcomeQuery) (NotificationOutcomes, error) {
	at := askedAt(q.At)
	var out NotificationOutcomes
	if err := l.db.Read(ctx, func(tx *sql.Tx) error {
		out = NotificationOutcomes{Skipped: map[string]int{}, Coalesced: map[string]int{}}
		query, args := q.countSQL(at)
		if err := scanOutcomes(ctx, tx, query, args, func(kind, app string, count int) {
			out.add(kind, app, count)
		}); err != nil {
			return err
		}
		where, args := q.unsettledWhere(at)
		named, err := readUnsettled(ctx, tx, where, args)
		if err != nil {
			return err
		}
		for _, r := range named {
			// A ROW WITH NO APP IS IN NO COUNT, so there is nothing to take
			// it back out of, nor anything for the asker to add.
			if r.App == "" {
				continue
			}
			out.add(r.Type, r.App, -1)
			out.Unsettled = append(out.Unsettled, r)
		}
		return nil
	}); err != nil {
		return NotificationOutcomes{}, fmt.Errorf("store: count notification outcomes: %w", err)
	}
	return out, nil
}

// Add counts one row under its type and app — what a fleet's merge does with a
// row some node named rather than counted. A row of another type counts
// nowhere.
func (o *NotificationOutcomes) Add(r UnsettledRow) { o.add(r.Type, r.App, 1) }

// add counts n of an outcome type under an app, dropping a key its last row
// was taken back from: an app with nothing kept is absent, like one with
// nothing at all.
func (o *NotificationOutcomes) add(kind, app string, n int) {
	var counts map[string]int
	switch kind {
	case skippedType:
		counts = o.Skipped
	case coalescedType:
		counts = o.Coalesced
	default:
		return
	}
	if counts[app] += n; counts[app] == 0 {
		delete(counts, app)
	}
}

// scanOutcomes runs a statement answering (type, raw app tag, count) rows and
// hands each with its app trimmed, skipping a row that names no app.
func scanOutcomes(ctx context.Context, tx *sql.Tx, query string, args []any,
	each func(kind, app string, count int),
) error {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var kind, tag string
		var count int
		if err := rows.Scan(&kind, &tag, &count); err != nil {
			return err
		}
		if app := strings.TrimSpace(tag); app != "" {
			each(kind, app, count)
		}
	}
	return rows.Err()
}

// countSQL is the statement [EventLog.NotificationOutcomes] runs, a function so
// its plan is read back (TestEveryGroupedReadSeeksItsFiltersIndex).
//
// BY TYPE on `crewlet_events_type_time_idx`: one seek of the window's range per
// type; the CATEGORY would also read every `external_notification`. ONE SELECT
// PER TYPE, UNION ALL, the only spelling that seeks the WINDOW: `event_type IN
// (?, ?)` filters the window afterwards, an OR is a `MULTI-INDEX OR`.
//
// THE APP IS READ FROM THE ROW'S TAGS: a column would cost a write on every
// event. Rows are selected in a DERIVED TABLE and grouped outside it ([EventLog]).
// EVERY ROW OF THE WINDOW, unsettled ones included: [OutcomeQuery.unsettledWhere]
// names them in the same snapshot and they are taken back out.
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

// unsettledWhere is the predicate [unsettledSQL] narrows the custody rows to
// for this count: the window's outcome rows — what [EventLog.NotificationOutcomes]
// names instead of counting — and its arguments; `at` as for
// [OutcomeQuery.countSQL].
func (q OutcomeQuery) unsettledWhere(at time.Time) ([]string, []any) {
	since, until := q.window(at)
	args := make([]any, 0, len(outcomeTypes)+2)
	for _, kind := range outcomeTypes {
		args = append(args, kind)
	}
	args = append(args, EncodeTime(since), EncodeTime(until))
	return []string{
		"event_type IN (?" + strings.Repeat(", ?", len(outcomeTypes)-1) + ")",
		"event_time >= ?", "event_time < ?",
	}, args
}

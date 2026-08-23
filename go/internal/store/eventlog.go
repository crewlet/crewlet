package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
)

// ErrNotFound reports that a row a caller named by id does not exist.
var ErrNotFound = errors.New("store: not found")

const (
	// EventHistory is how far back the event log is read. This is the hard
	// bottom of paging: once a cursor crosses it every page is empty
	// forever, and a UI that cannot name the floor draws that as "the org
	// went quiet". It is a named constant so the API can ship the number to
	// a dashboard footer rather than each query inlining a literal.
	EventHistory = 30 * 24 * time.Hour

	// EventRetention is how long rows are kept — one day past the read
	// floor, and never less than it.
	//
	// The direction matters more than the margin. Sweeping SHORTER than the
	// read floor deletes rows a reader can still ask for, so a page
	// assembled at the boundary loses rows underneath it and the floor
	// stops describing the table. A day of slack is longer than any paging
	// session and three orders of magnitude longer than the maintenance
	// tick, so the sweep never races a reader.
	//
	// That a sweep exists at all is the change from the Postgres schema,
	// where this table — the highest-volume one in the deployment — had no
	// retention policy while every read of it stopped at 30 days. Rows past
	// the floor were unreachable and permanent.
	EventRetention = EventHistory + 24*time.Hour

	// MaxTraceEvents caps one trace's rows. A trace is unbounded in
	// principle — a long turn with sub-agents accumulates thousands of
	// spans — and the whole thing goes out in a single WebSocket frame, so
	// an uncapped read is a query that times out client-side and reports as
	// a generic failure. The OLDEST rows are kept: the root is what
	// explains a trace, and a truncated tail is legible where a truncated
	// head is not.
	MaxTraceEvents = 500

	// defaultListLimit is the page size when a caller names none.
	defaultListLimit = 50

	// relatedFetchMultiple and relatedFetchCap bound the over-fetch the
	// RelatedAgent filter needs: it matches across the actor column plus
	// several keys inside the tags blob, which no index covers, so it runs
	// after the query and the query must return rows for it to match in.
	//
	// A heuristic, and safe to be one, because it cannot make an answer
	// WRONG — only short. That surface's termination condition is a
	// zero-row page rather than a short one (see ListQuery.RelatedAgent),
	// so under-fetching costs an extra round trip and nothing else. The cap
	// is what keeps a broad filter from pulling a whole page of history to
	// discard it.
	relatedFetchMultiple = 5
	relatedFetchCap      = 500
)

// EventRecord is one row of the audit log.
//
// Payload is populated only by ByID. List and Trace deliberately leave it nil:
// they never select the column, because thirty days of serialized events is a
// large amount of JSON to move for a listing that shows a summary line.
type EventRecord struct {
	ID           string
	Type         string
	Source       string
	Time         time.Time
	Category     string
	Summary      string
	Actor        string
	TraceID      string
	SpanID       string
	ParentSpanID string

	// Tags are the filterable dimensions the writer extracted. The
	// promoted columns (agent_id, agent_role, task_id, channel_id, sender)
	// are copies of five of these; the rest exist only here.
	Tags map[string]string

	// Payload is the full serialized event. Nil on a listing — see above.
	Payload json.RawMessage

	// Failed says whether the work this event reports failed. Derived on
	// read from the event type plus the stored `failed` tag, because a
	// listing never selects the payload and the tag is all that survives
	// into history. Events written before the writer stamped that tag read
	// back as not-failed — a real discontinuity at that point in the
	// timeline, not a bug to paper over.
	Failed bool
}

// Cursor is an exclusive keyset position: the reader holds this row and wants
// what comes before it.
//
// The ID half is not optional. (Time, ID) is the table's primary key and
// therefore unique; Time alone is not, because burst writes routinely share a
// timestamp at microsecond resolution. A cursor over a non-unique key skips or
// repeats whatever collided with it, and the reader who scrolled past the gap
// gets no error either way.
type Cursor struct {
	Time time.Time
	ID   string
}

// ListQuery selects a page of the event log. The zero value asks for the most
// recent defaultListLimit events.
type ListQuery struct {
	Limit    int
	Type     string
	Source   string
	Category string
	TraceID  string
	Actor    string

	// RelatedAgent is a broad filter: events whose actor is the agent, or
	// whose tags name it as agent_role / target / recipient / sender, plus
	// every event sharing a trace with one of those — so the inbound
	// webhook that caused the agent's work shows up beside it.
	//
	// It over-fetches and post-filters, so a page shorter than Limit does
	// NOT mean history is exhausted here; only a zero-row page does.
	RelatedAgent string

	// Before is an exclusive cursor. Nil starts at the newest row.
	Before *Cursor
}

// EventLog is the audit and observability event store.
type EventLog struct{ db *DB }

// Events returns the audit log backed by this database.
func (d *DB) Events() *EventLog { return &EventLog{db: d} }

const eventInsertSQL = `
INSERT INTO crewlet_events (
	event_time, event_id, event_type, source, category,
	trace_id, span_id, parent_span_id,
	agent_id, agent_role, task_id, channel_id, sender,
	summary, actor, tags, payload
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (event_time, event_id) DO NOTHING`

// ErrIncompleteRecord reports a record missing part of its identity.
var ErrIncompleteRecord = errors.New("store: incomplete event record")

// Append writes one record, idempotently.
//
// ON CONFLICT DO NOTHING rather than an error, because the duplicate is
// expected: a publish retry, a replay, a redelivery after a node died all
// present the same (time, id) pair. Raising instead would make every one of
// them log a write failure that describes nothing wrong.
func (l *EventLog) Append(ctx context.Context, rec EventRecord) error {
	// The identity is checked here because SQL cannot check it. NOT NULL
	// catches a missing column; it does not catch a zero one, and both
	// halves of this key have a zero value that stores fine and then reads
	// as nothing: a zero Time lands in year 1, permanently below the read
	// floor, and an empty ID collides with every other record that forgot
	// the same field. Either way the row exists and no query returns it.
	if rec.ID == "" {
		return fmt.Errorf("%w: no event id", ErrIncompleteRecord)
	}
	if rec.Time.IsZero() {
		return fmt.Errorf("%w: event %s has no timestamp", ErrIncompleteRecord, rec.ID)
	}
	tags := rec.Tags
	if tags == nil {
		tags = map[string]string{}
	}
	tagJSON, err := json.Marshal(tags)
	if err != nil {
		return fmt.Errorf("store: encode event tags (%s): %w", rec.ID, err)
	}
	payload := rec.Payload
	if len(payload) == 0 {
		payload = json.RawMessage("{}")
	}
	if _, err := l.db.sql.ExecContext(ctx, eventInsertSQL,
		EncodeTime(rec.Time), rec.ID, rec.Type, rec.Source, rec.Category,
		rec.TraceID, rec.SpanID, rec.ParentSpanID,
		tags["agent_id"], tags["agent_role"], tags["task_id"],
		tags["channel_id"], tags["sender"],
		rec.Summary, rec.Actor, string(tagJSON), string(payload),
	); err != nil {
		return fmt.Errorf("store: append event %s: %w", rec.ID, err)
	}
	return nil
}

// listColumns is every column a listing reads. `payload` is absent
// deliberately — see EventRecord.Payload.
const listColumns = `event_time, event_id, event_type, source, category,
	summary, actor, trace_id, span_id, parent_span_id, tags`

// List returns a page of events, newest first, ordered by (time, id)
// descending.
func (l *EventLog) List(ctx context.Context, q ListQuery) ([]EventRecord, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}

	where := []string{"event_time >= ?"}
	args := []any{EncodeTime(now().Add(-EventHistory))}
	addEq := func(col, val string) {
		if val != "" {
			where = append(where, col+" = ?")
			args = append(args, val)
		}
	}
	addEq("event_type", q.Type)
	addEq("source", q.Source)
	addEq("category", q.Category)
	addEq("trace_id", q.TraceID)
	addEq("actor", q.Actor)

	if q.Before != nil {
		// Keyset, not OFFSET: (event_time, event_id) is the primary key,
		// so it is unique and already in index order — no sort node, and
		// no drift as new rows land at the head while a reader pages
		// backwards.
		if q.Before.ID != "" {
			where = append(where, "(event_time, event_id) < (?, ?)")
			args = append(args, EncodeTime(q.Before.Time), q.Before.ID)
		} else {
			where = append(where, "event_time < ?")
			args = append(args, EncodeTime(q.Before.Time))
		}
	}

	fetch := limit
	if q.RelatedAgent != "" {
		fetch = min(limit*relatedFetchMultiple, relatedFetchCap)
	}
	args = append(args, fetch)

	// Every fragment joined into `where` is a compile-time constant; each
	// one's value travels as a bound parameter in args.
	query := "SELECT " + listColumns + " FROM crewlet_events WHERE " +
		strings.Join(where, " AND ") +
		" ORDER BY event_time DESC, event_id DESC LIMIT ?"

	out, err := l.scanRows(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	if q.RelatedAgent != "" {
		out = collectRelated(out, q.RelatedAgent, limit)
	}
	return out, nil
}

// Trace returns every event in a trace, OLDEST first, because a trace is read
// as a causal sequence rather than a feed. A caller that gets exactly
// MaxTraceEvents rows should say the view is truncated.
func (l *EventLog) Trace(ctx context.Context, traceID string) ([]EventRecord, error) {
	return l.scanRows(ctx,
		"SELECT "+listColumns+" FROM crewlet_events "+
			"WHERE trace_id = ? AND event_time >= ? "+
			"ORDER BY event_time ASC, event_id ASC LIMIT ?",
		traceID, EncodeTime(now().Add(-EventHistory)), MaxTraceEvents)
}

// ByID returns one event WITH its payload, or ErrNotFound.
//
// The identity is (event_time, event_id) and a caller holding only an id — a
// link, a line pasted from a log — has no time to seek with, so this reads the
// id index and takes the newest match.
func (l *EventLog) ByID(ctx context.Context, id string) (EventRecord, error) {
	row := l.db.sql.QueryRowContext(ctx,
		"SELECT "+listColumns+", payload FROM crewlet_events "+
			"WHERE event_id = ? ORDER BY event_time DESC LIMIT 1", id)

	var rec EventRecord
	var micros int64
	var tagJSON, payload string
	if err := row.Scan(&micros, &rec.ID, &rec.Type, &rec.Source, &rec.Category,
		&rec.Summary, &rec.Actor, &rec.TraceID, &rec.SpanID, &rec.ParentSpanID,
		&tagJSON, &payload,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return EventRecord{}, fmt.Errorf("%w: event %s", ErrNotFound, id)
		}
		return EventRecord{}, fmt.Errorf("store: read event %s: %w", id, err)
	}
	finishRecord(&rec, micros, tagJSON)
	rec.Payload = json.RawMessage(payload)
	return rec, nil
}

// Purge deletes events past EventRetention and reports how many went.
//
// It takes no window, deliberately. The retention is derived from the read
// floor and has no honest reason to vary per deployment: a shorter one deletes
// rows the log still serves, and a longer one keeps rows nothing can reach.
// Offering the choice would only make both mistakes possible.
func (l *EventLog) Purge(ctx context.Context) (int64, error) {
	res, err := l.db.sql.ExecContext(ctx,
		"DELETE FROM crewlet_events WHERE event_time < ?",
		EncodeTime(now().Add(-EventRetention)))
	if err != nil {
		return 0, fmt.Errorf("store: purge events: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: purge events count: %w", err)
	}
	return n, nil
}

func (l *EventLog) scanRows(ctx context.Context, query string, args ...any) ([]EventRecord, error) {
	rows, err := l.db.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []EventRecord
	for rows.Next() {
		var rec EventRecord
		var micros int64
		var tagJSON string
		if err := rows.Scan(&micros, &rec.ID, &rec.Type, &rec.Source,
			&rec.Category, &rec.Summary, &rec.Actor, &rec.TraceID,
			&rec.SpanID, &rec.ParentSpanID, &tagJSON,
		); err != nil {
			return nil, fmt.Errorf("store: scan event: %w", err)
		}
		finishRecord(&rec, micros, tagJSON)
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate events: %w", err)
	}
	return out, nil
}

// finishRecord fills in the fields derived from the raw columns.
func finishRecord(rec *EventRecord, micros int64, tagJSON string) {
	rec.Time = DecodeTime(micros)
	tags := map[string]string{}
	// A tags blob that will not decode costs the failure flag and the
	// related-agent match for that one row; it must not cost the row. This
	// is the same choice the Python reader makes, and the alternative —
	// failing the page — hides every event around the malformed one too.
	if err := json.Unmarshal([]byte(tagJSON), &tags); err != nil {
		tags = map[string]string{}
	}
	rec.Tags = tags
	rec.Failed = types.Failed(rec.Type, false, tags["failed"] == "true")
}

// agentTagKeys are the tag keys that mean "this event involves that agent".
var agentTagKeys = []string{"agent_role", "target", "recipient", "sender"}

// relatesToAgent reports whether an event is DIRECTLY about the named agent.
func relatesToAgent(rec EventRecord, name string) bool {
	if rec.Actor == name {
		return true
	}
	return slices.ContainsFunc(agentTagKeys, func(k string) bool {
		return rec.Tags[k] == name
	})
}

// collectRelated returns the events related to name, plus their trace
// siblings.
//
// Two passes, because the second is the point: an agent's work is caused by
// something — a webhook, a notification, a schedule tick — and that trigger
// carries the agent's name nowhere. Pulling in everything sharing a trace with
// a direct match is what puts the cause next to the effect.
func collectRelated(all []EventRecord, name string, limit int) []EventRecord {
	var direct []EventRecord
	traces := map[string]struct{}{}
	for _, rec := range all {
		if relatesToAgent(rec, name) {
			direct = append(direct, rec)
			if rec.TraceID != "" {
				traces[rec.TraceID] = struct{}{}
			}
		}
	}
	if len(traces) == 0 {
		return truncate(direct, limit)
	}

	seen := make(map[string]struct{}, len(direct))
	for _, rec := range direct {
		seen[rec.ID] = struct{}{}
	}
	out := direct
	for _, rec := range all {
		if _, dup := seen[rec.ID]; dup {
			continue
		}
		if _, ok := traces[rec.TraceID]; ok {
			out = append(out, rec)
			seen[rec.ID] = struct{}{}
		}
	}
	// Re-sort: the sibling pass appends in scan order, so the merged list
	// is no longer the newest-first order the query guaranteed. Same
	// (time, id) tiebreak as the query, for the same reason.
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].Time.Equal(out[j].Time) {
			return out[i].Time.After(out[j].Time)
		}
		return out[i].ID > out[j].ID
	})
	return truncate(out, limit)
}

func truncate(recs []EventRecord, limit int) []EventRecord {
	if limit > 0 && len(recs) > limit {
		return recs[:limit]
	}
	return recs
}

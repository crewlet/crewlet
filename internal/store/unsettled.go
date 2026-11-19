package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// A ROW A NODE HOLDS IS NOT ALWAYS A ROW IT KEEPS.
//
// A failed-claim custody batch (internal/observe) is also written by a second
// keeper, so until it settles a fleet sum counts its rows twice. Every COUNT
// counts the rows this node KEEPS and treats those of a batch it wrote and not
// settled ([EventLog.WriteCustody]) by who reads it:
//   - ANSWERED to a fleet read: names them as [UnsettledRow]s in the SAME
//     SNAPSHOT (read apart, a settling batch is counted kept and named, or
//     neither); the asker counts each once ([EventLog.KeptRows]). [unsettledSQL].
//   - PUBLISHED ([DB.UsageForDay]): leaves them out, as no reader can ask a
//     node what it keeps. [keptRowSQL].
// One table, `custody_unsettled_rows`, serves both.

// UnsettledRow is one row this node holds of a custody batch it has written and
// not settled: its identity in the event log — `(Time, ID)`, the table's
// primary key, to the store's microsecond — and what each count adds it in by.
//
// ONE TYPE FOR EVERY COUNT, each reading the fields it needs (outcome counts:
// type and app; the axis: category and failed; a turn's sums: turn, type,
// tokens, duration; an extent: the identity alone). The fields are the columns
// the counts' own statements read, so a row is added back by the rule that
// counted it ([EventHistogram.Count], [TurnPartial.Add],
// [NotificationOutcomes.Add]). On the wire (internal/eventfan) every field but
// the identity is omitted when empty.
type UnsettledRow struct {
	Time time.Time `json:"time"`
	ID   string    `json:"id"`
	Type string    `json:"type,omitempty"`
	// App is the `notification_source` tag, trimmed, which the outcome
	// counts key on.
	App string `json:"app,omitempty"`

	Category string `json:"category,omitempty"`
	// Failed is [failedRow]'s rule over the row.
	Failed bool `json:"failed,omitempty"`

	TurnID       string `json:"turn_id,omitempty"`
	InputTokens  int    `json:"input_tokens,omitempty"`
	OutputTokens int    `json:"output_tokens,omitempty"`
	TotalTokens  int    `json:"total_tokens,omitempty"`
	CacheRead    int    `json:"cache_read_tokens,omitempty"`
	CacheWrite   int    `json:"cache_write_tokens,omitempty"`
	// DurationMS is a completion record's own measurement, and zero on every
	// other row — the term [TurnQuery.partialsSQL] sums.
	DurationMS int `json:"duration_ms,omitempty"`
}

// Key is the row's identity, comparable: the stored instant and the id.
func (r UnsettledRow) Key() RowKey { return RowKey{At: EncodeTime(r.Time), ID: r.ID} }

// Identity is the row with its identity alone — what [EventLog.KeptRows] is
// asked about, since whether a node keeps a row is a property of the row and
// not of what it counts as.
func (r UnsettledRow) Identity() UnsettledRow { return UnsettledRow{Time: r.Time, ID: r.ID} }

// RowKey is an event row's identity as the log stores it. Microseconds rather
// than the time.Time, because a time.Time carries a location and a monotonic
// reading and is not a safe map key.
type RowKey struct {
	At int64
	ID string
}

// querier is what a read needs of its connection: the pool for a read of one
// statement, or a transaction for a read that has to be ONE SNAPSHOT with
// another — a count and the rows it names unsettled.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// unsettledSQL is the rows of every unsettled custody batch this node wrote
// that `where` selects, and its arguments: every [UnsettledRow] column.
//
// `where` is a predicate over the event log with UNQUALIFIED columns, as every
// count writes it: the custody table shares no name but the identity, which
// the join equates, so one predicate counts the rows and narrows those named.
//
// DRIVEN FROM THE IN-FLIGHT IDENTITIES: each is a seek of the log by primary
// key, and CROSS JOIN fixes that order so the planner cannot walk the count's
// rows and probe the identities (TestTheCustodyReadsSeekTheLogByIdentity). One
// seek per identity in flight, whatever the window: a few normally, tens of
// thousands after a coordination outage, still far less than the broad counts.
func unsettledSQL(where []string, args []any) (string, []any) {
	failed, failedArgs := failedRow(func(name string) string { return "e." + name })
	query := `SELECT e.event_time, e.event_id, e.event_type, e.category,
	       CASE WHEN ` + failed + ` THEN 1 ELSE 0 END,
	       e.turn_id, e.input_tokens, e.output_tokens, e.total_tokens,
	       e.cache_read_tokens, e.cache_write_tokens,
	       CASE WHEN e.event_type = ? THEN COALESCE(json_extract(e.payload, '$.duration_ms'), 0) ELSE 0 END,
	       COALESCE(json_extract(e.tags, '$.notification_source'), '')
	  FROM custody_unsettled_rows AS u CROSS JOIN crewlet_events AS e
	 WHERE e.event_time = u.row_time
	   AND e.event_id = u.row_id`
	if len(where) > 0 {
		query += "\n\t   AND " + strings.Join(where, " AND ")
	}
	all := make([]any, 0, len(failedArgs)+1+len(args))
	all = append(all, failedArgs...)
	all = append(all, turnCompleted)
	return query, append(all, args...)
}

// readUnsettled runs [unsettledSQL] over `where` on one connection — the count's
// own transaction — and answers each row ONCE, though two batches on this node
// could name it: the count beside it holds the row once.
func readUnsettled(ctx context.Context, q querier, where []string, args []any) ([]UnsettledRow, error) {
	query, all := unsettledSQL(where, args)
	rows, err := q.QueryContext(ctx, query, all...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []UnsettledRow{}
	seen := map[RowKey]bool{}
	for rows.Next() {
		var (
			stamp, failed, duration int64
			r                       UnsettledRow
			tag                     string
		)
		if err := rows.Scan(&stamp, &r.ID, &r.Type, &r.Category, &failed, &r.TurnID,
			&r.InputTokens, &r.OutputTokens, &r.TotalTokens, &r.CacheRead, &r.CacheWrite,
			&duration, &tag); err != nil {
			return nil, err
		}
		r.Time, r.Failed, r.DurationMS = DecodeTime(stamp), failed != 0, int(duration)
		r.App = strings.TrimSpace(tag)
		if seen[r.Key()] {
			continue
		}
		seen[r.Key()] = true
		out = append(out, r)
	}
	return out, rows.Err()
}

// KeptRows answers which of some rows, named by identity, this node KEEPS:
// holds, and not as a row of an unsettled custody batch it wrote.
//
// The other half of every count's unsettled rows (see the file doc). A row one
// node names unsettled may be kept by another; one no node keeps is in no
// count. So whoever sums the fleet asks every data node this and counts a row
// once unless a keeper counted it already.
//
// ONE STATEMENT for rows held and batches unsettled (each named row a seek of
// the log, its in-flight key probed from it, [keptRowSQL]) gives one snapshot
// and costs the rows named, not the backlog. The answer is never nil; each row
// carries its identity alone.
func (l *EventLog) KeptRows(ctx context.Context, named []UnsettledRow) ([]UnsettledRow, error) {
	out := []UnsettledRow{}
	if len(named) == 0 {
		return out, nil
	}
	query, args, err := keptSQL(named)
	if err != nil {
		return nil, fmt.Errorf("store: read the kept rows: %w", err)
	}
	if err := l.db.Read(ctx, func(tx *sql.Tx) error {
		out = out[:0]
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var stamp int64
			var r UnsettledRow
			if err := rows.Scan(&stamp, &r.ID); err != nil {
				return err
			}
			r.Time = DecodeTime(stamp)
			out = append(out, r)
		}
		return rows.Err()
	}); err != nil {
		return nil, fmt.Errorf("store: read the kept rows: %w", err)
	}
	return out, nil
}

// keptRowSQL is a predicate over the event log under the alias `e` that holds
// for a row this node KEEPS: every row but those of an unsettled custody batch
// it wrote. It is how a PUBLISHED count obeys the file doc's rule.
//
// A SEEK OF THE IN-FLIGHT KEY FROM EACH ROW COUNTED, at the same cost for a few
// identities or fifty thousand: the `custody_unsettled_rows` key leads with the
// log's `(event_time, event_id)`, so the anti-join is a covering-index probe
// (TestEveryUsageCountSeeksItsWindow). An uncorrelated `NOT IN` over the
// batches' JSON checks every row against the whole set (2.4 s for a day of five
// thousand rows beside ten thousand identities in flight).
const keptRowSQL = `NOT EXISTS (SELECT 1 FROM custody_unsettled_rows AS u
	         WHERE u.row_time = e.event_time AND u.row_id = e.event_id)`

// keptSQL is the statement [EventLog.KeptRows] runs and its arguments: the
// named rows this log KEEPS — holds by its primary key, and not as a row of a
// batch in flight ([keptRowSQL]) — the names bound as ONE JSON array so the
// statement's variables do not grow with them. A function of its own so its
// plan can be read back (TestTheCustodyReadsSeekTheLogByIdentity).
func keptSQL(named []UnsettledRow) (string, []any, error) {
	ids := make([]rowIdentity, 0, len(named))
	for _, r := range named {
		ids = append(ids, rowIdentity{Time: EncodeTime(r.Time), ID: r.ID})
	}
	raw, err := json.Marshal(ids)
	if err != nil {
		return "", nil, err
	}
	// DRIVEN FROM THE NAMES, each a seek of the key: CROSS JOIN fixes the
	// order, so the planner cannot start from the log and probe the names
	// for every row it holds.
	return `SELECT e.event_time, e.event_id
	  FROM json_each(?) AS j CROSS JOIN crewlet_events AS e
	 WHERE e.event_time = json_extract(j.value, '$.t')
	   AND e.event_id = json_extract(j.value, '$.id')
	   AND ` + keptRowSQL, []any{string(raw)}, nil
}

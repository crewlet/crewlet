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
// Every row of this node's own is written once, here, by the node that
// published it — but a node without `data` hands its events to the data nodes
// in batches (internal/observe's custody), and a batch whose claim failed is
// written by a second keeper before the first has learned it is not its own to
// keep. Until that first node settles the batch, two logs hold the same row,
// and a fleet adding two nodes' counts counts it twice.
//
// So every read that COUNTS — the outcome counts, the axis's bars and facets, a
// trace's and a turn's extent, a turn's sums, a usage day — counts the rows
// this node KEEPS, and does one of two things with the rows it holds of a
// batch it has written and not settled ([EventLog.WriteCustody]), depending on
// who reads the count:
//
//   - A count that is ANSWERED — to a fleet read, whose asker sums every
//     data node's answer — names those rows beside the count, as
//     [UnsettledRow]s: by identity, with whatever each count needs to add the
//     row in. The asker asks every data node which of the named rows it keeps
//     ([EventLog.KeptRows]) and counts each once (internal/eventfan). The
//     named rows are read in the SAME SNAPSHOT as the count they are taken out
//     of: read apart, a batch settled between the two is counted as kept and
//     named as unsettled, or neither. [unsettledSQL] is the read.
//   - A count that is PUBLISHED — a usage day, summed across nodes by every
//     reader of the replicated history ([DB.UsageForDay]) — leaves them out
//     and names nothing, because no reader of a published day can ask a node
//     which of its rows it keeps: the day has to BE the rows it keeps.
//     [keptRowSQL] is the predicate.
//
// ONE TABLE AND ONE KEY for both: what makes a row unsettled is a property of
// the row and not of the question, so `custody_unsettled_rows` — one row per
// identity of a batch in flight, keyed on the log's own `(event_time,
// event_id)` — is the only place either shape finds it. [unsettledSQL] drives
// a seek of the log from each of its rows; [keptRowSQL] seeks it from each row
// counted, and [EventLog.KeptRows] from each row named. A count that took its own route
// to the custody tables is a count whose idea of "unsettled" could drift from
// its neighbours'.

// UnsettledRow is one row this node holds of a custody batch it has written and
// not settled: its identity in the event log — `(Time, ID)`, the table's
// primary key, to the store's microsecond — and what each count adds it in by.
//
// ONE TYPE FOR EVERY COUNT, each reading the fields it needs: the outcome
// counts read the type and the app, the axis the category and the failed flag,
// a turn's sums its turn, its type, its tokens and its duration, and a trace's
// or a turn's extent nothing but the identity. The fields are the columns the
// counts' own statements read, so a row is added back by the rule that counted
// it ([EventHistogram.Count], [TurnPartial.Add], [NotificationOutcomes.Add]).
//
// ON THE WIRE between nodes (internal/eventfan), where every field but the
// identity is omitted when empty: a count reads only the fields it adds by.
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

// unsettledSQL is the rows of every custody batch this node has written and not
// settled that `where` selects, and its arguments — every column an
// [UnsettledRow] carries.
//
// `where` is a predicate over the event log written with its columns
// UNQUALIFIED, the way every count's own statement writes it ([ListQuery]'s
// filters, [TurnQuery]'s row terms): each column it names is the log's alone,
// since the custody table has none of those names but the identity, which the
// join equates, so one predicate counts the rows and narrows the rows named.
//
// DRIVEN FROM THE IN-FLIGHT IDENTITIES: each is a seek of the log by its
// primary key, and CROSS JOIN fixes that order, so the planner cannot walk the
// count's rows and probe the identities for each
// (TestTheCustodyReadsSeekTheLogByIdentity). That costs one seek per identity
// in flight, whatever the count's own window — a few normally, and tens of
// thousands after a coordination outage leaves batches unsettled (about 12 ms
// per count beside fifty thousand, measured). Driven from the identities on
// purpose all the same: the broad counts — the axis over the thirty-day floor
// — would otherwise read every row they count to probe the key, which is far
// more than any backlog.
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
// holds in its log, and not as a row of a custody batch it has written and not
// settled.
//
// The other half of every count's unsettled rows (see the file doc). A row one
// node names unsettled may be kept by another — the batch's keeper, which
// settled first — and that node's count already holds it, while one no node
// keeps is in no count at all. So whoever sums the fleet asks every data node
// this about the rows named unsettled, and counts a row once unless a node that
// keeps it counted it already. Only the identity of each row is read. ONE
// STATEMENT for the rows held and the batches unsettled — each named row a seek
// of the log, and its in-flight key probed from it ([keptRowSQL]) — so the two
// are one snapshot, for the counts' own reason, and the cost is the rows named
// rather than the backlog in flight: read whole into a set, as it was, the
// in-flight table cost every fleet read about 170 ms beside the fifty thousand
// identities a coordination outage leaves. The answer is never nil, and each
// row in it carries its identity alone.
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

// keptRowSQL is a predicate over the event log under the alias `e`, which
// every usage statement gives it, that holds for a row this node KEEPS: every
// row it holds but those of a custody batch it has written and not settled.
//
// It is how a count that is PUBLISHED rather than answered obeys the rule the
// file doc states. Counted as held, a batch two data nodes wrote before either
// settled it was in both nodes' days, and once the loser's copy was deleted the
// double count stayed in the history of any day the publisher no longer
// re-derived.
//
// A SEEK OF THE IN-FLIGHT KEY FROM EACH ROW COUNTED, which costs the same
// whether a few identities are in flight or fifty thousand: the
// `custody_unsettled_rows` key leads with the log's own `(event_time,
// event_id)`, so the anti-join is a covering-index probe per row
// (TestEveryUsageCountSeeksItsWindow). The obvious alternative — an
// uncorrelated `NOT IN` over the batches' JSON — is read once and then checks
// every counted row against the whole of it, measured at 2.4 s for one
// fingerprint of a five-thousand-row day beside ten thousand identities in
// flight, which a coordination store away for a few minutes leaves behind.
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

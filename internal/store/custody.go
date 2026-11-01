package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Custody: a data node writing a stateless node's events into its own log,
// and remembering which of those batches it has not yet learned it keeps
// (ADR-0025). Which node keeps a batch is decided in coordination; what lives
// here is this node's half — the rows, and the batches it still has to ask
// about. See internal/observe's Keeper for the protocol.

// CustodyBatch is a batch of another node's events this node writes into its
// own event log.
type CustodyBatch struct {
	// ID is the batch's identity — the carrier event's id, which is what
	// the fleet decides custody of.
	ID string
	// Origin is the node that published the events.
	Origin string
	// Records are the batch's rows.
	Records []EventRecord
}

// UnsettledBatch is a batch this node wrote and has not settled.
type UnsettledBatch struct {
	ID        string
	Origin    string
	WrittenAt time.Time
}

// rowIdentity is one row of a batch by the event log's own (time, id) key, as
// stored — the shape [keptSQL] binds a list of names in.
type rowIdentity struct {
	Time int64  `json:"t"`
	ID   string `json:"id"`
}

// WriteCustody writes a batch's rows into the event log and records the batch
// unsettled, in ONE transaction.
//
// One transaction because the two halves are only true together: rows with no
// unsettled record are a copy nothing will ever ask the fleet about — a second
// copy for good if another node keeps the batch — and a record with no rows
// would make a release delete nothing it had written. A batch written before
// and not yet settled is written again harmlessly: each row's append is
// idempotent, and the record keeps the instant it was first written.
//
// A BATCH THIS NODE HAS ALREADY SETTLED IS WRITTEN NOT AT ALL. A group
// redelivers a batch whose acknowledgement was lost to whichever member asks
// next, which can be this node after its settle: written again, the batch
// would be in flight again, and its kept rows — counted and published — would
// leave every usage count until the claim answered a second time, so a day
// would be published short over good history in the meantime. Kept, its rows are here and
// counting; not kept, the fleet's copy is another node's. The settle that
// follows finds nothing unsettled and settles to nothing.
//
// Every record is checked before anything is written, and one that cannot be
// stored refuses the batch: a batch partly written and recorded as whole would
// be released incompletely.
func (l *EventLog) WriteCustody(ctx context.Context, b CustodyBatch, at time.Time) error {
	if b.ID == "" || b.Origin == "" {
		return errors.New("store: a custody batch needs an id and an origin")
	}
	rows := make([]eventRow, 0, len(b.Records))
	for _, rec := range b.Records {
		row, err := prepareEvent(rec, l.db.eventSeatsNow())
		if err != nil {
			return fmt.Errorf("store: custody batch %s: %w", b.ID, err)
		}
		rows = append(rows, row)
	}
	if err := l.db.Tx(ctx, func(tx *sql.Tx) error {
		var settled int
		switch err := tx.QueryRowContext(ctx,
			`SELECT 1 FROM custody_settled WHERE batch_id = ?`, b.ID).Scan(&settled); {
		case err == nil:
			return nil
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("read whether it was settled: %w", err)
		}
		for _, row := range rows {
			if err := row.insert(ctx, tx); err != nil {
				return fmt.Errorf("event %s: %w", row.rec.ID, err)
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO custody_unsettled_rows (row_time, row_id, batch_id)
				VALUES (?, ?, ?) ON CONFLICT DO NOTHING`,
				EncodeTime(row.rec.Time), row.rec.ID, b.ID); err != nil {
				return fmt.Errorf("event %s: record it in flight: %w", row.rec.ID, err)
			}
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO custody_unsettled (batch_id, origin, written_at)
			VALUES (?, ?, ?)
			ON CONFLICT (batch_id) DO NOTHING`,
			b.ID, b.Origin, EncodeTime(at.UTC()))
		return err
	}); err != nil {
		return fmt.Errorf("store: write custody batch %s: %w", b.ID, err)
	}
	return nil
}

// SettleCustody records what the fleet decided about a batch this node wrote,
// in one transaction: kept, the rows stay; not kept, its rows go with it.
// Either way the batch leaves the unsettled record and is remembered as
// settled ([EventLog.WriteCustody] says why). A batch this node holds no
// unsettled record of settles to nothing and records nothing: either it was
// settled already, and the earlier settle remembered it, or it was never
// written here.
//
// KEPT, ITS ROWS START TO COUNT — every usage count leaves out a batch in
// flight (usage.go) — so the days they fall on are not the days this node
// last published, and any of them may be older than the two its usage
// publisher looks at on every tick: a batch can carry a node's events from a
// partition that lasted days. So the settle marks the oldest usage row it made
// count ([DB.UsageRederive]) IN THIS TRANSACTION: written in a transaction of
// its own, a crash between the two would leave either rows counting on a day
// nothing will derive again or a mark for rows that do not count. Not kept,
// its rows were never counted, so no count changes and nothing is marked — a
// fire whose trace only these rows named loses its turn label
// ([DB.usageSchedules]), which no count is made of.
func (l *EventLog) SettleCustody(ctx context.Context, batchID string, kept bool) error {
	if err := l.db.Tx(ctx, func(tx *sql.Tx) error {
		var held int
		read := tx.QueryRowContext(ctx,
			`SELECT 1 FROM custody_unsettled WHERE batch_id = ?`, batchID).Scan(&held)
		switch {
		case errors.Is(read, sql.ErrNoRows):
			return nil
		case read != nil:
			return read
		}
		if kept {
			if err := markUsageRederive(ctx, tx, batchID); err != nil {
				return err
			}
		} else if err := releaseCustody(ctx, tx, batchID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM custody_unsettled_rows WHERE batch_id = ?`, batchID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM custody_unsettled WHERE batch_id = ?`, batchID); err != nil {
			return err
		}
		keptFlag := 0
		if kept {
			keptFlag = 1
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO custody_settled (batch_id, kept, settled_at) VALUES (?, ?, ?)
			ON CONFLICT (batch_id) DO UPDATE SET kept = excluded.kept,
			                                     settled_at = excluded.settled_at`,
			batchID, keptFlag, EncodeTime(now()))
		return err
	}); err != nil {
		return fmt.Errorf("store: settle custody batch %s: %w", batchID, err)
	}
	return nil
}

// releaseCustody deletes the rows of a batch another node keeps, each by the
// log's own key, read off the batch's in-flight identities.
func releaseCustody(ctx context.Context, tx *sql.Tx, batchID string) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT row_time, row_id FROM custody_unsettled_rows WHERE batch_id = ?`, batchID)
	if err != nil {
		return fmt.Errorf("read its rows: %w", err)
	}
	var ids []rowIdentity
	for rows.Next() {
		var id rowIdentity
		if err := rows.Scan(&id.Time, &id.ID); err != nil {
			_ = rows.Close()
			return fmt.Errorf("read its rows: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("read its rows: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("read its rows: %w", err)
	}
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `DELETE FROM crewlet_event_parties
			WHERE event_time = ? AND event_id = ?`, id.Time, id.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM crewlet_events
			WHERE event_time = ? AND event_id = ?`, id.Time, id.ID); err != nil {
			return err
		}
	}
	return nil
}

// UnsettledCustody lists up to limit batches written before `before` and not
// settled, oldest first — by (written_at, batch_id) — and AFTER `after` in
// that order: the zero [UnsettledBatch] starts at the oldest, and the last
// batch of one page starts the next. Only after's WrittenAt and ID are read.
//
// A KEYSET, never an offset and never "the oldest again": a caller settling
// what it lists leaves some batches as they are — one whose keeper cannot be
// decided (coord.ErrCustodyUndecidable) stays unsettled for as long as the
// event log keeps its rows — and listed from the oldest every time, the first
// page of those would be every page there is, so a pass never reached a batch
// behind them. An offset would still count the batches a settle has removed
// since, and skip as many of the ones still there.
func (l *EventLog) UnsettledCustody(ctx context.Context, before time.Time, after UnsettledBatch,
	limit int) ([]UnsettledBatch, error) {

	query := `SELECT batch_id, origin, written_at FROM custody_unsettled WHERE written_at < ?`
	args := []any{EncodeTime(before.UTC())}
	if after.ID != "" {
		at := EncodeTime(after.WrittenAt)
		query += ` AND (written_at > ? OR (written_at = ? AND batch_id > ?))`
		args = append(args, at, at, after.ID)
	}
	query += ` ORDER BY written_at, batch_id LIMIT ?`
	args = append(args, limit)
	rows, err := l.db.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list unsettled custody: %w", err)
	}
	defer rows.Close()
	out := []UnsettledBatch{}
	for rows.Next() {
		var b UnsettledBatch
		var written int64
		if err := rows.Scan(&b.ID, &b.Origin, &written); err != nil {
			return nil, fmt.Errorf("store: list unsettled custody: %w", err)
		}
		b.WrittenAt = DecodeTime(written)
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list unsettled custody: %w", err)
	}
	return out, nil
}

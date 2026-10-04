package store

import (
	"context"
	"database/sql"
	"encoding/json"
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

// rowIdentity is one row of a batch as the unsettled record names it: the
// event log's own (time, id) key, as stored.
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
// is written again harmlessly: each row's append is idempotent, and the
// record keeps the instant it was first written.
//
// Every record is checked before anything is written, and one that cannot be
// stored refuses the batch: a batch partly written and recorded as whole would
// be released incompletely.
func (l *EventLog) WriteCustody(ctx context.Context, b CustodyBatch, at time.Time) error {
	if b.ID == "" || b.Origin == "" {
		return errors.New("store: a custody batch needs an id and an origin")
	}
	rows := make([]eventRow, 0, len(b.Records))
	ids := make([]rowIdentity, 0, len(b.Records))
	for _, rec := range b.Records {
		row, err := prepareEvent(rec)
		if err != nil {
			return fmt.Errorf("store: custody batch %s: %w", b.ID, err)
		}
		rows = append(rows, row)
		ids = append(ids, rowIdentity{Time: EncodeTime(rec.Time), ID: rec.ID})
	}
	events, err := json.Marshal(ids)
	if err != nil {
		return fmt.Errorf("store: encode custody batch %s: %w", b.ID, err)
	}
	if err := l.db.Tx(ctx, func(tx *sql.Tx) error {
		for _, row := range rows {
			if err := row.insert(ctx, tx); err != nil {
				return fmt.Errorf("event %s: %w", row.rec.ID, err)
			}
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO custody_unsettled (batch_id, origin, written_at, events)
			VALUES (?, ?, ?, ?)
			ON CONFLICT (batch_id) DO NOTHING`,
			b.ID, b.Origin, EncodeTime(at.UTC()), string(events))
		return err
	}); err != nil {
		return fmt.Errorf("store: write custody batch %s: %w", b.ID, err)
	}
	return nil
}

// SettleCustody records what the fleet decided about a batch this node wrote,
// in one transaction: kept, the rows stay and the batch is forgotten; not
// kept, its rows go with it. A batch this node holds no record of settles to
// nothing — it was settled already, or never written here.
func (l *EventLog) SettleCustody(ctx context.Context, batchID string, kept bool) error {
	if err := l.db.Tx(ctx, func(tx *sql.Tx) error {
		var raw string
		read := tx.QueryRowContext(ctx,
			`SELECT events FROM custody_unsettled WHERE batch_id = ?`, batchID).Scan(&raw)
		switch {
		case errors.Is(read, sql.ErrNoRows):
			return nil
		case read != nil:
			return read
		}
		if !kept {
			var ids []rowIdentity
			if err := json.Unmarshal([]byte(raw), &ids); err != nil {
				return fmt.Errorf("decode its rows: %w", err)
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
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM custody_unsettled WHERE batch_id = ?`, batchID)
		return err
	}); err != nil {
		return fmt.Errorf("store: settle custody batch %s: %w", batchID, err)
	}
	return nil
}

// UnsettledCustody lists up to limit batches written before `before` and not
// settled, oldest first.
func (l *EventLog) UnsettledCustody(ctx context.Context, before time.Time, limit int) ([]UnsettledBatch, error) {
	rows, err := l.db.sql.QueryContext(ctx, `
		SELECT batch_id, origin, written_at FROM custody_unsettled
		 WHERE written_at < ?
		 ORDER BY written_at, batch_id
		 LIMIT ?`, EncodeTime(before.UTC()), limit)
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

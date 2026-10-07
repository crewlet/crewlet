package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// What this node's usage publisher holds back: a person's day it derived and
// may not publish yet, because a node applying the usage log runs a build that
// cannot read the record (internal/usage's publisher). Kept here, in the node's
// own file, because the publisher never derives a day older than yesterday
// again — held in memory alone, a restart during a rolling upgrade lost every
// such day for good. See node migration `a_node_keeps_the_person_days_it_holds`.

// UsageHeld is one held record: the company day it is for, the object's
// subject as the publisher names it, and the encoded record.
type UsageHeld struct {
	Day     string
	Subject string
	Record  []byte
}

// HoldUsage keeps a record held, replacing what was held for its object.
func (d *DB) HoldUsage(ctx context.Context, h UsageHeld, at time.Time) error {
	if d == nil || d.sql == nil {
		return ErrNoEstate
	}
	if h.Day == "" || h.Subject == "" || len(h.Record) == 0 {
		return fmt.Errorf("store: a held usage record needs its day, its subject and " +
			"its bytes")
	}
	if err := d.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO usage_held (day, subject, record, held_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT (day, subject) DO UPDATE SET
			    record = excluded.record, held_at = excluded.held_at`,
			h.Day, h.Subject, h.Record, EncodeTime(at.UTC()))
		return err
	}); err != nil {
		return fmt.Errorf("store: hold the usage record on %s for %s: %w", h.Subject, h.Day, err)
	}
	return nil
}

// HeldUsage is every record held, by day and subject.
func (d *DB) HeldUsage(ctx context.Context) ([]UsageHeld, error) {
	if d == nil || d.sql == nil {
		return nil, ErrNoEstate
	}
	rows, err := d.sql.QueryContext(ctx,
		`SELECT day, subject, record FROM usage_held ORDER BY day, subject`)
	if err != nil {
		return nil, fmt.Errorf("store: list the held usage records: %w", err)
	}
	defer rows.Close()
	out := []UsageHeld{}
	for rows.Next() {
		var h UsageHeld
		if err := rows.Scan(&h.Day, &h.Subject, &h.Record); err != nil {
			return nil, fmt.Errorf("store: list the held usage records: %w", err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list the held usage records: %w", err)
	}
	return out, nil
}

// ReleaseUsage forgets a held record — published, or past the history.
// Releasing one that is not held does nothing.
func (d *DB) ReleaseUsage(ctx context.Context, day, subject string) error {
	if d == nil || d.sql == nil {
		return ErrNoEstate
	}
	if err := d.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`DELETE FROM usage_held WHERE day = ? AND subject = ?`, day, subject)
		return err
	}); err != nil {
		return fmt.Errorf("store: release the usage record on %s for %s: %w", subject, day, err)
	}
	return nil
}

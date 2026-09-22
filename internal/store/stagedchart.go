package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// THE CHART AN OFFLINE IMPORT LEFT FOR THE NEXT BOOT TO PUBLISH.
//
// See the migration for why it exists and why it is sealed. What this file
// adds is the one rule the table cannot state: a stage is TAKEN rather than
// read, in a single transaction, because the alternative is a boot that
// publishes it and crashes before deleting the row — and the next boot
// publishes it again.
//
// The ledger makes the second publish a no-op on every node, so the cost of
// getting this wrong is a record rather than a wrong company. It is still
// worth the transaction: a record on the structure's own subject is the one
// every other structural write in the company serialises behind.

// StagedChart is one chart waiting to be published.
type StagedChart struct {
	// ID is the chart's own content hash, which is also the key the
	// import ledger collapses a second landing on.
	ID string

	// Payload is the SEALED, encoded chart.Authored value. This package
	// neither opens nor interprets it: the cipher is Tier A's and the
	// shape is the chart domain's.
	Payload []byte

	SourcePath string
	StagedAt   time.Time
	StagedBy   string
}

// StagedCharts is the staging table, backed by this database.
type StagedCharts struct{ db *DB }

// StagedCharts returns it.
func (d *DB) StagedCharts() *StagedCharts { return &StagedCharts{db: d} }

// Stage records a chart for the next boot to publish, replacing whatever was
// staged before.
//
// REPLACING, because a stage is a PENDING INTENT rather than a history: two
// offline imports in a row mean the operator changed their mind, and keeping
// both would publish a structure they have already abandoned.
func (s *StagedCharts) Stage(ctx context.Context, in StagedChart) error {
	if in.ID == "" {
		return errors.New("store: a staged chart needs its content key")
	}
	if len(in.Payload) == 0 {
		return errors.New("store: a staged chart with no payload would publish nothing")
	}
	at := in.StagedAt
	if at.IsZero() {
		at = time.Now().UTC()
	}
	_, err := s.db.sql.ExecContext(ctx, `
		DELETE FROM chart_import_staged`)
	if err != nil {
		return fmt.Errorf("store: clear the staged chart: %w", err)
	}
	_, err = s.db.sql.ExecContext(ctx, `
		INSERT INTO chart_import_staged (id, payload, source_path, staged_at, staged_by)
		VALUES (?, ?, ?, ?, ?)`,
		in.ID, in.Payload, in.SourcePath, at.UTC().UnixMilli(), in.StagedBy)
	if err != nil {
		return fmt.Errorf("store: stage the chart: %w", err)
	}
	return nil
}

// Take removes the staged chart and returns it, reporting whether there was
// one.
//
// ONE TRANSACTION, which is the whole reason this is not a read and a delete:
// a boot that published and then crashed before deleting would publish again
// on the next one. The ledger collapses that on every node, so what this buys
// is one fewer record on the subject every structural write serialises behind
// — which is worth a transaction and not worth a second mechanism.
func (s *StagedCharts) Take(ctx context.Context) (StagedChart, bool, error) {
	tx, err := s.db.sql.BeginTx(ctx, nil)
	if err != nil {
		return StagedChart{}, false, fmt.Errorf("store: take the staged chart: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var out StagedChart
	var at int64
	err = tx.QueryRowContext(ctx, `
		SELECT id, payload, source_path, staged_at, staged_by
		FROM chart_import_staged LIMIT 1`).
		Scan(&out.ID, &out.Payload, &out.SourcePath, &at, &out.StagedBy)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return StagedChart{}, false, nil
	case err != nil:
		return StagedChart{}, false, fmt.Errorf("store: read the staged chart: %w", err)
	}
	out.StagedAt = time.UnixMilli(at).UTC()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM chart_import_staged WHERE id = ?`, out.ID); err != nil {
		return StagedChart{}, false, fmt.Errorf("store: clear the staged chart: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return StagedChart{}, false, fmt.Errorf("store: take the staged chart: %w", err)
	}
	return out, true, nil
}

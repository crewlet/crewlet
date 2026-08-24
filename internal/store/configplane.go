package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/configplane"
)

// MaxApplyErrorLength bounds the failure text a node records.
//
// A wrapped error chain from a failed apply is a sentence or two; a driver's
// own message with a query in it can be kilobytes, and this column is read
// back by every peer on every reconcile tick AND rendered in the fleet view.
// 2000 characters keeps a real diagnosis intact and stops one node's stack
// trace becoming a download for everyone else.
const MaxApplyErrorLength = 2000

// Activation is one entry of the pointer every node converges on.
type Activation struct {
	Epoch      int64
	RevisionID string
	At         time.Time
	Summary    string
}

// NodeApply is one node's last word about an epoch.
type NodeApply struct {
	NodeID     string
	Epoch      int64
	RevisionID string
	Status     configplane.ApplyStatus
	Error      string
	UpdatedAt  time.Time
}

// ConfigPlane is the activation log plus the per-node apply status.
type ConfigPlane struct{ db *DB }

// ControlPlane returns the control plane backed by this database.
func (d *DB) ControlPlane() *ConfigPlane { return &ConfigPlane{db: d} }

// ActivationInsertSQL is the append that moves the pointer.
//
// Exported because activation is ONE transaction: the company_config row flips
// active and the pointer advances together, or neither happens. A caller
// already inside that transaction runs this statement rather than calling
// RecordActivation, which would open a second one — and a pointer that moved
// without the row, or a row that activated without the pointer, is a fleet
// converging on a revision nobody can read.
const ActivationInsertSQL = `
INSERT INTO config_activations (revision_id, activated_at, summary)
VALUES (?, ?, ?)`

// RecordActivation appends an activation and returns its epoch.
//
// For callers not already inside an activation transaction — a test, a
// migration, an operator tool. The activation path itself uses
// [ActivationInsertSQL] on its own transaction.
func (c *ConfigPlane) RecordActivation(ctx context.Context, revisionID, summary string, at time.Time) (int64, error) {
	res, err := c.db.sql.ExecContext(ctx, ActivationInsertSQL,
		revisionID, EncodeTime(at), summary)
	if err != nil {
		return 0, fmt.Errorf("store: record activation of %s: %w", revisionID, err)
	}
	epoch, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: record activation of %s: epoch: %w", revisionID, err)
	}
	return epoch, nil
}

// Target is the activation every node should converge on, and whether there is
// one at all.
//
// The absence is a real state and distinct from an error: a fresh deployment
// has no activation, which is what "unconfigured" means. Collapsing the two
// would make a database outage look like a company nobody had configured yet.
func (c *ConfigPlane) Target(ctx context.Context) (Activation, bool, error) {
	row := c.db.sql.QueryRowContext(ctx,
		`SELECT epoch, revision_id, activated_at, summary
		 FROM config_activations ORDER BY epoch DESC LIMIT 1`)
	var a Activation
	var micros int64
	switch err := row.Scan(&a.Epoch, &a.RevisionID, &micros, &a.Summary); {
	case errors.Is(err, sql.ErrNoRows):
		return Activation{}, false, nil
	case err != nil:
		return Activation{}, false, fmt.Errorf("store: read activation pointer: %w", err)
	}
	a.At = DecodeTime(micros)
	return a, true, nil
}

// RecordApply writes this node's outcome for an epoch, replacing its previous
// one.
//
// An upsert on node_id rather than an append: this is a node's LAST WORD, and
// a history of every apply a long-lived node ever made is a table that grows
// without bound to answer a question nobody asks.
func (c *ConfigPlane) RecordApply(ctx context.Context, a NodeApply) error {
	if a.NodeID == "" {
		return fmt.Errorf("%w: apply status with no node id", ErrIncompleteRecord)
	}
	if a.UpdatedAt.IsZero() {
		return fmt.Errorf("%w: apply status for %s has no timestamp",
			ErrIncompleteRecord, a.NodeID)
	}
	message := a.Error
	if len(message) > MaxApplyErrorLength {
		message = message[:MaxApplyErrorLength]
	}
	if _, err := c.db.sql.ExecContext(ctx,
		`INSERT INTO config_apply_status
		     (node_id, epoch, revision_id, status, error, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT (node_id) DO UPDATE SET
		     epoch = excluded.epoch, revision_id = excluded.revision_id,
		     status = excluded.status, error = excluded.error,
		     updated_at = excluded.updated_at`,
		a.NodeID, a.Epoch, a.RevisionID, string(a.Status), message,
		EncodeTime(a.UpdatedAt)); err != nil {
		return fmt.Errorf("store: record apply for %s: %w", a.NodeID, err)
	}
	return nil
}

// PeerHealth counts how many OTHER nodes reported this epoch, and how many of
// those applied it cleanly.
//
// Bounded on freshness, and the bound is load-bearing rather than tidy. A row
// here is a peer's last word, not a liveness signal: a node that was scaled in,
// redeployed or crashed leaves its `ok` behind forever. Counting that ghost
// makes a diverged survivor believe a healthy peer exists, so it SHEDS its
// seats to a node that no longer exists — the company goes dark exactly where
// it should have gone degraded and raised an alarm.
//
// A peer that reported `degraded` counts as reported but NOT as ok: its
// rollback did not restore what it tore down, so it is not somewhere work can
// safely go.
func (c *ConfigPlane) PeerHealth(ctx context.Context, epoch int64, selfNode string, since time.Time) (ok, reported int, err error) {
	row := c.db.sql.QueryRowContext(ctx,
		`SELECT
		     COALESCE(SUM(CASE WHEN status = 'ok' THEN 1 ELSE 0 END), 0),
		     COUNT(*)
		 FROM config_apply_status
		 WHERE epoch = ? AND node_id <> ? AND updated_at > ?`,
		epoch, selfNode, EncodeTime(since))
	if err := row.Scan(&ok, &reported); err != nil {
		return 0, 0, fmt.Errorf("store: read peer health for epoch %d: %w", epoch, err)
	}
	return ok, reported, nil
}

// Fleet is every node's last reported status — the operator view.
//
// Ordered by node id so two readers of the same fleet see the same page. The
// tiebreak is total because the id is the primary key.
func (c *ConfigPlane) Fleet(ctx context.Context) ([]NodeApply, error) {
	rows, err := c.db.sql.QueryContext(ctx,
		`SELECT node_id, epoch, revision_id, status, error, updated_at
		 FROM config_apply_status ORDER BY node_id`)
	if err != nil {
		return nil, fmt.Errorf("store: read fleet: %w", err)
	}
	defer rows.Close()

	var out []NodeApply
	for rows.Next() {
		var a NodeApply
		var status string
		var micros int64
		if err := rows.Scan(&a.NodeID, &a.Epoch, &a.RevisionID, &status,
			&a.Error, &micros); err != nil {
			return nil, fmt.Errorf("store: read fleet: %w", err)
		}
		a.Status = configplane.ApplyStatus(status)
		a.UpdatedAt = DecodeTime(micros)
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read fleet: %w", err)
	}
	return out, nil
}

// Purge drops rows for nodes that stopped reporting before cutoff.
//
// Garbage collection, not expiry — PeerHealth already stops a stale row
// affecting a decision within a minute. This is for a table that otherwise
// grows by one row per node id that has ever run, which under generated pod
// names is one row per deploy.
func (c *ConfigPlane) Purge(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := c.db.sql.ExecContext(ctx,
		`DELETE FROM config_apply_status WHERE updated_at < ?`, EncodeTime(cutoff))
	if err != nil {
		return 0, fmt.Errorf("store: purge apply status: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: purge apply status: rows affected: %w", err)
	}
	return n, nil
}

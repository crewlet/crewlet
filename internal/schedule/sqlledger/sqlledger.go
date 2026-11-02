// Package sqlledger is the durable [schedule.Ledger] over the `scheduled_runs`
// table: this node's audit record of what its scheduler dispatched.
//
// The fleet's at-most-once claim is the coordination store's (see
// [schedule.SharedClaimer]); what this table is for is the record. The usage
// domain derives every schedule's history from it (store.DB.UsageForDay), so a
// fire this node sent reaches every data node, joined with what became of it.
// Nothing reads the table through this package: a claim and a purge are the
// whole of what it does.
//
// It sits beside the scheduler rather than in internal/store because the SQL
// is what the LEDGER means, not what the store does — and because keeping it
// here leaves internal/schedule itself free of a database driver. Package
// schedule declares the interface, this package satisfies it, and both answer
// to internal/schedule/scheduletest.
package sqlledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/schedule"
	"github.com/crewlet/crewlet/internal/store"
)

// DB is the database surface this ledger needs: two statements, no
// transaction management, no schema.
//
// Narrow and consumer-defined so the ledger can be handed a *sql.DB, a
// *sql.Tx or a *sql.Conn without any of them having to know about it, and so
// nothing here can reach past the one table it owns.
type DB interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// Ledger is the `scheduled_runs` dispatch ledger.
type Ledger struct{ db DB }

// New returns a ledger over db. The schema is the store's (0004_runtime.sql);
// this package neither creates nor migrates it, so a caller that has not
// opened a migrated store gets an error from the first statement rather than a
// table quietly conjured with the wrong shape.
func New(db DB) *Ledger { return &Ledger{db: db} }

var _ schedule.Ledger = (*Ledger)(nil)

// ErrNoDB reports a ledger constructed without a database. It exists so the
// failure names itself rather than arriving as a nil dereference three frames
// into a tick.
var ErrNoDB = errors.New("sqlledger: no database")

const claimSQL = `
INSERT INTO scheduled_runs (
	scope_type, scope_id, schedule_name, fire_label, target_handle,
	scheduled_at, fired_at, outcome, trace_id
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (scope_type, scope_id, schedule_name, fire_label, target_handle) DO NOTHING`

// Claim atomically records a fire, reporting whether this call wrote the row.
//
// The whole at-most-once guarantee is these two lines of SQL: a composite
// PRIMARY KEY over the identity, and DO NOTHING on conflict. The conflict
// target is named rather than left bare so the statement says WHICH constraint
// it expects to lose to — a bare DO NOTHING would also swallow a future
// constraint on this table, and swallowing an unexpected violation is how a
// row silently stops being written.
//
// Reporting rests on RowsAffected rather than on RETURNING. That began as a
// dialect-intersection rule for two drivers, and the second driver is gone;
// it stays because RowsAffected is what this statement needs and is the answer
// database/sql gives whatever the driver does, so the ledger's tri-state does
// not depend on a newer SQLite feature reaching the one that is pinned.
func (l *Ledger) Claim(ctx context.Context, run schedule.Run) (bool, error) {
	if l == nil || l.db == nil {
		return false, ErrNoDB
	}
	run = schedule.Stamped(run)
	res, err := l.db.ExecContext(ctx, claimSQL,
		string(run.Scope), run.ScopeID, run.ScheduleName, run.FireLabel, run.TargetHandle,
		store.EncodeTime(run.ScheduledAt), store.EncodeTime(run.FiredAt),
		string(run.Outcome), run.TraceID,
	)
	if err != nil {
		// Unknown, never a refusal. A caller reading this as "already
		// claimed" would drop the fire for good; reading it as unknown
		// costs one tick.
		return false, fmt.Errorf("sqlledger: claim %s/%s/%s: %w",
			run.ScopeID, run.ScheduleName, run.FireLabel, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("sqlledger: claim %s/%s/%s: rows affected: %w",
			run.ScopeID, run.ScheduleName, run.FireLabel, err)
	}
	return n > 0, nil
}

const purgeSQL = `DELETE FROM scheduled_runs WHERE fired_at < ?`

// Purge drops rows fired strictly before the cutoff, returning how many went.
//
// Deleting the row deletes the claim, because in this backend they are the
// same row — which is the property a twin has to reproduce by hand, and the
// one a twin most easily gets wrong.
func (l *Ledger) Purge(ctx context.Context, before time.Time) (int, error) {
	if l == nil || l.db == nil {
		return 0, ErrNoDB
	}
	res, err := l.db.ExecContext(ctx, purgeSQL, store.EncodeTime(before))
	if err != nil {
		return 0, fmt.Errorf("sqlledger: purge: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("sqlledger: purge: rows affected: %w", err)
	}
	return int(n), nil
}

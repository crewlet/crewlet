// Package sqlledger is the durable [schedule.Ledger] over the `scheduled_runs`
// table.
//
// It is what makes at-most-once survive a restart, and the reason scheduling
// requires a store at all: the in-memory twin's claim lives in one process, so
// a fleet running it would fire every schedule once per node, and a restart
// would re-fire whatever the catchup window still covers.
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

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/schedule"
	"github.com/crewlet/crewlet/internal/store"
)

// DB is the database surface this ledger needs — three statements, no
// transaction management, no schema.
//
// Narrow and consumer-defined so the ledger can be handed a *sql.DB, a
// *sql.Tx or a *sql.Conn without any of them having to know about it, and so
// nothing here can reach past the one table it owns.
type DB interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
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
// dialect-intersection rule for two drivers (d-002, retired by d-003); it
// stays because RowsAffected is what this statement needs and is the answer
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

// recentSQL orders by fired_at descending and then by the identity tuple.
//
// The tiebreak is not decoration: two rows can share a microsecond, and
// without a total order this backend and the memory twin hand a dashboard
// different pages for the same data. Both certified drivers compare TEXT
// byte-wise by default, which is the same order Go's cmp.Compare gives the
// twin — so the two agree without either of them naming a collation.
const recentSQL = `
SELECT scope_type, scope_id, schedule_name, fire_label, target_handle,
       scheduled_at, fired_at, outcome, trace_id
FROM scheduled_runs
ORDER BY fired_at DESC, scope_type, scope_id, schedule_name, fire_label, target_handle
LIMIT ?`

// Recent returns the newest rows first, at most limit of them.
func (l *Ledger) Recent(ctx context.Context, limit int) ([]schedule.Run, error) {
	if l == nil || l.db == nil {
		return nil, ErrNoDB
	}
	if limit <= 0 {
		// Not passed to SQL: SQLite reads a negative LIMIT as unbounded,
		// so a caller whose page size arrived as -1 would pull the whole
		// table. The contract says a non-positive limit returns nothing.
		return nil, nil
	}
	rows, err := l.db.QueryContext(ctx, recentSQL, limit)
	if err != nil {
		return nil, fmt.Errorf("sqlledger: recent: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []schedule.Run
	for rows.Next() {
		var (
			run         schedule.Run
			scope       string
			outcome     string
			scheduledAt int64
			firedAt     int64
		)
		if err := rows.Scan(&scope, &run.ScopeID, &run.ScheduleName, &run.FireLabel,
			&run.TargetHandle, &scheduledAt, &firedAt, &outcome, &run.TraceID); err != nil {
			return nil, fmt.Errorf("sqlledger: recent: scan: %w", err)
		}
		run.Scope = types.ScheduleScope(scope)
		run.Outcome = schedule.Outcome(outcome)
		run.ScheduledAt = store.DecodeTime(scheduledAt)
		run.FiredAt = store.DecodeTime(firedAt)
		out = append(out, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlledger: recent: %w", err)
	}
	return out, nil
}

const purgeSQL = `DELETE FROM scheduled_runs WHERE fired_at < ?`

// Purge drops rows fired strictly before the cutoff, returning how many went.
//
// Deleting the row deletes the claim, because in this backend they are the
// same row — which is the property the twin has to reproduce by hand and the
// Python twin got wrong.
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

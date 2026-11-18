package worklet

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/maintenance"
	"github.com/crewlet/crewlet/internal/store"
)

// The inbox retention sweep: it ages out the per-recipient rows of
// `tracker_notifications` past the company's `inbox.retention_days`. A notice
// is a pointer at a history row; only the mailbox entry ages out, and the
// history it points at is untouched.
//
// It is [maintenance.NodeLocal] because the table is [statelog.Divergent]:
// outside the identity claim, so two nodes on different epochs legitimately
// hold different rows and deleting on this node's own authority breaks
// nothing. A fleet singleton would sweep one node and let the table grow on
// every other.

// InboxJobs is the sweep for a person's inbox rows, over the replicated estate
// db: the zero handle is a node with no tracker to sweep, which contributes no
// job rather than one that fails every tick.
//
// The applier declines to write an inbox row older than the horizon, read off
// the epoch at every batch, and [purgeInbox] deletes exactly those rows: the
// two must hold ONE horizon, or an inbox grows back on every reanchor. An
// apply moves `inbox.retention_days` under a running node, so retention is
// the job's [maintenance.Job.Horizon], asked afresh on every sweep rather than
// held as a value; a horizon of zero or less is refused there rather than
// read as "retain nothing", which would empty every inbox. See
// [maintenance.Horizon].
func InboxJobs(db store.ReplicatedHandle, retention maintenance.Horizon) []maintenance.Job {
	if db.IsZero() {
		return nil
	}
	return []maintenance.Job{{
		Name: "tracker_notifications", Scope: maintenance.NodeLocal, Horizon: retention,
		Run: func(ctx context.Context, _, cutoff time.Time) (int64, error) {
			return purgeInbox(ctx, db, cutoff)
		},
	}}
}

// purgeInbox deletes the notification rows authored before the cutoff.
//
// It compares the AUTHORED instant, the indexed column the applier's horizon
// also compares, so it deletes exactly what a replay would decline to write;
// the effective instant would let the inbox grow back on every reanchor.
//
// A pooled write transaction, not a pin: every pin the replicated estate
// declares belongs to a domain's applier for the process's life, so a pin
// would be refused every tick, and the pooled transaction takes the same
// write lock through the same queue (internal/store's writequeue.go).
func purgeInbox(ctx context.Context, db store.ReplicatedHandle, cutoff time.Time) (int64, error) {
	var swept int64
	err := db.Tx(ctx, func(tx *sql.Tx) error {
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		res, err := tx.ExecContext(ctx,
			`DELETE FROM tracker_notifications WHERE created_at < ?`,
			store.EncodeTime(cutoff))
		if err != nil {
			return fmt.Errorf("worklet: sweep the inbox: %w", err)
		}
		swept, err = res.RowsAffected()
		return err
	})
	return swept, err
}

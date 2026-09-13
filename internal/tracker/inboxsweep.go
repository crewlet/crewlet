package tracker

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/maintenance"
	"github.com/crewlet/crewlet/internal/store"
)

// The inbox retention sweep.
//
// # It was designed, indexed, configured — and never written
//
// `tracker_notifications` ships `tracker_notifications_swept_idx ON
// (created_at)`, whose migration comment names "the per-node inbox retention
// sweep, which is a range delete". `tracker.native.inbox_retention_days` is a
// validated company setting with a default, a floor and a ceiling, and its
// package doc says it is "the one horizon here that deletes anything". Nothing
// deleted anything: the horizon was read at APPLY time, where it stops a whole-
// log replay rebuilding an expired inbox, and no row ever aged out afterwards.
//
// So every routed change wrote a row per recipient and kept it for the life of
// the deployment — on every node, since each applies the same log. That is the
// exact failure the maintenance package doc names about the `<domain>_ops`
// tables, one table over.
//
// # PER NODE, and the table's class is why
//
// `tracker_notifications` is [statelog.Divergent]: it travels inside a
// snapshot but is NOT in the identity claim, because what it holds depends on
// the epoch's own horizon. Two nodes on different epochs legitimately hold
// different rows, so deleting on this node's own authority breaks nothing —
// and a fleet singleton would sweep one node and let the table grow for ever
// on every other, which looks exactly like a sweep that is working to the
// operator who checks the node it ran on.
//
// # The history is untouched
//
// A notice is a POINTER at a history row, and the history answers for ever.
// What ages out is the mailbox entry, which is what the config doc promises:
// "the history it points at is untouched".

// InboxJobs is the sweep for a person's inbox rows.
func InboxJobs(db *store.DB, retention time.Duration) []maintenance.Job {
	if db == nil {
		return nil
	}
	return []maintenance.Job{{
		Name: "tracker_notifications", Horizon: retention, PerNode: true,
		Run: func(ctx context.Context, _, cutoff time.Time) (int64, error) {
			return purgeInbox(ctx, db, cutoff)
		},
	}}
}

// purgeInbox deletes the notification rows authored before the cutoff.
//
// ON THE AUTHORED INSTANT, which is the column the index is on and the one the
// applier's own horizon compares against — so a row this sweep deletes is
// exactly a row a replay would decline to write. Comparing on the effective
// instant instead would make the two disagree, and an inbox would grow back on
// every reanchor by the width of that disagreement.
func purgeInbox(ctx context.Context, db *store.DB, cutoff time.Time) (int64, error) {
	w, err := db.Replicated().Writer(ctx)
	if err != nil {
		return 0, fmt.Errorf("tracker: take the writer to sweep the inbox: %w", err)
	}
	defer w.Close()

	var swept int64
	err = w.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`DELETE FROM tracker_notifications WHERE created_at < ?`,
			store.EncodeTime(cutoff))
		if err != nil {
			return fmt.Errorf("tracker: sweep the inbox: %w", err)
		}
		swept, err = res.RowsAffected()
		return err
	})
	return swept, err
}

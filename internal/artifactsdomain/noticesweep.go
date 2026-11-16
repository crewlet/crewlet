package artifactsdomain

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/maintenance"
	"github.com/crewlet/crewlet/internal/store"
)

// THE NOTICES' SWEEP: the one write of the replicated estate this domain makes
// outside its applier, and why it may. artifact_notifications is DIVERGENT
// (apply_notices.go): whether a record writes a notice depends on the batch's
// instant and the horizon its epoch states, so two nodes legitimately hold
// different rows and deleting on this node's own authority diverges nothing,
// Gitlet's notice sweep's argument, which internal/store's applier gate names
// beside this file.

// NoticeJobs is the sweep of the notices this node's applier wrote, over the
// replicated estate db: one job deleting the rows past the company's inbox
// horizon, asked at every sweep (persons), as the applier reads it off every
// batch's epoch. PER NODE ([maintenance.NodeLocal]): the table is divergent,
// so each node holds its own rows. The zero handle is a node with no estate,
// which contributes no job.
func NoticeJobs(db store.ReplicatedHandle, persons maintenance.Horizon) []maintenance.Job {
	if db.IsZero() {
		return nil
	}
	return []maintenance.Job{{Name: NoticesTable, Scope: maintenance.NodeLocal, Horizon: persons,
		Run: func(ctx context.Context, _, cutoff time.Time) (int64, error) {
			var swept int64
			err := db.Tx(ctx, func(tx *sql.Tx) error {
				res, err := tx.ExecContext(ctx, sweepNoticesSQL, store.EncodeTime(cutoff))
				if err != nil {
					return fmt.Errorf("artifactsdomain: sweep the comment notices: %w", err)
				}
				swept, err = res.RowsAffected()
				return err
			})
			return swept, err
		}}}
}

// sweepNoticesSQL is the sweep, a range of artifact_notifications_swept_idx.
const sweepNoticesSQL = `DELETE FROM artifact_notifications INDEXED BY artifact_notifications_swept_idx
	WHERE created_at < ?`

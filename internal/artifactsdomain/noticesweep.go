package artifactsdomain

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/maintenance"
	"github.com/crewlet/crewlet/internal/store"
)

// THE NOTICES' SWEEP, the one estate write outside the applier: the notices
// table is DIVERGENT (apply_notices.go), so deleting on this node's own
// authority diverges nothing, as with Gitlet's notice sweep.

// NoticeJobs is the per-node sweep ([maintenance.NodeLocal]) of the notices
// past the company's inbox horizon, over the replicated estate db; a zero
// handle, a node with no estate, contributes no job.
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

package gitlet

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/maintenance"
	"github.com/crewlet/crewlet/internal/store"
)

// THE NOTICES' SWEEP is this domain's one write of the replicated estate
// outside its applier. It may: code_notifications is DIVERGENT
// (apply_notices.go), since whether a record writes a notice depends on the
// batch's instant and its epoch's horizon, so deleting on this node's own
// authority diverges nothing, as for the tracker's inbox sweep of
// tracker_notifications. internal/store's applier gate names both files.

// NoticeJobs is the sweep of the notices this node's applier wrote: one job a
// recipient kind, deleting its rows past its horizon — a person's the
// company's inbox horizon (persons, asked at every sweep as the applier reads
// it off every batch's epoch), a seat's [SeatNoticeHorizon]. PER NODE
// ([maintenance.NodeLocal]): each node holds its own divergent rows, so a
// fleet singleton would let every other node's grow for ever. The zero handle
// is a node with no estate, which contributes no job.
func NoticeJobs(db store.ReplicatedHandle, persons maintenance.Horizon) []maintenance.Job {
	if db.IsZero() {
		return nil
	}
	sweep := func(kind PartyKind) func(ctx context.Context, _, cutoff time.Time) (int64, error) {
		return func(ctx context.Context, _, cutoff time.Time) (int64, error) {
			var swept int64
			err := db.Tx(ctx, func(tx *sql.Tx) error {
				res, err := tx.ExecContext(ctx, sweepNoticesSQL, string(kind), store.EncodeTime(cutoff))
				if err != nil {
					return fmt.Errorf("gitlet: sweep the %s notices: %w", kind, err)
				}
				swept, err = res.RowsAffected()
				return err
			})
			return swept, err
		}
	}
	return []maintenance.Job{
		{Name: "code_notifications", Scope: maintenance.NodeLocal, Horizon: persons, Run: sweep(PartyPerson)},
		{Name: "code_notifications_agents", Scope: maintenance.NodeLocal,
			Horizon: maintenance.Fixed(SeatNoticeHorizon), Run: sweep(PartyAgent)},
	}
}

// sweepNoticesSQL is a kind's sweep, a range of code_notifications_swept_idx
// (registered in plan_test.go).
const sweepNoticesSQL = `DELETE FROM code_notifications INDEXED BY code_notifications_swept_idx
	WHERE recipient_kind = ? AND created_at < ?`

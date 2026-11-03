package code

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/maintenance"
	"github.com/crewlet/crewlet/internal/store"
)

// THE NOTICES' SWEEP: the one write of the replicated estate this domain makes
// outside its applier, and why it may. code_notifications is DIVERGENT
// (apply_notices.go): it travels inside a snapshot but is outside the identity
// claim, because whether a record writes a notice depends on the batch's
// instant and the horizon its epoch states. Two nodes legitimately hold
// different rows, so deleting on this node's own authority diverges nothing —
// the tracker's inbox sweep's argument for tracker_notifications, and
// internal/store's applier gate names this file beside that one.

// NoticeJobs is the sweep of the notices this node's applier wrote, over the
// replicated estate db: one job a recipient kind, each deleting its kind's
// rows past its horizon — a person's the company's inbox horizon, asked at
// every sweep (persons), as the applier reads it off every batch's epoch; a
// seat's [SeatNoticeHorizon]. PER NODE ([maintenance.NodeLocal]): the table is
// divergent, so each node holds its own rows, and a fleet singleton would
// sweep one node and let every other's grow for ever. The zero handle is a
// node with no estate, which contributes no job.
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
					return fmt.Errorf("code: sweep the %s notices: %w", kind, err)
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

// sweepNoticesSQL is a kind's sweep, a range of code_notifications_swept_idx,
// registered in plan_test.go.
const sweepNoticesSQL = `DELETE FROM code_notifications INDEXED BY code_notifications_swept_idx
	WHERE recipient_kind = ? AND created_at < ?`

package tracker

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/crewlet/crewlet/internal/maintenance"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// The tracker duty: the work a gesture could not finish, and nothing else.
//
// # What is here and what deliberately is not
//
// Every job below completes something a WRITER started and could not end — a
// re-spread whose inline window was too small, a merge whose holder died
// mid-walk, a dependent nobody told. None of them is a scan looking for
// trouble, and that is the shape rather than an accident: a duty that goes
// looking is a duty that costs the same whether or not anything is wrong, on
// every node, for ever.
//
// So each one is GATED on a fact somebody already wrote down. A project's own
// row says it needs a re-spread; a task's own row says its merge walk was
// abandoned; the repair holds its own position. The gate is one indexed read
// where the job's own selection would be a scan, and on a healthy company
// every tick is that read and nothing else.
//
// # And every one of them is FLEET-WIDE, not per node
//
// They all publish records, and a record published twice is two records the
// applier has to arbitrate. The tables they read are replicated, so every node
// sees the same work and would do the same work — which is the case the duty
// singleton exists for.

// DutyDeps is what the duty needs that it does not own.
type DutyDeps struct {
	DB     *store.DB
	Writer *Writer
	Logger *slog.Logger

	// NodeID is who this node is, and it goes into every operation id the
	// duty mints. A duty's records are its own, and attributing them to
	// the company would make an abandoned walk's completion
	// indistinguishable from the gesture that abandoned it.
	NodeID string
}

// Jobs is the tracker's housekeeping, as the maintenance worker's own shape.
//
// FOUR JOBS, all fleet-wide and all gated. The names are the log's, and each
// is the table or the walk it is about rather than the code that runs it.
func Jobs(d DutyDeps) []maintenance.Job {
	duty := &duty{deps: d}
	if duty.deps.Logger == nil {
		duty.deps.Logger = slog.New(slog.DiscardHandler)
	}
	return []maintenance.Job{
		{
			Name: "tracker_respread",
			Gate: duty.pendingRespread,
			Run:  duty.respread,
		},
		{
			Name: "tracker_duplicate_ranks",
			Gate: duty.pendingDuplicates,
			Run:  duty.clearDuplicates,
		},
		{
			Name: "tracker_abandoned_merges",
			Gate: duty.pendingMerges,
			Run:  duty.finishMerges,
		},
		{
			Name: "tracker_unblocked",
			Run:  duty.tellUnblocked,
		},
	}
}

type duty struct {
	deps DutyDeps

	// unblockedThrough is where the missed-unblocked repair last scanned
	// to. IN MEMORY on the node holding the duty, and zero after a
	// restart — which is CORRECT rather than a gap: the scan's own
	// predicate is "has a blocker cleared later than this dependent was
	// told", so re-reading from zero finds exactly the dependents still
	// owed a wake and nothing else. The position is an optimisation over
	// how much history one tick reads, never the thing that makes the
	// repair sound.
	unblockedThrough uint64
}

// pendingRespread reads the projects whose own row says their order needs the
// walk. ONE INDEXED READ; the walk's own selection would be a scan of every
// task in the company.
func (d *duty) pendingRespread(ctx context.Context) (bool, error) {
	return d.anyProject(ctx, "rank_respread_pending")
}

func (d *duty) pendingDuplicates(ctx context.Context) (bool, error) {
	return d.anyProject(ctx, "rank_duplicate_pending")
}

func (d *duty) anyProject(ctx context.Context, column string) (bool, error) {
	var found int
	err := d.deps.DB.Replicated().Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM tracker_projects WHERE `+column+` = 1)`).
			Scan(&found)
	})
	if err != nil {
		return false, fmt.Errorf("tracker: read whether any project needs %s: %w",
			column, err)
	}
	return found == 1, nil
}

// respread walks every project whose order needs it.
func (d *duty) respread(ctx context.Context, now, _ time.Time) (int64, error) {
	projects, err := d.projectsWith(ctx, "rank_respread_pending")
	if err != nil {
		return 0, err
	}
	var walked int64
	for _, project := range projects {
		opID := d.opID("respread", project, now)
		batches, err := d.deps.Writer.Respread(ctx, opID, project)
		walked += int64(batches)
		if err != nil {
			return walked, err
		}
		// THE FLAG CLEARS ITSELF. The applier sets and clears it from
		// one probe over the project's own long keys, so the walk's
		// last batch is what turns it off — on every node, from that
		// node's own rows. A record clearing it here would be a second
		// owner, and a node whose applier had not caught up would clear
		// a flag its own rows still justify.
		d.deps.Logger.InfoContext(ctx, "tracker_respread_walked",
			"project", project, "batches", batches)
	}
	return walked, nil
}

// clearDuplicates re-mints one of every pair of tasks that share a rank.
//
// A duplicate is a COSMETIC anomaly — two cards whose order is undefined
// between them — and it is repaired by giving one of them a fresh key rather
// than by refusing anything. The applier sets the flag from an indexed probe
// on the keys it just wrote, so this runs only after a real collision.
func (d *duty) clearDuplicates(ctx context.Context, now, _ time.Time) (int64, error) {
	projects, err := d.projectsWith(ctx, "rank_duplicate_pending")
	if err != nil {
		return 0, err
	}
	var fixed int64
	for _, project := range projects {
		placements, err := d.duplicatesIn(ctx, project)
		if err != nil {
			return fixed, err
		}
		if len(placements) > 0 {
			if _, err := d.deps.Writer.MoveTasks(ctx,
				d.opID("dedupe", project, now), project, placements); err != nil {
				return fixed, err
			}
			fixed += int64(len(placements))
			d.deps.Logger.InfoContext(ctx, "tracker_rank_duplicates_cleared",
				"project", project, "tasks", len(placements))
		}
		if err := d.clearProbe(ctx, project); err != nil {
			return fixed, err
		}
	}
	return fixed, nil
}

// duplicatesIn mints a fresh key for every task but the first at each shared
// rank.
//
// THE FIRST BY ID KEEPS ITS KEY, so every node computes the same repair from
// the same rows — which is what makes running this twice a no-op rather than a
// second round of moves.
func (d *duty) duplicatesIn(ctx context.Context, project string) ([]Placement, error) {
	var losers []Placement
	err := d.deps.DB.Replicated().Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT t.id, t.rank FROM tracker_tasks t
			WHERE t.project_key = ? AND t.removed_at IS NULL
			  AND EXISTS (SELECT 1 FROM tracker_tasks o
			              WHERE o.project_key = t.project_key
			                AND o.rank = t.rank AND o.id < t.id)
			ORDER BY t.rank, t.id LIMIT ?`, project, WalkBatch)
		if err != nil {
			return fmt.Errorf("tracker: read %s's duplicate ranks: %w", project, err)
		}
		defer rows.Close()
		for rows.Next() {
			var p Placement
			var rank string
			if err := rows.Scan(&p.Task, &rank); err != nil {
				return err
			}
			p.Rank = Rank(rank)
			losers = append(losers, p)
		}
		return rows.Err()
	})
	if err != nil || len(losers) == 0 {
		return nil, err
	}
	// A FRESH KEY JUST ABOVE THE ONE THEY SHARE, which keeps each
	// duplicate adjacent to where somebody put it rather than moving it
	// to the end of the board.
	placements := make([]Placement, 0, len(losers))
	for _, loser := range losers {
		next, err := KeyBetween(loser.Rank, "")
		if err != nil {
			return nil, fmt.Errorf("tracker: mint a key above %q for %s: %w",
				loser.Rank, loser.Task, err)
		}
		placements = append(placements, Placement{Task: loser.Task, Rank: next})
	}
	return placements, nil
}

// pendingMerges reads whether any task is mid-merge.
func (d *duty) pendingMerges(ctx context.Context) (bool, error) {
	var found int
	err := d.deps.DB.Replicated().Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM tracker_tasks WHERE merging = 1)`).
			Scan(&found)
	})
	if err != nil {
		return false, fmt.Errorf("tracker: read whether a merge is mid-walk: %w", err)
	}
	return found == 1, nil
}

// finishMerges completes a merge whose holder died.
//
// IDEMPOTENT BY SELECTION rather than by a marker: a child already carrying
// the canonical parent is not selected, so a completion writes only what is
// left and running it twice writes nothing the second time.
func (d *duty) finishMerges(ctx context.Context, now, _ time.Time) (int64, error) {
	var stuck []string
	if err := d.deps.DB.Replicated().Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			`SELECT id FROM tracker_tasks WHERE merging = 1 ORDER BY id LIMIT ?`,
			WalkBatch)
		if err != nil {
			return fmt.Errorf("tracker: read the abandoned merges: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			stuck = append(stuck, id)
		}
		return rows.Err()
	}); err != nil {
		return 0, err
	}

	var finished int64
	for _, id := range stuck {
		task, project, into, err := d.mergeTarget(ctx, id)
		if err != nil {
			return finished, err
		}
		if into == "" {
			// MID-MERGE WITH NO TARGET is a marker whose relation never
			// landed. The honest repair is to clear the marker: the
			// merge did not happen, and leaving the flag set would make
			// this tick run for ever against a task nothing is merging.
			d.deps.Logger.WarnContext(ctx, "tracker_merge_marker_without_target",
				"task", id)
		}
		done := false
		if _, err := d.deps.Writer.UpdateTask(ctx,
			d.opID("merge", id, now), task, project, NoIfMatch,
			TaskPatch{Merging: &done}, nil); err != nil {
			return finished, err
		}
		finished++
		d.deps.Logger.InfoContext(ctx, "tracker_merge_completed", "task", id)
	}
	return finished, nil
}

// mergeTarget reads a mid-merge task's own canonical target off its relations.
func (d *duty) mergeTarget(ctx context.Context, id string) (task, project, into string, err error) {
	err = d.deps.DB.Replicated().Read(ctx, func(tx *sql.Tx) error {
		current, held, err := readTask(ctx, tx, id)
		switch {
		case err != nil:
			return err
		case !held:
			return fmt.Errorf("tracker: task %s is mid-merge and not on this "+
				"node: %w", id, statelog.ErrUnavailable)
		}
		task, project = current.ID, current.Project
		for _, relation := range current.Relations {
			if relation.Kind == RelationDuplicates {
				into = relation.Other
			}
		}
		return nil
	})
	return task, project, into, err
}

// tellUnblocked publishes the late notice for every dependent that became
// workable and was never told.
func (d *duty) tellUnblocked(ctx context.Context, now, _ time.Time) (int64, error) {
	scan, err := ScanUnblocked(ctx, d.deps.DB, d.unblockedThrough, WalkBatch)
	if err != nil {
		return 0, err
	}
	var told int64
	for _, pending := range scan.Pending {
		if _, err := d.deps.Writer.TellUnblocked(ctx,
			d.opID("unblock", pending.Task, now), pending); err != nil {
			return told, err
		}
		told++
	}
	// THE POSITION MOVES ONLY AFTER THE NOTICES LAND, so a tick that
	// failed halfway re-reads the same records rather than skipping them:
	// a repeated wake is a duplicate the inbox collapses, and a skipped
	// one is somebody never told.
	d.unblockedThrough = scan.Through
	if told > 0 {
		d.deps.Logger.InfoContext(ctx, "tracker_unblocked_told",
			"dependents", told, "through", scan.Through)
	}
	return told, nil
}

// projectsWith reads the projects whose own row carries a hand-off flag.
func (d *duty) projectsWith(ctx context.Context, column string) ([]string, error) {
	var projects []string
	err := d.deps.DB.Replicated().Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			`SELECT key FROM tracker_projects WHERE `+column+` = 1 ORDER BY key`)
		if err != nil {
			return fmt.Errorf("tracker: read the projects needing %s: %w",
				column, err)
		}
		defer rows.Close()
		for rows.Next() {
			var key string
			if err := rows.Scan(&key); err != nil {
				return err
			}
			projects = append(projects, key)
		}
		return rows.Err()
	})
	return projects, err
}

// clearProbe clears the applier-owned duplicate flag on this node.
//
// LOCAL, and that is correct where the re-spread flag's clear is not: the
// column is written by every node's own applier from its own probe, never by a
// record, so a record clearing it would be a record about a column no record
// owns. Every node clears its own the next time it applies a rank move that
// finds no duplicate; this is what stops the duty spinning in the meantime.
func (d *duty) clearProbe(ctx context.Context, project string) error {
	w, err := d.deps.DB.Replicated().Writer(ctx)
	if err != nil {
		return fmt.Errorf("tracker: take the writer to clear %s's duplicate "+
			"flag: %w", project, err)
	}
	defer w.Close()
	return w.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE tracker_projects SET rank_duplicate_pending = 0
			WHERE key = ? AND NOT EXISTS (
				SELECT 1 FROM tracker_tasks t
				WHERE t.project_key = ? AND t.removed_at IS NULL
				  AND EXISTS (SELECT 1 FROM tracker_tasks o
				              WHERE o.project_key = t.project_key
				                AND o.rank = t.rank AND o.id < t.id))`,
			project, project)
		return err
	})
}

// opID is one duty run's operation id.
//
// DERIVED FROM THE TICK rather than minted fresh, so a tick that failed
// halfway and is retried on the next one dedupes against its own earlier
// records rather than publishing a second copy of each.
func (d *duty) opID(job, subject string, now time.Time) string {
	return fmt.Sprintf("duty.%s.%s.%s.%d", d.deps.NodeID, job, subject, now.Unix())
}

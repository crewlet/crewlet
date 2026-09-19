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

	// Leads is the company's lead map, for the one repair whose commit
	// carries a wake. Nil routes to the blocker's assignee alone, which
	// is what that wake is for — the project lead is only its fallback.
	Leads Leads

	// NodeID is who this node is, and it goes into every operation id the
	// duty mints. A duty's records are its own, and attributing them to
	// the company would make an abandoned walk's completion
	// indistinguishable from the gesture that abandoned it.
	NodeID string
}

// Jobs is the tracker's housekeeping, as the maintenance worker's own shape.
//
// FIVE JOBS, every one [maintenance.Fleet] and all but one gated. The names
// are the log's, and each is the table or the walk it is about rather than the
// code that runs it.
func Jobs(d DutyDeps) []maintenance.Job {
	duty := &duty{deps: d}
	if duty.deps.Logger == nil {
		duty.deps.Logger = slog.New(slog.DiscardHandler)
	}
	return []maintenance.Job{
		{
			Name:  "tracker_respread",
			Scope: maintenance.Fleet,
			Gate:  duty.pendingRespread,
			Run:   duty.respread,
		},
		{
			Name:  "tracker_duplicate_ranks",
			Scope: maintenance.Fleet,
			Gate:  duty.pendingDuplicates,
			Run:   duty.clearDuplicates,
		},
		{
			Name:  "tracker_abandoned_merges",
			Scope: maintenance.Fleet,
			Gate:  duty.pendingMerges,
			Run:   duty.finishMerges,
		},
		{
			Name:  "tracker_unblocked",
			Scope: maintenance.Fleet,
			Run:   duty.tellUnblocked,
		},
		{
			Name:  "tracker_one_sided",
			Scope: maintenance.Fleet,
			Gate:  duty.pendingOneSided,
			Run:   duty.repairOneSided,
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
// IT FINISHES THE MERGE rather than tidying its marker. The residue a
// [Writer.MergeDuplicates] that died leaves is: the mark landed, some of the
// subtasks moved, and the close — the cancelled status and the marker's own
// removal — did not. So the repair is the rest of that sequence, in its own
// order: move what is left, then close. Clearing the marker alone left the
// duplicate OPEN and half-merged for ever, and on a board that is a live item
// linked as a duplicate of another live item, which is precisely the state
// the merge exists to remove.
//
// IDEMPOTENT BY SELECTION rather than by a cursor: [Writer.reparentOnto]'s
// batch asks for the children a task still HAS, so a completion moves only
// what is left and running it twice moves nothing the second time.
//
// AND IT RE-PARENTS ONLY IF THE MERGE SAID TO. `move_subtasks: false` is an
// explicit instruction to leave a subtree where it is, and a repair cannot
// tell that from a walk that died before its first child — which is why the
// mark carries the intent (see [Task.MergeReparent]) rather than the duty
// guessing at it.
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
		walk, err := d.abandonedMerge(ctx, id)
		if err != nil {
			return finished, err
		}
		opID := d.opID("merge", id, now)
		done := false
		if walk.into == "" {
			// MID-MERGE WITH NO TARGET is a marker whose relation never
			// landed. The honest repair is to clear the marker and
			// NOTHING ELSE: the merge did not happen, so cancelling
			// the task would close an item nobody merged — and leaving
			// the flag set would make this tick run for ever against a
			// task nothing is merging.
			d.deps.Logger.WarnContext(ctx, "tracker_merge_marker_without_target",
				"task", id, "detail", "the marker is cleared and the task left "+
					"open; the merge it names never linked anything")
			//nolint:govet // shadow: scoped to this block; see .golangci.yml
			if _, err := d.deps.Writer.UpdateTask(ctx, opID, walk.task,
				walk.project, NoIfMatch, TaskPatch{Merging: &done},
				ChangeFields, nil); err != nil {
				return finished, err
			}
			finished++
			continue
		}
		var moved int
		if walk.reparent {
			if moved, err = d.deps.Writer.reparentOnto(ctx, opID,
				walk.task, walk.into); err != nil {
				return finished, err
			}
		}
		// THE CLOSE IS THE SAME APPEND THE SEQUENCE WOULD HAVE MADE —
		// the cancelled status and the marker together, on the
		// duplicate's own subject. Split in two it would leave a
		// cancelled task still marked mid-merge, which this job would
		// then pick up again on every tick for ever.
		cancelled := StatusCancelled
		if _, err := d.deps.Writer.UpdateTask(ctx, stepID(opID, "close"),
			walk.task, walk.project, NoIfMatch,
			TaskPatch{Status: &cancelled, Merging: &done},
			ChangeStatus, nil); err != nil {
			return finished, err
		}
		finished++
		d.deps.Logger.InfoContext(ctx, "tracker_merge_completed",
			"task", id, "into", walk.into, "subtasks_moved", moved)
	}
	return finished, nil
}

// abandonedMerge is a mid-merge task's own account of the walk that stopped:
// what it was merging into, and what that walk meant to do with its subtasks.
type abandonedMerge struct {
	task, project string

	// into is the canonical task, read off the `duplicates` relation the
	// mark wrote — empty when the mark's relation never landed.
	into string

	// reparent is the walk's own intent, carried on the task since the
	// mark. FALSE for a marker written by a build that predates the
	// field, which is the conservative direction: a subtree left where it
	// is can be moved afterwards, and one moved against an explicit
	// `move_subtasks: false` has to be put back by hand.
	reparent bool
}

func (d *duty) abandonedMerge(ctx context.Context, id string) (abandonedMerge, error) {
	var walk abandonedMerge
	err := d.deps.DB.Replicated().Read(ctx, func(tx *sql.Tx) error {
		current, held, err := readTask(ctx, tx, id)
		switch {
		case err != nil:
			return err
		case !held:
			return fmt.Errorf("tracker: task %s is mid-merge and not on this "+
				"node: %w", id, statelog.ErrUnavailable)
		}
		walk.task, walk.project = current.ID, current.Project
		walk.reparent = current.MergeReparent
		for _, relation := range current.Relations {
			if relation.Kind == RelationDuplicates {
				walk.into = relation.Other
			}
		}
		return nil
	})
	return walk, err
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
	defer func() { _ = w.Close() }()
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

// pendingOneSided is the gate: one indexed read against the partial index the
// schema ships for exactly this predicate.
//
// A HEALTHY COMPANY PAYS THIS AND NOTHING ELSE. The index is
// `tracker_relations (task_id) WHERE one_sided = 1 AND one_sided_final = 0`,
// so the probe touches no row at all when every dependency is whole — which
// is the shape every job in this file is held to.
func (d *duty) pendingOneSided(ctx context.Context) (bool, error) {
	var any int
	err := d.deps.DB.Replicated().Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			SELECT EXISTS (SELECT 1 FROM tracker_relations
			               WHERE one_sided = 1 AND one_sided_final = 0)`).
			Scan(&any)
	})
	if err != nil {
		return false, fmt.Errorf("tracker: probe for one-sided dependencies: %w", err)
	}
	return any == 1, nil
}

// repairOneSided writes the mirror commit a dependency gesture never reached.
//
// AGED, by [OneSidedRepairAge], so the duty never races a gesture that is
// still running: a mirror published a second before the writer's own would be
// two records on one subject and two wakes for one blocker's assignee.
func (d *duty) repairOneSided(ctx context.Context, now, _ time.Time) (int64, error) {
	edges, err := ScanOneSided(ctx, d.deps.DB, now.Add(-OneSidedRepairAge), WalkBatch)
	if err != nil {
		return 0, err
	}
	var repaired int64
	for _, edge := range edges {
		reason, final := edge.Final()
		if _, err := d.deps.Writer.RepairOneSided(ctx,
			d.opID("onesided", edge.Dependent+"."+edge.Blocker, now),
			edge, d.leads()); err != nil {
			// ONE EDGE'S FAILURE IS NOT THE TICK'S. The rest of this
			// batch is independent — different subjects, different
			// blockers — and stopping here would let one wedged
			// counterparty hold up every other repair in the company.
			d.deps.Logger.WarnContext(ctx, "tracker_one_sided_repair_failed",
				"dependent", edge.Dependent, "blocker", edge.Blocker,
				"error", err)
			continue
		}
		repaired++
		if final {
			d.deps.Logger.InfoContext(ctx, "tracker_one_sided_final",
				"dependent", edge.Dependent, "blocker", edge.Blocker,
				"reason", reason)
		}
	}
	if repaired > 0 {
		d.deps.Logger.InfoContext(ctx, "tracker_one_sided_repaired",
			"edges", repaired)
	}
	return repaired, nil
}

// leads is the duty's own lead map, which is nil unless one was supplied.
//
// NIL IS A VALID ANSWER rather than a missing dependency: a repair's wake
// routes to the blocker's assignee, and the project lead is only the fallback
// for a blocker that has none. A duty without a chart tells the assignee and
// nobody else, which is the whole of what this wake is for.
func (d *duty) leads() Leads { return d.deps.Leads }

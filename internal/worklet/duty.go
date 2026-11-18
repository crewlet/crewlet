package worklet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/crewlet/crewlet/internal/maintenance"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// The Worklet duty finishes what a writer started and could not end — a
// re-spread too big for its inline window, a merge or cross-project move whose
// holder died mid-walk, a dependent nobody told — and nothing else. No job
// scans for trouble, which would cost the same whether or not anything is
// wrong, on every node, for ever: each is GATED on a fact somebody wrote down
// (a flag on a project's or task's own row, the repair's own position), so a
// healthy company's tick is one indexed read per job.
//
// Every job is FLEET-WIDE: each publishes records, a record published twice is
// two records to arbitrate, and the tables are replicated, so every node would
// do the same work — the case the duty singleton is for.

// DutyDeps is what the duty needs that it does not own.
type DutyDeps struct {
	DB     store.ReplicatedHandle
	Writer *Writer

	// Logger is where the duty reports. Nil is the package's own
	// component logger, never silence.
	Logger *slog.Logger

	// Leads is the company's lead map, for the one repair whose commit carries
	// a wake; nil is valid (see [duty.leads]).
	Leads Leads

	// NodeID goes into every operation id the duty mints: attributing its
	// records to the company would make an abandoned walk's completion
	// indistinguishable from the gesture that abandoned it.
	NodeID string
}

// Jobs is the tracker's housekeeping, as the maintenance worker's own shape:
// seven [maintenance.Fleet] jobs, all but tracker_unblocked gated, each named
// for the table or walk it is about rather than the code that runs it.
func Jobs(d DutyDeps) []maintenance.Job {
	duty := newDuty(d)
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
			Name:  "tracker_abandoned_moves",
			Scope: maintenance.Fleet,
			Gate:  duty.pendingMoves,
			Run:   duty.finishMoves,
		},
		{
			Name:  "tracker_move_stragglers",
			Scope: maintenance.Fleet,
			Gate:  duty.pendingStragglers,
			Run:   duty.carryStragglers,
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

// newDuty is the duty over its deps with every optional one defaulted — its
// own function because [Jobs] hands back method values a test cannot read the
// logger off, so the default is asserted through this.
func newDuty(d DutyDeps) *duty {
	if d.Logger == nil {
		d.Logger = log
	}
	return &duty{deps: d}
}

type duty struct {
	deps DutyDeps

	// unblockedThrough is where the missed-unblocked repair last scanned to, in
	// memory on the duty's node and zero after a restart. That is correct: the
	// scan asks "has a blocker cleared later than this dependent was told", so
	// rescanning from zero finds exactly the dependents still owed a wake. The
	// position only bounds how much history one tick reads.
	unblockedThrough uint64
}

// pendingRespread reads the projects whose own row says their order needs the
// walk: one indexed read where the walk's selection would scan every task.
func (d *duty) pendingRespread(ctx context.Context) (bool, error) {
	return d.anyProject(ctx, "rank_respread_pending")
}

func (d *duty) pendingDuplicates(ctx context.Context) (bool, error) {
	return d.anyProject(ctx, "rank_duplicate_pending")
}

func (d *duty) anyProject(ctx context.Context, column string) (bool, error) {
	var found int
	err := d.deps.DB.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM tracker_projects WHERE `+column+` = 1)`).
			Scan(&found)
	})
	if err != nil {
		return false, fmt.Errorf("worklet: read whether any project needs %s: %w",
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
		// The flag clears itself: the applier sets and clears it from one probe of
		// the project's own long keys, so the walk's last batch turns it off on
		// every node. A record clearing it would be a second owner, and would clear
		// it on a lagging node whose own rows still justify it.
		d.deps.Logger.InfoContext(ctx, "tracker_respread_walked",
			"project", project, "batches", batches)
	}
	return walked, nil
}

// clearDuplicates re-mints one of every pair of tasks that share a rank. A
// duplicate is COSMETIC — two cards whose order is undefined — so it is
// repaired with a fresh key, never by refusing anything; the applier sets the
// flag from an indexed probe on the keys it just wrote, so this runs only after
// a real collision.
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

// The two reads [duty.duplicatesIn] takes, named for the plan test: a page of
// the project's live tasks holding a rank an earlier task of it holds too, in
// the board's order, and the next rank above a shared one over every row of
// the project.
const (
	duplicateRanksStatement = `
		SELECT t.id, t.rank FROM tracker_tasks t
		WHERE t.project_key = ? AND t.removed_at IS NULL
		  AND EXISTS (SELECT 1 FROM tracker_tasks o
		              WHERE o.project_key = t.project_key
		                AND o.rank = t.rank AND o.id < t.id)
		ORDER BY t.rank, t.id LIMIT ?`
	rankAboveStatement = `
		SELECT MIN(rank) FROM tracker_tasks
		WHERE project_key = ? AND rank > ?`
)

// duplicatesIn mints a fresh key for every task but the first at each shared
// rank.
//
// The first by id keeps its key, so every node computes the same repair from
// the same rows and running it twice is a no-op. Each group is re-minted with
// [MoveKeys] between the shared key and the next key above it, so every loser
// gets its own key adjacent to where somebody put it. Minting "the key after
// this one" per loser is wrong twice: three tasks sharing a key leave two
// sharing the next, and the key after a pure integer is the next integer — the
// create lattice — so the repair would collide with the project's next create.
func (d *duty) duplicatesIn(ctx context.Context, project string) ([]Placement, error) {
	var (
		losers  []Placement
		ceiling = map[Rank]Rank{}
	)
	err := d.deps.DB.Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, duplicateRanksStatement, project, WalkBatch)
		if err != nil {
			return fmt.Errorf("worklet: read %s's duplicate ranks: %w", project, err)
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
		if err := rows.Err(); err != nil {
			return err
		}
		// The next key above each shared one, over EVERY row of the project: a
		// removed task keeps its key, and a restore returns it there.
		for _, loser := range losers {
			if _, read := ceiling[loser.Rank]; read {
				continue
			}
			var next sql.NullString
			if err := tx.QueryRowContext(ctx, rankAboveStatement,
				project, string(loser.Rank)).Scan(&next); err != nil {
				return fmt.Errorf("worklet: read the key above %q in %s: %w",
					loser.Rank, project, err)
			}
			ceiling[loser.Rank] = Rank(next.String)
		}
		return nil
	})
	if err != nil || len(losers) == 0 {
		return nil, err
	}
	placements := make([]Placement, 0, len(losers))
	for from := 0; from < len(losers); {
		shared := losers[from].Rank
		to := from
		for to < len(losers) && losers[to].Rank == shared {
			to++
		}
		keys, err := MoveKeys(shared, ceiling[shared], to-from)
		if err != nil {
			return nil, fmt.Errorf("worklet: mint %d keys above %q in %s: %w",
				to-from, shared, project, err)
		}
		for i, loser := range losers[from:to] {
			placements = append(placements, Placement{Task: loser.Task, Rank: keys[i]})
		}
		from = to
	}
	return placements, nil
}

// mergePendingStatement is [duty.pendingMerges]' probe, named for the plan
// test.
const mergePendingStatement = `SELECT EXISTS (SELECT 1 FROM tracker_tasks WHERE merging = 1)`

// pendingMerges reads whether any task is mid-merge.
//
// A removed one counts: its merge cannot finish until it is restored, but the
// job also NAMES such a merge every tick ([mergeWaits]), and skipping it would
// hide a merge waiting on a restore nobody knows is needed. The job recognises
// it from its own rows before any claim, for one keyed read.
func (d *duty) pendingMerges(ctx context.Context) (bool, error) {
	var found int
	err := d.deps.DB.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, mergePendingStatement).Scan(&found)
	})
	if err != nil {
		return false, fmt.Errorf("worklet: read whether a merge is mid-walk: %w", err)
	}
	return found == 1, nil
}

// finishMerges completes the merges whose holder died: the rest of
// [Writer.MergeDuplicates]' sequence — move the subtasks left, only if the mark
// said to ([Task.MergeReparent]), then close; clearing the marker alone would
// leave the duplicate open and half-merged. Idempotent by selection:
// [Writer.reparentOnto] moves only the children a task still has.
//
// Each merge runs under its own claim ([duty.finishMerge]). A merge only a
// person can unblock ([mergeWaits]) is recognised from its rows before any
// claim, takes no place in the budget and is named every tick with its remedy,
// so it never holds up the merges after it. One merge's failure is likewise
// its own warning, not the tick's; the job fails only when it cannot read
// which merges there are.
func (d *duty) finishMerges(ctx context.Context, now, _ time.Time) (int64, error) {
	var (
		finished  int64
		attempted int
		waiting   = map[mergeWait][]string{}
		after     string
	)
	fail := func(id string, err error) {
		attempted++
		d.deps.Logger.WarnContext(ctx, "tracker_merge_completion_failed",
			"task", id, "error", err)
	}
	// The budget is [WalkBatch] merges attempted: a waiting merge costs one
	// keyed read and no claim, so it never stands in for work.
	for attempted < WalkBatch {
		ids, err := d.markedAfter(ctx, after)
		if err != nil {
			return finished, err
		}
		if len(ids) == 0 {
			break
		}
		for _, id := range ids {
			if attempted == WalkBatch {
				break
			}
			after = id
			walk, err := d.abandonedMerge(ctx, id)
			switch {
			case err != nil:
				fail(id, err)
				continue
			case walk.waiting != waitNothing:
				waiting[walk.waiting] = append(waiting[walk.waiting], id)
				continue
			}
			done, err := d.finishMerge(ctx, id, now)
			if err != nil {
				fail(id, err)
				continue
			}
			attempted++
			if done {
				finished++
			}
		}
	}
	for _, wait := range mergeWaits {
		if tasks := waiting[wait.why]; len(tasks) > 0 {
			d.deps.Logger.WarnContext(ctx, wait.event, "tasks", tasks,
				"detail", wait.detail)
		}
	}
	return finished, nil
}

// mergeWait is what an abandoned merge is waiting for somebody to change.
type mergeWait string

const (
	// waitNothing: nothing is in the way, and the duty finishes it.
	waitNothing mergeWait = ""
	// waitSurvivorInTrash: the item it merges into is in the trash.
	waitSurvivorInTrash mergeWait = "survivor_in_trash"
	// waitDuplicateInTrash: the duplicate itself is in the trash.
	waitDuplicateInTrash mergeWait = "duplicate_in_trash"
	// waitSurvivorUnderDuplicate: the survivor is filed under one of the
	// subtasks the merge would move onto it.
	waitSurvivorUnderDuplicate mergeWait = "survivor_under_duplicate"
)

// mergeWaits is every reason an abandoned merge waits, with the line the duty
// names it on and what releases it. Each arrives after the mark, so no
// pre-flight prevents it, and each refuses every attempt until somebody acts.
// One table, so a reason cannot be recognised without being said, each with
// its own remedy: one line naming three fixes sends a reader to the wrong one.
var mergeWaits = []struct {
	why           mergeWait
	event, detail string
}{
	{waitSurvivorInTrash, "tracker_merges_waiting_on_the_trash",
		"each is marked mid-merge into an item that is now in the trash, " +
			"which takes no subtask and no duplicate; restore that item and " +
			"the next tick finishes the merge, or remove the duplicate's link " +
			"to it and the next tick clears the mark"},
	{waitDuplicateInTrash, "tracker_merges_of_items_in_the_trash",
		"each is marked mid-merge and is itself in the trash, which takes no " +
			"write — neither the close that finishes the merge nor the " +
			"clearing of its mark; restore it and the next tick finishes the " +
			"merge, or purge it and the merge goes with it"},
	{waitSurvivorUnderDuplicate, "tracker_merges_waiting_on_a_cycle",
		"each would move its subtasks onto an item that has since been filed " +
			"under one of them, which would put that subtask under itself; " +
			"file the item somewhere else and the next tick finishes the " +
			"merge, or remove the duplicate's link to it and the next tick " +
			"clears the mark"},
}

// mergingAfterStatement is [duty.markedAfter]'s page, named for the plan test.
const mergingAfterStatement = `
		SELECT id FROM tracker_tasks WHERE merging = 1 AND id > ?
		ORDER BY id LIMIT ?`

// markedAfter is one page of the tasks marked mid-merge, in id order, after a
// cursor — the partial index's own order, so each page is a seek.
func (d *duty) markedAfter(ctx context.Context, after string) ([]string, error) {
	var ids []string
	err := d.deps.DB.Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, mergingAfterStatement, after, WalkBatch)
		if err != nil {
			return fmt.Errorf("worklet: read the abandoned merges: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	})
	return ids, err
}

// walkHeld reports a walk the duty left alone because it could not take the
// walk's claim, at two levels: a claim a peer holds is a running walk, the
// ordinary case; one coordination could not answer for is a walk the duty
// cannot see, worth attention if it repeats.
func (d *duty) walkHeld(ctx context.Context, event, task string, err error) {
	if errors.Is(err, statelog.ErrUnavailable) {
		d.deps.Logger.DebugContext(ctx, event, "task", task, "detail",
			"its claim is held, so the walk is running")
		return
	}
	d.deps.Logger.WarnContext(ctx, event, "task", task, "error", err)
}

// finishMerge completes one abandoned merge, reporting whether it did — false
// for a walk still running, a claim that could not be read, or a walk that
// finished or began to wait between the selection and the claim.
//
// The marker is up for a LIVE merge too, so only the walk's claim says it was
// abandoned: its holder takes merge/<task> before the mark and heartbeats it
// until the close, so a claim the duty can take was released or unrenewed for
// a whole [ClaimTTL]. The duty takes it under an owner of its own
// ([Writer.claimOwner]), so a walk on THIS node is refused exactly as a
// peer's; selecting on the marker alone re-parented a live walk's children
// twice and closed the duplicate twice.
func (d *duty) finishMerge(ctx context.Context, id string, now time.Time) (bool, error) {
	claim, err := d.deps.Writer.hold(ctx, mergeClaim(id))
	if err != nil {
		// A held claim is a running walk, and one coordination cannot answer for
		// fails CLOSED, as the walk's own claim does: a walk that may be running is
		// not one to race.
		d.walkHeld(ctx, "tracker_merge_walk_held", id, err)
		return false, nil
	}
	defer claim.release(ctx)

	// Read again under the claim: the walk may have finished, or begun to wait,
	// since the selection.
	walk, err := d.abandonedMerge(ctx, id)
	switch {
	case err != nil:
		return false, err
	case !walk.merging:
		// FINISHED BETWEEN THE SELECTION AND THE CLAIM, by the holder
		// that released it.
		return false, nil
	case walk.waiting != waitNothing:
		return false, nil
	}
	opID := d.opID("merge", id, now)
	done := false
	if walk.into == "" {
		// Mid-merge with no target: the relation never landed, or the survivor was
		// purged since. Clear the marker and nothing else — the merge did not
		// happen, so cancelling would close an item nobody merged, and leaving the
		// flag would make this tick run for ever.
		d.deps.Logger.WarnContext(ctx, "tracker_merge_marker_without_target",
			"task", id, "purged_target", walk.purged, "detail", "the marker "+
				"is cleared and the task left open; the merge it names links "+
				"nothing that is still there")
		var cleared WriteResult
		cleared, err = d.deps.Writer.UpdateTask(ctx, opID, walk.task,
			walk.project, NoIfMatch, TaskPatch{Merging: &done},
			ChangeFields, nil)
		if err = resolved(fmt.Sprintf("the merge marker's removal from "+
			"task %s", id), cleared, err); err != nil {
			return false, err
		}
		return true, nil
	}
	var moved int
	if walk.reparent {
		if moved, err = d.deps.Writer.reparentOnto(ctx, opID,
			walk.task, walk.into); err != nil {
			return false, err
		}
	}
	// The close is the sequence's own append: cancelled status and marker
	// together, on the duplicate's subject. Split, it would leave a cancelled task
	// still marked, which this job would pick up every tick for ever.
	cancelled := StatusCancelled
	closed, err := d.deps.Writer.UpdateTask(ctx, stepID(opID, "close"),
		walk.task, walk.project, NoIfMatch,
		TaskPatch{Status: &cancelled, Merging: &done},
		ChangeStatus, nil)
	// An unknown close is not a completed merge: the next tick, which finds the
	// marker still up if it did not land, decides.
	if err = resolved(fmt.Sprintf("task %s's close as a duplicate of %s",
		id, walk.into), closed, err); err != nil {
		return false, err
	}
	d.deps.Logger.InfoContext(ctx, "tracker_merge_completed",
		"task", id, "into", walk.into, "subtasks_moved", moved)
	return true, nil
}

// movePendingStatement is [duty.pendingMoves]' probe, named for the plan test.
const movePendingStatement = `
		SELECT EXISTS (SELECT 1 FROM tracker_tasks
		               WHERE moving = 1 AND removed_at IS NULL)`

// pendingMoves reads whether any root is marked mid-move: one probe on the
// partial index the schema ships for this predicate. A removed root is not
// pending: it is frozen, and its restore brings it back.
func (d *duty) pendingMoves(ctx context.Context) (bool, error) {
	var found int
	err := d.deps.DB.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, movePendingStatement).Scan(&found)
	})
	if err != nil {
		return false, fmt.Errorf("worklet: read whether a move is mid-walk: %w", err)
	}
	return found == 1, nil
}

// movingStatement is [duty.finishMoves]' selection of the marked roots, named
// for the plan test.
const movingStatement = `
		SELECT id FROM tracker_tasks
		WHERE moving = 1 AND removed_at IS NULL ORDER BY id LIMIT ?`

// finishMoves completes every cross-project move whose walk stopped with its
// root marked mid-move, shaped like [duty.finishMerges]: only under the move's
// own claim (the mark is up for a live move too), the root read again once the
// claim is held, and one move's failure not the tick's.
//
// Finishing is the writer's ([Writer.followRoot]), shared with a re-run of the
// move: every descendant still outside the root's project follows it on a
// fresh range, and the mark comes down. A descendant in the trash is waited for
// — the mark stays up and the failure names it — since carrying the rest
// around it would leave it in the old project for good.
func (d *duty) finishMoves(ctx context.Context, now, _ time.Time) (int64, error) {
	var marked []string
	if err := d.deps.DB.Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, movingStatement, WalkBatch)
		if err != nil {
			return fmt.Errorf("worklet: read the abandoned moves: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			marked = append(marked, id)
		}
		return rows.Err()
	}); err != nil {
		return 0, err
	}

	var finished int64
	for _, id := range marked {
		claim, err := d.deps.Writer.hold(ctx, moveClaim(id))
		if err != nil {
			d.walkHeld(ctx, "tracker_move_walk_held", id, err)
			continue
		}
		completed, err := d.deps.Writer.finishAbandonedMove(ctx,
			d.opID("move", id, now), id)
		claim.release(ctx)
		if err != nil {
			d.deps.Logger.WarnContext(ctx, "tracker_move_completion_failed",
				"task", id, "error", err)
			continue
		}
		if completed {
			finished++
			d.deps.Logger.InfoContext(ctx, "tracker_move_completed", "task", id)
		}
	}
	return finished, nil
}

// strandedFrom is the predicate the straggler gate and walk share: a live task
// flagged `inconsistent_project` under a root that is live, not mid-move and a
// real root. Each root condition means nothing to do — a trashed root is frozen
// until restored, a marked one is the abandoned-move job's, and one with a
// parent is where a cycle's walk stopped (the `cycle` flag's to report) — and
// selecting it would hold the gate open every tick.
//
// The partial index's WHERE is spelled out because this planner does not infer
// it (replicated migration 0041). CROSS JOIN drives the query from the flagged
// rows, each root then one primary-key read; joined plainly, the planner (no
// statistics on any node) drove it from the roots, walking every live task
// through `tracker_tasks_type_idx` on every tick.
const strandedFrom = `
	FROM tracker_tasks t CROSS JOIN tracker_tasks r ON r.id = t.root_id
	WHERE t.inconsistent_project = 1 AND t.removed_at IS NULL
	  AND r.removed_at IS NULL AND r.moving = 0
	  AND (r.parent_id IS NULL OR r.parent_id = '')`

// strandedPendingStatement is [duty.pendingStragglers]' probe and
// strandedAfterStatement [duty.strandedAfter]'s page, over the predicate they
// share — named for the plan test.
const (
	strandedPendingStatement = `SELECT EXISTS (SELECT 1 ` + strandedFrom + `)`
	strandedAfterStatement   = `SELECT DISTINCT t.root_id ` + strandedFrom +
		` AND t.root_id > ? ORDER BY t.root_id LIMIT ?`
)

// pendingStragglers reads whether any live task sits in another project than
// its root with nothing walking that root — see [duty.carryStragglers]. ONE
// PROBE on the partial index replicated migration 0041 ships for it.
func (d *duty) pendingStragglers(ctx context.Context) (bool, error) {
	var found int
	err := d.deps.DB.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, strandedPendingStatement).Scan(&found)
	})
	if err != nil {
		return false, fmt.Errorf("worklet: read whether a task was filed "+
			"behind a move: %w", err)
	}
	return found == 1, nil
}

// carryStragglers carries into its root's project every task a finished move
// left behind: a create DECIDED on a node that had not applied the root's move
// and ACCEPTED after the mark came down (the applier files it under its parent,
// now in the new project, and flags it `inconsistent_project`), or a re-parent
// a lagging node decided under a since-moved root's descendant. The move cannot
// see either, and with the mark down the abandoned-move job never looks again.
//
// Finishing is the move's own pass ([Writer.followRoot]) under the move's own
// claim, the root read again under it. A straggler in the trash is not waited
// for, since nothing marks the move unfinished: its restore flags it again.
// One root's failure is not the tick's; the budget is [WalkBatch] roots
// attempted, paged in root order on the partial index.
func (d *duty) carryStragglers(ctx context.Context, now, _ time.Time) (int64, error) {
	var (
		carried   int64
		attempted int
		after     string
	)
	for attempted < WalkBatch {
		roots, err := d.strandedAfter(ctx, after)
		if err != nil {
			return carried, err
		}
		if len(roots) == 0 {
			break
		}
		for _, root := range roots {
			if attempted == WalkBatch {
				break
			}
			after = root
			attempted++
			claim, err := d.deps.Writer.hold(ctx, moveClaim(root))
			if err != nil {
				d.walkHeld(ctx, "tracker_move_walk_held", root, err)
				continue
			}
			moved, err := d.deps.Writer.carryStragglers(ctx,
				d.opID("stragglers", root, now), root)
			claim.release(ctx)
			if err != nil {
				d.deps.Logger.WarnContext(ctx, "tracker_move_stragglers_failed",
					"task", root, "error", err)
				continue
			}
			if moved > 0 {
				carried++
				d.deps.Logger.InfoContext(ctx, "tracker_move_stragglers_carried",
					"task", root, "tasks", moved)
			}
		}
	}
	return carried, nil
}

// strandedAfter is one page of the roots with a straggler under them, in root
// order, after a cursor — the partial index's own order, so each page is a
// seek.
func (d *duty) strandedAfter(ctx context.Context, after string) ([]string, error) {
	var roots []string
	err := d.deps.DB.Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, strandedAfterStatement, after, WalkBatch)
		if err != nil {
			return fmt.Errorf("worklet: read the tasks filed behind a move: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var root string
			if err := rows.Scan(&root); err != nil {
				return err
			}
			roots = append(roots, root)
		}
		return rows.Err()
	})
	return roots, err
}

// abandonedMerge is a mid-merge task's own account of the walk that stopped:
// what it was merging into, and what that walk meant to do with its subtasks.
type abandonedMerge struct {
	task, project string

	// into is the canonical task, read off the `duplicates` relation the
	// mark wrote — empty when the mark's relation never landed, or when
	// the task it names has since been purged.
	into string

	// purged is the survivor the mark named when a purge has since
	// destroyed it, for the line that says why a marker was cleared.
	purged string

	// waiting is what the merge waits for somebody to change, or
	// [waitNothing] — see [duty.finishMerges].
	waiting mergeWait

	// reparent is the walk's own intent, carried on the task since the mark:
	// FALSE when the merge was asked to leave its subtasks where they are
	// (`move_subtasks: false`).
	reparent bool

	// merging is whether the task still carries the marker as this read
	// found it.
	merging bool
}

func (d *duty) abandonedMerge(ctx context.Context, id string) (abandonedMerge, error) {
	var walk abandonedMerge
	err := d.deps.DB.Read(ctx, func(tx *sql.Tx) error {
		current, held, err := readTask(ctx, tx, id)
		switch {
		case err != nil:
			return err
		case !held:
			return fmt.Errorf("worklet: task %s is mid-merge and not on this "+
				"node: %w", id, statelog.ErrUnavailable)
		}
		walk.task, walk.project = current.ID, current.Project
		walk.reparent = current.MergeReparent
		walk.merging = current.Merging
		for _, relation := range current.Relations {
			if relation.Kind == RelationDuplicates {
				walk.into = relation.Other
			}
		}
		// The duplicate in the trash first, whatever became of the survivor: a
		// tombstone is a freeze, so every write the duty could make here — the
		// close, or clearing a mark with no target — is refused until it is restored.
		if current.Removed != nil {
			walk.waiting = waitDuplicateInTrash
			return nil
		}
		if walk.into == "" {
			return nil
		}
		// THE SURVIVOR'S OWN STATE, three-valued as [missingTask]'s: in
		// the trash the merge waits, purged it has no target, and absent
		// with no marker this node has not applied what named it.
		canonical, held, err := readTask(ctx, tx, walk.into)
		switch {
		case err != nil:
			return err
		case held && canonical.Removed != nil:
			walk.waiting = waitSurvivorInTrash
			return nil
		case held && walk.reparent:
			// FILED UNDER THE DUPLICATE SINCE THE MARK, which the mark's
			// own pre-flight refused and nothing stops afterwards: the
			// subtask it sits under would move onto it, and
			// [refuseParent] refuses that as a cycle on every attempt.
			var under bool
			if under, err = inSubtree(ctx, tx, walk.task, walk.into); err != nil {
				return err
			}
			if under {
				walk.waiting = waitSurvivorUnderDuplicate
			}
			return nil
		case held:
			return nil
		}
		missing := missingTask(ctx, tx, walk.into, "nothing can be merged into it")
		if !errors.Is(missing, ErrNoTask) {
			// NOT ON THIS NODE YET, or a marker this node could not
			// read: either way nothing here may decide the merge has
			// no target.
			return fmt.Errorf("worklet: task %s is mid-merge into %s: %w",
				id, walk.into, missing)
		}
		walk.purged, walk.into = walk.into, ""
		return nil
	})
	return walk, err
}

// tellUnblocked publishes the late notice for every dependent that became
// workable and was never told.
func (d *duty) tellUnblocked(ctx context.Context, now, _ time.Time) (int64, error) {
	scan, err := ScanUnblocked(ctx, d.deps.DB.Reader(), d.unblockedThrough, WalkBatch)
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
	err := d.deps.DB.Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			`SELECT key FROM tracker_projects WHERE `+column+` = 1 ORDER BY key`)
		if err != nil {
			return fmt.Errorf("worklet: read the projects needing %s: %w",
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

// clearDuplicateProbeStatement is [duty.clearProbe]'s write, named for the
// plan test.
const clearDuplicateProbeStatement = `
		UPDATE tracker_projects SET rank_duplicate_pending = 0
		WHERE key = ? AND NOT EXISTS (
			SELECT 1 FROM tracker_tasks t
			WHERE t.project_key = ? AND t.removed_at IS NULL
			  AND EXISTS (SELECT 1 FROM tracker_tasks o
			              WHERE o.project_key = t.project_key
			                AND o.rank = t.rank AND o.id < t.id))`

// clearProbe clears the applier-owned duplicate flag on this node.
//
// LOCAL, unlike the re-spread flag's clear: each node's applier writes the
// column from its own probe and no record owns it. It is the ONLY clear (the
// apply path writes 0 only on a project's first INSERT), so a peer carries the
// flag until it restarts — bounded, as the column is outside the identity
// claim ([Domain.ClaimsIdentity]) and a published record repairs the
// duplicate. A clear on apply would need the per-drag aggregate
// [Applier.applyRankOrder] declines. The guard is why the tick publishing a
// repair clears nothing: the repair is not applied yet, so the next tick
// clears. A pooled transaction, not a pin, for purgeInbox's reason: every pin
// the replicated estate declares belongs to an applier for the process's life.
func (d *duty) clearProbe(ctx context.Context, project string) error {
	return d.deps.DB.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, clearDuplicateProbeStatement, project, project)
		return err
	})
}

// opID is one duty run's operation id, derived from the tick so every append
// one tick makes for one subject is one operation however often it retries. It
// carries the tick's own instant ([statelog.DeriveOpID]), truncated to the
// second: the moment the operation came to exist.
func (d *duty) opID(job, subject string, now time.Time) string {
	tick := now.Truncate(time.Second)
	return statelog.DeriveOpID(tick, job+"-"+subject, "crewlet.tracker.duty",
		d.deps.NodeID, job, subject, strconv.FormatInt(tick.Unix(), 10))
}

// oneSidedSubject names a one-sided edge's repair in its operation id: the
// dependent and the blocker joined by a COLON, not a dot, which is the
// operation-id grammar's step separator (`<uuidv7>[.<name>][.<step>…]`) and
// would make the blocker a step of another operation. Both are task ids, so a
// colon keeps the pair one name of visible ASCII.
func oneSidedSubject(edge OneSided) string {
	return edge.Dependent + ":" + edge.Blocker
}

// pendingOneSided is the gate: one read of the partial index
// `tracker_relations (task_id) WHERE one_sided = 1 AND one_sided_final = 0`,
// which touches no row when every dependency is whole.
func (d *duty) pendingOneSided(ctx context.Context) (bool, error) {
	var any int
	err := d.deps.DB.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, pendingOneSidedStatement).Scan(&any)
	})
	if err != nil {
		return false, fmt.Errorf("worklet: probe for one-sided dependencies: %w", err)
	}
	return any == 1, nil
}

// repairOneSided settles every one-sided edge old enough to be abandoned: it
// writes the mirror commit a dependency gesture never reached or, where the
// blocker can never take it, stamps the edge permanently one-sided. Aged by
// [OneSidedRepairAge] so it never races a running gesture, whose mirror and
// this one would be two records on one subject and two wakes. The pass's line
// counts mirrors (the repair working) apart from stamps (a dependency somebody
// must resolve by hand).
func (d *duty) repairOneSided(ctx context.Context, now, _ time.Time) (int64, error) {
	edges, err := ScanOneSided(ctx, d.deps.DB.Reader(), now.Add(-OneSidedRepairAge), WalkBatch)
	if err != nil {
		return 0, err
	}
	var mirrored, stamped int64
	for _, edge := range edges {
		reason, final := edge.Final()
		if _, err := d.deps.Writer.RepairOneSided(ctx,
			d.opID("onesided", oneSidedSubject(edge), now),
			edge, d.leads()); err != nil {
			// One edge's failure is not the tick's: the edges are independent, and
			// stopping would let one wedged counterparty hold up every other repair.
			d.deps.Logger.WarnContext(ctx, "tracker_one_sided_repair_failed",
				"dependent", edge.Dependent, "blocker", edge.Blocker,
				"error", err)
			continue
		}
		if !final {
			mirrored++
			continue
		}
		stamped++
		d.deps.Logger.InfoContext(ctx, "tracker_one_sided_final",
			"dependent", edge.Dependent, "blocker", edge.Blocker,
			"reason", reason)
	}
	settled := mirrored + stamped
	if settled > 0 {
		d.deps.Logger.InfoContext(ctx, "tracker_one_sided_repaired",
			"edges", settled, "mirrored", mirrored, "final", stamped)
	}
	return settled, nil
}

// leads is the duty's lead map, nil unless supplied. Nil is valid: a repair's
// wake routes to the blocker's assignee, and the project lead is only the
// fallback for a blocker with none.
func (d *duty) leads() Leads { return d.deps.Leads }

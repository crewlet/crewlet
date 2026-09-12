package tracker

import (
	"context"
	"database/sql"
	"errors"
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

	// Zone is the company's ONE timezone, which the sprint mint needs: a
	// sprint's window is a calendar boundary — a weekday and a minute past
	// midnight — and a boundary computed in UTC for a team in Berlin
	// starts their sprint at one in the morning.
	Zone *time.Location

	// NodeID is who this node is, and it goes into every operation id the
	// duty mints. A duty's records are its own, and attributing them to
	// the company would make an abandoned walk's completion
	// indistinguishable from the gesture that abandoned it.
	NodeID string
}

// Jobs is the tracker's housekeeping, as the maintenance worker's own shape.
//
// FIVE JOBS, all fleet-wide and all gated. The names are the log's, and each
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
		// THE ONE JOB THAT IS NOT A REPAIR, and it says so rather than
		// pretending: a sprint's start and end are CALENDAR boundaries
		// that arrive whether or not anybody is looking, so there is no
		// gesture to finish — the duty is the only thing that can notice
		// them. Its gate is still one indexed read, so a company that
		// runs no sprints pays exactly that.
		{
			Name: "tracker_sprints",
			Gate: duty.sprintWork,
			Run:  duty.runSprints,
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

// sprintWork reports whether any project has a sprint decision waiting.
//
// ONE INDEXED READ over `tracker_sprints`, and it asks the union of the four
// questions the run below answers: is a future sprint due to start, is an
// active one past its end, is a closed one's spillover unsettled, and is a
// settled one old enough to archive. A separate probe per question would be
// four reads on every tick of a company with nothing to do.
//
// AND A FIFTH: is any project short of its `ahead` count. It reads the
// PROJECTS table rather than the sprints one, so it is a second EXISTS rather
// than a fifth term — and it cannot be left out, because a project that has
// just been given a policy has no sprint row for the first four terms to find
// and would never be minted for at all.
func (d *duty) sprintWork(ctx context.Context) (bool, error) {
	var any bool
	err := d.deps.DB.Replicated().Read(ctx, func(tx *sql.Tx) error {
		now := store.EncodeTime(time.Now().UTC())
		return tx.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM tracker_sprints
				WHERE (state = ? AND start_at <= ?)
				   OR (state = ? AND end_at <= ?)
				   OR (state = ? AND (rollover_to IS NULL OR rollover_done = 0))
				   OR (state = ? AND rollover_done = 1 AND archived = 0))
			OR EXISTS (
				SELECT 1 FROM tracker_projects p
				WHERE p.sprint_policy_json IS NOT NULL AND p.archived = 0
				  AND COALESCE(json_extract(p.sprint_policy_json, '$.ahead'), 0) >
				      (SELECT COUNT(*) FROM tracker_sprints s
				       WHERE s.project_key = p.key AND s.state = ?))`,
			string(SprintFuture), now, string(SprintActive), now,
			string(SprintClosed), string(SprintClosed),
			string(SprintFuture)).Scan(&any)
	})
	if err != nil {
		return false, fmt.Errorf("tracker: look for sprint work: %w", err)
	}
	return any, nil
}

// runSprints advances every sprint whose own window says it should move.
//
// # The order is the argument
//
// Close before start, because a project runs ONE sprint at a time and the
// pointer is what says so: starting first would be refused by a pointer the
// close is about to clear, and the tick would do nothing. Roll after close,
// because the rollover selects on a CLOSED sprint. Mint last, because `next`
// mints on demand anyway and a tick that closed nothing has nothing to top up
// that the previous tick did not.
//
// # Every step is idempotent by SELECTION, not by a marker
//
// A close selects active sprints past their end; a rollover selects the open
// tasks still pointing at a closed sprint. So a tick interrupted halfway
// leaves a state the next tick simply continues from, and two nodes racing
// contend at the broker on each record rather than here.
func (d *duty) runSprints(ctx context.Context, now, _ time.Time) (int64, error) {
	var wrote int64
	closed, err := d.closeEndedSprints(ctx, now)
	if err != nil {
		return wrote, err
	}
	wrote += closed

	started, err := d.startDueSprints(ctx, now)
	if err != nil {
		return wrote, err
	}
	wrote += started

	rolled, err := d.rollSettledSprints(ctx, now)
	if err != nil {
		return wrote, err
	}
	wrote += rolled

	archived, err := d.archiveOldSprints(ctx, now)
	if err != nil {
		return wrote, err
	}
	wrote += archived

	minted, err := d.mintAhead(ctx, now)
	if err != nil {
		return wrote, err
	}
	return wrote + minted, nil
}

// closeEndedSprints ends every active sprint past its own window.
//
// UNCONDITIONALLY — a close is not a setting. A sprint that ran past its end
// is a window nobody can report on, because every figure in a sprint report is
// a predicate over that window and an open-ended one grows on every poll.
func (d *duty) closeEndedSprints(ctx context.Context, now time.Time) (int64, error) {
	due, err := d.sprintsWhere(ctx, "state = ? AND end_at <= ?",
		string(SprintActive), store.EncodeTime(now))
	if err != nil {
		return 0, err
	}
	var wrote int64
	for _, s := range due {
		_, err := d.deps.Writer.CloseSprint(ctx,
			d.opID("sprint_close", s.key(), now), s.Project, s.Number)
		switch {
		case errors.Is(err, statelog.ErrExists), errors.Is(err, statelog.ErrConflict):
			// ANOTHER NODE CLOSED IT FIRST, which is the ordinary
			// outcome of two nodes reaching the same tick — the state
			// the caller wanted is the state it is in.
			continue
		case err != nil:
			d.deps.Logger.WarnContext(ctx, "tracker_sprint_close_failed",
				"project", s.Project, "sprint", s.Number, "error", err)
			continue
		}
		wrote++
	}
	return wrote, nil
}

// startDueSprints flips every future sprint whose start has arrived, where the
// policy says to.
//
// `AutoStart` GATES THIS AND NOTHING ELSE. A team that starts its own sprints
// still gets them minted, closed at their end and rolled over — what the knob
// buys is the moment the board changes underneath somebody, which is the one
// part a person may reasonably want to own.
func (d *duty) startDueSprints(ctx context.Context, now time.Time) (int64, error) {
	due, err := d.sprintsWhere(ctx, "state = ? AND start_at <= ? AND archived = 0",
		string(SprintFuture), store.EncodeTime(now))
	if err != nil {
		return 0, err
	}
	var wrote int64
	for _, s := range due {
		policy, held, err := d.sprintPolicy(ctx, s.Project)
		if err != nil {
			return wrote, err
		}
		if !held || !policy.AutoStart {
			continue
		}
		_, err = d.deps.Writer.StartSprint(ctx,
			d.opID("sprint_start", s.key(), now), s.Project, s.Number)
		switch {
		case errors.Is(err, statelog.ErrExists), errors.Is(err, statelog.ErrConflict):
			continue
		case err != nil:
			// A PROJECT ALREADY RUNNING ONE is the ordinary refusal
			// here, not a failure: the previous sprint's close has not
			// been applied on this node yet, and the next tick starts
			// it. Logged at debug for that reason.
			d.deps.Logger.DebugContext(ctx, "tracker_sprint_start_declined",
				"project", s.Project, "sprint", s.Number, "error", err)
			continue
		}
		wrote++
	}
	return wrote, nil
}

// rollSettledSprints carries the unfinished work out of every closed sprint
// whose spillover the policy decides.
//
// `AutoRoll: false` LEAVES IT PENDING and this does nothing, which is the
// whole of that setting: the work stays where it is until a lead says where it
// goes, and `work_sprints` reports `rollover_pending` so somebody can.
func (d *duty) rollSettledSprints(ctx context.Context, now time.Time) (int64, error) {
	// TWO SELECTIONS, ONE PASS. A closed sprint with no `rollover_to` is
	// undecided; one with a target and `rollover_done = 0` is a walk
	// somebody abandoned — including this duty on a previous tick, whose
	// batch was full.
	pending, err := d.sprintsWhere(ctx,
		"state = ? AND (rollover_to IS NULL OR rollover_done = 0)",
		string(SprintClosed))
	if err != nil {
		return 0, err
	}
	var wrote int64
	for _, s := range pending {
		target := RolloverTarget(s.RolloverTo)
		if target == "" {
			policy, held, err := d.sprintPolicy(ctx, s.Project)
			if err != nil {
				return wrote, err
			}
			if !held || !policy.AutoRoll {
				// PENDING IS A STATE, not an absence: a lead settles
				// it, and until they do the work is exactly where
				// everybody left it.
				continue
			}
			target = RolloverNext
		}
		moved, _, err := d.deps.Writer.RolloverSprint(ctx,
			d.opID("sprint_rollover", s.key(), now), s.Project, s.Number, target)
		if err != nil {
			d.deps.Logger.WarnContext(ctx, "tracker_sprint_rollover_failed",
				"project", s.Project, "sprint", s.Number, "error", err)
			continue
		}
		wrote += int64(moved)
	}
	return wrote, nil
}

// archiveOldSprints hides a settled sprint once the policy's window has moved
// past it.
//
// IT TOUCHES NO TASK and no stay — `sprint=<an archived number>` still answers
// — so what this buys is a sprint picker somebody can use on a project three
// years into a weekly cadence.
func (d *duty) archiveOldSprints(ctx context.Context, now time.Time) (int64, error) {
	settled, err := d.sprintsWhere(ctx,
		"state = ? AND rollover_done = 1 AND archived = 0", string(SprintClosed))
	if err != nil {
		return 0, err
	}
	var wrote int64
	for _, s := range settled {
		policy, held, err := d.sprintPolicy(ctx, s.Project)
		if err != nil {
			return wrote, err
		}
		if !held || policy.ArchiveAfter <= 0 {
			// ZERO IS OFF, which is the documented meaning — and the
			// right default: a company that never archives has a long
			// picker, and one that archives by accident has a sprint
			// its team cannot find.
			continue
		}
		newer, err := d.closedSprintsAfter(ctx, s.Project, s.Number)
		if err != nil {
			return wrote, err
		}
		if newer < policy.ArchiveAfter {
			continue
		}
		if _, err := d.deps.Writer.ArchiveSprint(ctx,
			d.opID("sprint_archive", s.key(), now), s.Project, s.Number); err != nil {
			d.deps.Logger.WarnContext(ctx, "tracker_sprint_archive_failed",
				"project", s.Project, "sprint", s.Number, "error", err)
			continue
		}
		wrote++
	}
	return wrote, nil
}

// mintAhead tops every sprinting project up to its policy's `ahead` count.
func (d *duty) mintAhead(ctx context.Context, now time.Time) (int64, error) {
	projects, err := d.sprintingProjects(ctx)
	if err != nil {
		return 0, err
	}
	var wrote int64
	for _, project := range projects {
		minted, err := d.deps.Writer.MintSprints(ctx,
			d.opID("sprint_mint", project, now), project, d.deps.Zone)
		if err != nil {
			d.deps.Logger.WarnContext(ctx, "tracker_sprint_mint_failed",
				"project", project, "error", err)
		}
		wrote += int64(len(minted))
	}
	return wrote, nil
}

// dutySprint is one sprint row the duty selected.
type dutySprint struct {
	Project    string
	Number     int
	RolloverTo string
}

func (s dutySprint) key() string { return fmt.Sprintf("%s.%d", s.Project, s.Number) }

// sprintsWhere reads the sprints one step selects.
//
// BOUNDED, like every selection here: a company whose duty has not run for a
// week has a backlog of transitions, and a tick that tried to do all of them
// in one transaction is a tick that times out and never does any.
func (d *duty) sprintsWhere(ctx context.Context, where string, args ...any) (
	[]dutySprint, error) {

	var out []dutySprint
	err := d.deps.DB.Replicated().Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT project_key, number, COALESCE(rollover_to, '')
			FROM tracker_sprints WHERE `+where+`
			ORDER BY project_key, number
			LIMIT ?`, append(args, SprintsPerTick)...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var s dutySprint
			if err := rows.Scan(&s.Project, &s.Number, &s.RolloverTo); err != nil {
				return err
			}
			out = append(out, s)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("tracker: select sprints (%s): %w", where, err)
	}
	return out, nil
}

// SprintsPerTick bounds how many sprint transitions one duty tick makes.
//
// SIXTY-FOUR, the same bound a rollover batch takes, and for the same reason:
// each is a published record the broker has to accept, and a company whose
// duty has been down for a week should catch up over several ticks rather than
// in one transaction that times out and achieves nothing. The duty runs every
// minute, so sixty-four a tick clears a year of backlog in under an hour.
const SprintsPerTick = 64

// sprintPolicy reads one project's sprint policy.
func (d *duty) sprintPolicy(ctx context.Context, project string) (
	SprintPolicy, bool, error) {

	var policy SprintPolicy
	var held bool
	err := d.deps.DB.Replicated().Read(ctx, func(tx *sql.Tx) error {
		p, found, err := readProject(ctx, tx, project)
		if err != nil || !found || p.Sprints == nil {
			return err
		}
		policy, held = *p.Sprints, true
		return nil
	})
	if err != nil {
		return SprintPolicy{}, false, fmt.Errorf(
			"tracker: read the sprint policy of %s: %w", project, err)
	}
	return policy, held, nil
}

// sprintingProjects is every project whose policy mints.
func (d *duty) sprintingProjects(ctx context.Context) ([]string, error) {
	var out []string
	err := d.deps.DB.Replicated().Read(ctx, func(tx *sql.Tx) error {
		// THE COLUMN RATHER THAN THE DOCUMENT: `sprint_policy_json` is
		// NULL on a project that runs none, so this is a scan of the
		// projects table with a predicate the planner can drive on
		// rather than a decode of every document.
		rows, err := tx.QueryContext(ctx, `
			SELECT key FROM tracker_projects
			WHERE sprint_policy_json IS NOT NULL AND archived = 0
			ORDER BY key LIMIT ?`, SprintsPerTick)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var key string
			if err := rows.Scan(&key); err != nil {
				return err
			}
			out = append(out, key)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("tracker: read the sprinting projects: %w", err)
	}
	return out, nil
}

// closedSprintsAfter counts how many of a project's sprints closed after one.
func (d *duty) closedSprintsAfter(ctx context.Context, project string, number int) (
	int, error) {

	var n int
	err := d.deps.DB.Replicated().Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM tracker_sprints
			WHERE project_key = ? AND number > ? AND state = ?`,
			project, number, string(SprintClosed)).Scan(&n)
	})
	if err != nil {
		return 0, fmt.Errorf("tracker: count the sprints after %d of %s: %w",
			number, project, err)
	}
	return n, nil
}

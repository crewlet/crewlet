package tracker

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
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
// re-spread a drag's long key asked for, a merge whose holder died
// mid-walk, a dependent nobody told. None of them is a scan looking for
// trouble, and that is the shape rather than an accident: a duty that goes
// looking is a duty that costs the same whether or not anything is wrong, on
// every node, for ever.
//
// So each one is GATED on a fact somebody already wrote down. A project's own
// row says it needs a re-spread; a task's own row says its merge walk has not
// finished; the repair holds its own position. The gate is one indexed read
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
	var unconfirmed []error
	for _, project := range projects {
		opID := d.opID("respread", project, now)
		report, err := d.deps.Writer.Respread(ctx, opID, project)
		walked += int64(report.Batches)
		switch {
		case errors.Is(err, ErrRespreadUnconfirmed):
			// THE BROKER COULD NOT CONFIRM THIS WALK'S BATCHES, which is a
			// fault and is reported as one: a warning here, and the job's
			// own error once every other flagged project has had its
			// walk, so a fault on one project's order does not hold the
			// rest up. The project stays flagged for the next sweep.
			d.deps.Logger.WarnContext(ctx, "tracker_respread_unconfirmed",
				"project", project, "batches", report.Batches,
				"plans", report.Plans, "unconfirmed", report.Unconfirmed,
				"moved", report.Moved, "error", err)
			unconfirmed = append(unconfirmed, err)
			continue
		case errors.Is(err, ErrRespreadYielded):
			// SOMEBODY IS ARRANGING THIS BOARD RIGHT NOW: every plan was
			// abandoned because the order moved under it, and the walk
			// gave way — see [RespreadPlans]. Nothing is wrong. The
			// project stays flagged and the next sweep plans from where
			// they left it; the other flagged projects are not held up
			// behind it.
			d.deps.Logger.InfoContext(ctx, "tracker_respread_yielded",
				"project", project, "batches", report.Batches,
				"plans", report.Plans, "error", err)
			continue
		case err != nil:
			return walked, errors.Join(append(unconfirmed, err)...)
		}
		// THE FLAG CLEARS ITSELF. The applier sets and clears it from
		// one probe over the project's own long keys, so the walk's
		// last batch is what turns it off — on every node, from that
		// node's own rows. A record clearing it here would be a second
		// owner, and a node whose applier had not caught up would clear
		// a flag its own rows still justify.
		d.deps.Logger.InfoContext(ctx, "tracker_respread_walked",
			"project", project, "batches", report.Batches,
			"plans", report.Plans)
	}
	return walked, errors.Join(unconfirmed...)
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
		placements, version, truncated, err := d.duplicatesIn(ctx, project)
		if err != nil {
			return fixed, err
		}
		if len(placements) > 0 {
			_, err := d.deps.Writer.MoveTasks(ctx,
				d.opID("dedupe", project, now), project, version, placements)
			if errors.Is(err, ErrOrderMoved) {
				// THE ORDER MOVED UNDER THE KEYS this sweep minted, so
				// they may no longer sit where the duplicates were. The
				// project stays flagged — nothing below ran — and the
				// next sweep mints from the order as it then is.
				d.deps.Logger.InfoContext(ctx, "tracker_rank_duplicates_raced",
					"project", project, "error", err)
				continue
			}
			if err != nil {
				return fixed, err
			}
			fixed += int64(len(placements))
			// THE CUT IS MARKED HERE OR IT IS MARKED NOWHERE, which is
			// the one-sided and unblocked repairs' rule applied to this
			// read: `tasks=64` is what a project holding exactly 64
			// duplicates and a project holding five thousand both
			// print, and one log line is the whole of what an operator
			// ever sees of this job. WHERE THE REST WENT: still
			// duplicated, so [duty.clearProbe]'s NOT EXISTS leaves the
			// project flagged and the next sweep reads them.
			d.deps.Logger.InfoContext(ctx, "tracker_rank_duplicates_cleared",
				"project", project, "tasks", len(placements),
				"truncated", truncated)
		}
		if err := d.clearProbe(ctx, project); err != nil {
			return fixed, err
		}
	}
	return fixed, nil
}

// duplicatesIn mints a fresh key for every task but the first at each shared
// rank, and says whether the project held more of them than one batch —
// with the order's version it read them at, which [Writer.MoveTasks] checks
// so keys minted from this read land only on the order they were minted in.
//
// THE FIRST BY ID KEEPS ITS KEY, so every node computes the same repair from
// the same rows — which is what makes running this twice a no-op rather than a
// second round of moves.
//
// THE CUT IS ASKED, NOT INFERRED, in [ScanOneSided]'s idiom: the query reads
// ONE ROW PAST [WalkBatch] and that row is dropped rather than placed, so its
// presence is the evidence. `len(losers) == WalkBatch` is a different fact — a
// project holding exactly a batch of duplicates holds all of them — and a full
// page read as a cut would mark every clean sweep truncated.
//
// NOTHING IS LOST TO THE BOUND. [duty.clearProbe] clears the project's flag
// only when NO duplicate is left, so a project this read cut stays selected
// and the next sweep reads the rest from the same query.
//
// # Why a cut sweep takes the HIGHEST ids at a shared rank
//
// The board breaks a tie on id, ascending, so the tasks at one key read in id
// order and the repair has to leave them in it. Every fresh key is above the
// shared one, so what a sweep moves ends up above what it leaves behind — and
// that is the tie order only when what it moves are the highest ids. Taking
// the lowest instead would put the ids a cut left at the shared key ahead of
// the ids it moved — the first batch of the tie jumping behind the rest of it,
// a reorder nobody asked for. The next sweep's ceiling is the lowest key this
// one minted, so what it moves lands below this sweep's and the order holds
// across every sweep it takes.
func (d *duty) duplicatesIn(ctx context.Context, project string) (
	[]Placement, int64, bool, error) {

	var losers []Placement
	var version int64
	var truncated bool
	ceilings := map[Rank]Rank{}
	err := d.deps.DB.Replicated().Read(ctx, func(tx *sql.Tx) error {
		var err error
		if version, err = OrderVersion(ctx, tx, project); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `
			SELECT t.id, t.rank FROM tracker_tasks t
			WHERE t.project_key = ? AND t.removed_at IS NULL
			  AND EXISTS (SELECT 1 FROM tracker_tasks o
			              WHERE o.project_key = t.project_key
			                AND o.rank = t.rank AND o.id < t.id)
			-- ONE ROW PAST THE BOUND: the extra row is evidence that
			-- the project holds more duplicates than this sweep
			-- re-mints, never an answer. HIGHEST ID FIRST within a
			-- rank, so a cut leaves the lowest ids at the shared key.
			ORDER BY t.rank, t.id DESC LIMIT ?`, project, WalkBatch+1)
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
		if err := rows.Err(); err != nil {
			return err
		}
		truncated = len(losers) > WalkBatch
		if truncated {
			losers = losers[:WalkBatch]
		}
		// BACK INTO THE BOARD'S OWN ORDER, id ascending within a rank, so
		// the mint below chains each rank's losers upward in the order a
		// tie already reads them in.
		slices.SortFunc(losers, func(a, b Placement) int {
			return cmp.Or(strings.Compare(string(a.Rank), string(b.Rank)),
				strings.Compare(a.Task, b.Task))
		})
		// THE NEXT KEY ABOVE EACH SHARED ONE, in this same snapshot: it is
		// the ceiling every fresh key for that rank must stay under, or
		// the repair would carry a card past a neighbour somebody put
		// above it.
		for _, loser := range losers {
			if _, held := ceilings[loser.Rank]; held {
				continue
			}
			ceiling, err := rankAbove(ctx, tx, project, loser.Rank)
			if err != nil {
				return err
			}
			ceilings[loser.Rank] = ceiling
		}
		return nil
	})
	if err != nil || len(losers) == 0 {
		return nil, 0, false, err
	}
	// A FRESH KEY JUST ABOVE THE ONE THEY SHARE AND BELOW THE NEXT ONE UP,
	// which keeps each duplicate adjacent to where somebody put it. The
	// losers of one rank are in id order here and each mints above the
	// last, so the tasks sharing a key leave in the order a tie already
	// drew them in.
	placements := make([]Placement, 0, len(losers))
	minted := map[Rank]Rank{}
	for _, loser := range losers {
		floor := loser.Rank
		if last, held := minted[loser.Rank]; held {
			floor = last
		}
		next, err := KeyBetween(floor, ceilings[loser.Rank])
		if err != nil {
			return nil, 0, false, fmt.Errorf("tracker: mint a key between %q and "+
				"%q for %s: %w", floor, ceilings[loser.Rank], loser.Task, err)
		}
		minted[loser.Rank] = next
		placements = append(placements, Placement{Task: loser.Task, Rank: next})
	}
	return placements, version, truncated, nil
}

// rankAbove is the lowest key in the project strictly above rank, or — when
// rank is the highest — the next integer position above it.
//
// NEVER AN OPEN END. [KeyBetween] with an empty upper bound walks the CREATE
// lattice, so a repair minted that way is the next pure integer: it carries
// the card past every key between, and it can land on a key a task already
// holds or on the one the next create mints. A create's key is strictly the
// new maximum and a pure integer, so it is never below the next integer
// position above the current maximum — and a key under that bound is one no
// create can collide with.
//
// A REMOVED TASK'S KEY COUNTS, as it does in the applier's own duplicate
// probe: a restore brings it back where it was, and a repair that took its
// key would make the restore the next duplicate.
func rankAbove(ctx context.Context, tx *sql.Tx, project string, rank Rank) (Rank, error) {
	var above sql.NullString
	if err := tx.QueryRowContext(ctx, `
		SELECT MIN(rank) FROM tracker_tasks
		WHERE project_key = ? AND rank > ?`, project, string(rank)).
		Scan(&above); err != nil {
		return "", fmt.Errorf("tracker: read the key above %q in %s: %w",
			rank, project, err)
	}
	if above.Valid {
		return Rank(above.String), nil
	}
	integer, err := integerPart(string(rank))
	if err != nil {
		return "", err
	}
	next, err := incrementInteger(integer)
	if err != nil {
		return "", err
	}
	return Rank(next), nil
}

// rankBelow is the highest key in the project strictly below rank, or empty
// when nothing is below it.
//
// [rankAbove]'s mirror, for a drop at the head of what somebody saw: a key
// minted with no lower bound is the integer below `rank`'s own, which can
// carry the card past a card the board was not showing, or land on its key.
// Empty is safe where it is returned, because then nothing sits below `rank`
// for a head placement to pass. A removed task's key counts, for
// [rankAbove]'s reason.
func rankBelow(ctx context.Context, tx *sql.Tx, project string, rank Rank) (Rank, error) {
	var below sql.NullString
	if err := tx.QueryRowContext(ctx, `
		SELECT MAX(rank) FROM tracker_tasks
		WHERE project_key = ? AND rank < ?`, project, string(rank)).
		Scan(&below); err != nil {
		return "", fmt.Errorf("tracker: read the key below %q in %s: %w",
			rank, project, err)
	}
	return Rank(below.String), nil
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

// finishMerges completes every merge whose walk was abandoned — its holder
// died or gave up, which the merge's claim says ([duty.finishAbandoned]).
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
	var truncated bool
	if err := d.deps.DB.Replicated().Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			// ONE ROW PAST THE BOUND: the extra row is evidence that
			// more merges are abandoned than this sweep completes,
			// never an answer. See the log line at the end of this
			// function for what reads it.
			`SELECT id FROM tracker_tasks WHERE merging = 1 ORDER BY id LIMIT ?`,
			WalkBatch+1)
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
		if err := rows.Err(); err != nil {
			return err
		}
		// THE PROBE ROW IS DROPPED RATHER THAN COMPLETED, so the number
		// of merges one tick walks is exactly the bound. `len(stuck) ==
		// WalkBatch` is a different fact from a cut: a company holding
		// exactly a batch of abandoned merges holds all of them.
		truncated = len(stuck) > WalkBatch
		if truncated {
			stuck = stuck[:WalkBatch]
		}
		return nil
	}); err != nil {
		return 0, err
	}

	var finished int64
	for _, id := range stuck {
		done, err := d.finishAbandoned(ctx, id, now)
		if err != nil {
			return finished, err
		}
		if done {
			finished++
		}
	}
	if finished > 0 {
		// THE SWEEP'S OWN LINE, and it exists for the mark rather than
		// for the count: the per-merge lines above already say what was
		// completed, and nothing in them distinguishes a tick that
		// finished every abandoned merge in the company from one that
		// finished the first batch of thousands. WHERE THE REST WENT:
		// still carrying `merging = 1`, which is the gate this job
		// selects on, so the next sweep reads them.
		//
		// `finished` IS THE WHOLE OF WHAT THIS TICK CARRIED. Every
		// iteration above counts, returns, or passes over a merge whose
		// walk is still running, which is not abandoned and so no part of
		// what this repair owes. A shortfall inside the batch is
		// therefore an error the worker reports rather than a number that
		// has to be subtracted here — unlike the one-sided repair, whose
		// per-commit failures are deliberately swallowed.
		d.deps.Logger.InfoContext(ctx, "tracker_abandoned_merges_finished",
			"merges", finished, "truncated", truncated)
	}
	return finished, nil
}

// finishAbandoned completes one merge under the merge's own claim, and reports
// whether it did.
//
// # A merge is abandoned when nothing holds its claim, and only then
//
// The marker is raised by the mark and lowered by the close, so it stands for
// the whole of every walk, a live one included — the row alone cannot tell a
// walk whose holder died from one still running. Its claim can: a holder that
// returns gives it back ([Writer.MergeDuplicates] releases on every path), and
// one that died loses it when its lease runs out. So a claim somebody still
// holds, on this node or another, is a walk left to finish, and the next sweep
// reads the marker again if it outlives that walk. Run beside it instead, this
// repair would re-read the same subtasks and publish a second re-parent for
// each one the walk had not moved yet, and a second close of the duplicate.
//
// AND A MERGE THAT ENDED IS NOT FINISHED AGAIN. This node's rows can be older
// than the log — a walk that closed on another node gives its claim back before
// this node applies the close — so every append the repair makes reads the
// merge as still running in its own decide ([Writer.whileMerging]), and one
// that has ended writes nothing and is not counted.
func (d *duty) finishAbandoned(ctx context.Context, id string, now time.Time) (bool, error) {
	claim, err := d.deps.Writer.claim(ctx, mergeClaim(id))
	switch {
	case err != nil:
		return false, err
	case claim == nil:
		return false, nil
	}
	defer claim.release(ctx)

	walk, err := d.abandonedMerge(ctx, id)
	if err != nil {
		return false, err
	}
	opID := d.opID("merge", id, now)
	if walk.into == "" {
		// MID-MERGE WITH NO TARGET is a marker whose relation never
		// landed. The honest repair is to clear the marker and NOTHING
		// ELSE: the merge did not happen and now cannot, so cancelling the
		// task would close an item nobody merged — and leaving the flag set
		// would make this tick run for ever against a task nothing is
		// merging.
		d.deps.Logger.WarnContext(ctx, "tracker_merge_marker_without_target",
			"task", id, "detail", "the marker is cleared and the task left "+
				"open; the merge it names never linked anything")
		done := false
		_, err = d.deps.Writer.whileMerging().UpdateTask(ctx, opID, walk.task,
			walk.project, NoIfMatch, TaskPatch{Merging: &done}, ChangeFields, nil)
		switch {
		case errors.Is(err, errMergeOver):
			return false, nil
		case err != nil:
			return false, err
		}
		return true, nil
	}
	// THE REST OF THE SEQUENCE ITSELF, from where its holder stopped — and
	// whether the target is still there is read by each of its steps, in
	// the snapshot that step is decided from, rather than by this sweep's
	// scan, which is older than all of them. A target purged since the
	// mark gives the merge up there, exactly as it does for a holder that
	// is still alive ([Writer.finishMerge]).
	end, err := d.deps.Writer.finishMerge(ctx, opID, walk.task, walk.project,
		walk.into, walk.reparent, statelog.Position{}, nil)
	switch {
	case errors.Is(err, errMergeOver):
		// ENDED BEFORE THIS SWEEP REACHED IT: its close or its give-up
		// landed on the log after the scan read this node's rows, which
		// is nothing for a repair to finish.
		return false, nil
	case err != nil:
		return false, err
	}
	if end.Purged != nil {
		d.deps.Logger.WarnContext(ctx, "tracker_merge_target_purged",
			"task", id, "target", walk.into, "detail", "the task this one "+
				"was being merged into was purged; the marker is cleared and "+
				"the task left open with the subtasks it still has")
		return true, nil
	}
	d.deps.Logger.InfoContext(ctx, "tracker_merge_completed",
		"task", id, "into", walk.into, "subtasks_moved", end.Moved)
	return true, nil
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

// abandonedMerge reads a mid-merge task's own account of its walk.
//
// NOT WHETHER THE TARGET IS STILL THERE: that is read by each step the repair
// makes, in its own decide ([Writer.finishMerge]). Read here, it would answer
// for the moment of this scan, and a purge landing after it would see subtasks
// re-parented onto a task no row holds.
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
		// THE CUT IS MARKED HERE OR IT IS MARKED NOWHERE. A tick that
		// told 64 people because 64 were owed and a tick that told 64 of
		// a thousand publish the same `dependents=64`, and the second is
		// a company hours behind on its late notices — so the scan's own
		// evidence row ([UnblockScan.Truncated]) is carried onto the
		// line rather than dropped, and this is its one reader.
		//
		// WHERE THE REST WENT: still owed, still inside this window, and
		// carried by the next sweep, because a truncated scan reports
		// the position it was GIVEN (see [UnblockScan.Through]) and the
		// assignment above therefore leaves `unblockedThrough` where it
		// was. Nothing is lost to the bound; only the delay is, which is
		// what the flag is for.
		d.deps.Logger.InfoContext(ctx, "tracker_unblocked_told",
			"dependents", told, "through", scan.Through,
			"truncated", scan.Truncated)
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
// owns.
//
// AND THIS IS THE ONLY CLEAR THERE IS, which is worth stating because this
// comment used to say otherwise ("every node clears its own the next time it
// applies a rank move that finds no duplicate") and the apply path does no
// such thing: apply_objects.go sets the flag to 1 on a colliding rank and
// writes 0 only on a project's first INSERT, never in the upsert that follows.
// So a peer that probed a duplicate carries the flag until it restarts, and
// only the node holding this fleet-singleton duty ever writes a 0.
//
// That divergence is bounded rather than harmless: the column is deliberately
// outside the identity claim (see [Domain.ClaimsIdentity]), nothing reads it
// as a fleet-wide fact, and the duplicate itself is repaired by a published
// record every node applies. Making the clear deterministic — every applier
// clearing the flag when it applies the duty's repair, re-probing under this
// same guard, and this local write going away — is a change to what a record
// owns, which the apply path's own comment has weighed and declined once
// before ("a company-wide aggregate on every drag is the per-minute scan no
// index answers"), so it wants its own change rather than this one.
//
// The guard below is also why this clears nothing on the tick that publishes
// a repair: the repair has not been applied yet, so the NOT EXISTS still
// sees the duplicates, and it is the NEXT tick that clears.
//
// A POOLED TRANSACTION, NOT A PIN, for the reason purgeInbox carries: every
// pin the replicated estate declares belongs to an applier for the life of
// the process, so a repair asking for one of its own was refused on every
// tick of a running node.
func (d *duty) clearProbe(ctx context.Context, project string) error {
	return d.deps.DB.Replicated().Tx(ctx, func(tx *sql.Tx) error {
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
	scan, err := ScanOneSided(ctx, d.deps.DB, now.Add(-OneSidedRepairAge), WalkBatch)
	if err != nil {
		return 0, err
	}
	var repaired int64
	// ONE COMMIT PER SUBJECT, which is what makes a blocker several broken
	// edges name repairable at all: two records on one subject in one tick
	// cannot both be decided here, and the second is refused `behind`.
	// [PlanOneSided] states the whole of that grouping.
	for _, commit := range PlanOneSided(scan.Edges) {
		if _, err := d.deps.Writer.RepairOneSided(ctx,
			d.opID("onesided", commit.Task, now), commit, d.leads()); err != nil {
			// ONE COMMIT'S FAILURE IS NOT THE TICK'S. The rest of this
			// batch is independent — different subjects, different
			// blockers — and stopping here would let one wedged
			// counterparty hold up every other repair in the company.
			// The edges it carried are counted as left behind below.
			d.deps.Logger.WarnContext(ctx, "tracker_one_sided_repair_failed",
				"task", commit.Task, "mirrored", len(commit.Mirror),
				"stamped", len(commit.Final), "error", err)
			continue
		}
		repaired += int64(len(commit.Mirror) + len(commit.Final))
		for _, edge := range commit.Final {
			reason, _ := edge.Final()
			d.deps.Logger.InfoContext(ctx, "tracker_one_sided_final",
				"dependent", edge.Dependent, "blocker", edge.Blocker,
				"reason", reason)
		}
	}
	// TWO DIFFERENT SHORTFALLS, and one of them alone is the same silent
	// cut as neither.
	//
	// `truncated` is the SCAN's: the window held more broken edges than
	// this tick read at all, evidenced by the probe row. `deferred` is
	// THIS TICK's, over the edges it did read — a commit that failed, and
	// the edges past a blocker's remaining room that [PlanOneSided]
	// deliberately left for the sweep that will stamp them final. Without
	// it a tick that carried 64 edges and landed one prints the same
	// `edges` and the same `truncated` as a tick that landed all 64.
	//
	// WHERE THEY ALL WENT: still flagged `one_sided` on their own rows, so
	// the next sweep's indexed read finds them — this repair keeps no
	// position to lose them behind.
	deferred := int64(len(scan.Edges)) - repaired
	if repaired > 0 || deferred > 0 {
		d.deps.Logger.InfoContext(ctx, "tracker_one_sided_repaired",
			"edges", repaired, "deferred", deferred,
			"truncated", scan.Truncated)
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

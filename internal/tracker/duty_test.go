package tracker_test

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/maintenance"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// EVERY DUTY JOB IS GATED ON A FACT SOMEBODY WROTE DOWN.
//
// # Why that is the property rather than "the duty is correct"
//
// A duty that goes looking costs the same whether or not anything is wrong, on
// every node, for ever — and this one runs every fifteen minutes against a
// company with hundreds of thousands of tasks. So each job asks one indexed
// question first, and on a healthy company a whole tick is those questions and
// nothing else.
//
// The exception is named rather than hidden: the missed-unblocked repair has
// no flag to read, because the fact it looks for is a comparison between two
// instants rather than something a writer could have stamped. It is bounded by
// its own last position instead.
func TestEveryTrackerJobIsGatedOrSaysWhyNot(t *testing.T) {
	t.Parallel()
	jobs := tracker.Jobs(tracker.DutyDeps{NodeID: "node-a"})
	if len(jobs) == 0 {
		t.Fatal("the tracker registers no duty jobs, so this case is " +
			"measuring its own reader")
	}
	ungated := map[string]bool{
		// The repair has no flag to read: what it looks for is a
		// comparison between a blocker's clearing instant and what its
		// dependent was told, and neither is a column a writer stamps.
		"tracker_unblocked": true,
	}
	for _, job := range jobs {
		if job.Gate == nil && !ungated[job.Name] {
			t.Errorf("%s runs its own selection on every tick — a duty that "+
				"goes looking costs the same whether or not anything is "+
				"wrong, on every node, for ever", job.Name)
		}
		if job.Scope != maintenance.Fleet {
			t.Errorf("%s has scope %q and every tracker job publishes "+
				"RECORDS — running one on every node is N copies of one "+
				"record for the applier to arbitrate", job.Name, job.Scope)
		}
	}
}

// A TICK ON A HEALTHY COMPANY READS ITS GATES AND DOES NOTHING ELSE.
func TestAQuietTickRunsNoJob(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if _, err := r.writer.CreateTask(t.Context(), "op-1", newTask("t-1"), nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r.drain()

	swept, err := trackerWorker(t, r).Tick(t.Context())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if len(swept) != 0 {
		t.Fatalf("a tick against a healthy company did %v — every job is "+
			"gated on a fact somebody wrote down, and nobody wrote one",
			swept)
	}
}

// THE DUTY FINISHES A RE-SPREAD THE INLINE WINDOW COULD NOT.
//
// A drag past the inline cap still succeeds with its long key, and the applier
// flags the project from that key's own length — on every node, so the fact is
// the fleet's rather than the writer's. The duty is what turns that flag into
// a walk.
func TestTheDutyWalksAFlaggedProject(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	const total = 6
	for i := range total {
		if _, err := r.writer.CreateTask(t.Context(), fmt.Sprintf("op-%d", i),
			newTask(fmt.Sprintf("t-%02d", i)), nil); err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
		r.drain()
	}
	crowd(t, r, total)
	if !flagged(t, r, "rank_respread_pending") {
		t.Fatal("the applier did not flag a project whose keys are past the " +
			"threshold, so the duty has nothing to be triggered by")
	}

	worker := trackerWorker(t, r)
	swept, err := worker.Tick(t.Context())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if swept["tracker_respread"] == 0 {
		t.Fatalf("the duty walked nothing against a flagged project: %v", swept)
	}
	r.drain()

	for _, rank := range boardRanks(t, r) {
		if len(rank) > tracker.RankRenormaliseAt {
			t.Errorf("the duty left rank %q at %d characters", rank, len(rank))
		}
	}
	if flagged(t, r, "rank_respread_pending") {
		t.Error("the project is still flagged after its walk, so the duty " +
			"walks it again on every tick for ever")
	}
}

// THE DUTY GIVES ONE OF TWO TASKS SHARING A RANK A FRESH KEY.
//
// A duplicate is a COSMETIC anomaly — two cards whose order is undefined
// between them — and it is repaired rather than refused, because a unique
// index here would turn it into a deterministic fleet-wide stalled log.
func TestTheDutyClearsADuplicateRank(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for _, id := range []string{"t-1", "t-2"} {
		if _, err := r.writer.CreateTask(t.Context(), "op-"+id,
			newTask(id), nil); err != nil {
			t.Fatalf("CreateTask %s: %v", id, err)
		}
		r.drain()
	}
	// ONE RECORD PUTTING BOTH AT ONE KEY, which is what two concurrent
	// drags into the same gap produce on two nodes.
	if _, err := r.writer.MoveTasks(t.Context(), "op-collide", "ENG", r.order("ENG"),
		[]tracker.Placement{
			{Task: "t-1", Rank: "a0V"}, {Task: "t-2", Rank: "a0V"},
		}); err != nil {
		t.Fatalf("MoveTasks: %v", err)
	}
	r.drain()
	if !flagged(t, r, "rank_duplicate_pending") {
		t.Fatal("the applier's own probe did not notice two tasks at one " +
			"rank, so nothing hands the repair to the duty")
	}

	// THE TICK RUNS BESIDE A LIVE APPLIER, which holds the estate's pin for
	// the life of the process: a repair that asked for one of its own was
	// refused on every tick of a running node.
	holdTheAppliersPin(t, r)
	if _, err := trackerWorker(t, r).Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	r.drain()

	ranks := boardRanks(t, r)
	if len(ranks) != 2 || ranks[0] == ranks[1] {
		t.Fatalf("the board still reads %v — the repair mints a fresh key for "+
			"one of them so the order between the two is defined", ranks)
	}
}

// A REPAIRED DUPLICATE STAYS WHERE SOMEBODY PUT IT.
//
// The fresh key has to land above the one the tasks share and below the next
// key up. Minted with no upper bound it is the next pure integer instead —
// the create lattice's own shape — which carries the card past every task
// between and can land on a key another task already holds.
func TestARepairedDuplicateStaysWhereSomebodyPutIt(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for _, id := range []string{"t-1", "t-2", "t-3", "t-4", "t-5"} {
		if _, err := r.writer.CreateTask(t.Context(), "op-"+id,
			newTask(id), nil); err != nil {
			t.Fatalf("CreateTask %s: %v", id, err)
		}
		r.drain()
	}
	// THREE AT ONE KEY, with a neighbour just above them.
	if _, err := r.writer.MoveTasks(t.Context(), "op-collide", "ENG", r.order("ENG"),
		[]tracker.Placement{
			{Task: "t-1", Rank: "a0V"}, {Task: "t-2", Rank: "a0V"},
			{Task: "t-4", Rank: "a0V"}, {Task: "t-3", Rank: "a0X"},
		}); err != nil {
		t.Fatalf("MoveTasks: %v", err)
	}
	r.drain()
	want := boardOrder(t, r)

	holdTheAppliersPin(t, r)
	if _, err := trackerWorker(t, r).Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	r.drain()

	if got := boardOrder(t, r); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("the repair reordered the board: it read %v and reads %v — "+
			"a fresh key has to stay below the next key up", want, got)
	}
	seen := map[tracker.Rank]bool{}
	for _, rank := range boardRanks(t, r) {
		if seen[rank] {
			t.Fatalf("the board still holds two tasks at %q", rank)
		}
		seen[rank] = true
	}
}

// A SWEEP CUT SHORT LEAVES THE BOARD IN ITS OWN ORDER.
//
// More tasks share one key than one sweep re-mints, so the repair takes two
// sweeps and the board is read between them. The board breaks the tie on id,
// and a cut sweep that moved the LOWEST ids above the shared key would leave
// the highest ones at it — reading them ahead of the ones it moved, a reorder
// nobody asked for, in the middle of a repair that exists only to make an
// undefined order a defined one without changing what anybody sees.
func TestASweepCutShortLeavesTheBoardInItsOwnOrder(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	// TWO PAST A SWEEP'S BATCH: the first task keeps the key, so the
	// losers are one more than a sweep carries and the first sweep cuts.
	tasks := tracker.WalkBatch + 2
	placements := make([]tracker.Placement, 0, tasks)
	for i := range tasks {
		id := fmt.Sprintf("t-%03d", i)
		if _, err := r.writer.CreateTask(t.Context(), "op-"+id,
			newTask(id), nil); err != nil {
			t.Fatalf("CreateTask %s: %v", id, err)
		}
		r.drain()
		placements = append(placements, tracker.Placement{Task: id, Rank: "a0V"})
	}
	for from := 0; from < len(placements); from += tracker.MaxBulkTasks {
		batch := placements[from:min(from+tracker.MaxBulkTasks, len(placements))]
		if _, err := r.writer.MoveTasks(t.Context(),
			fmt.Sprintf("op-collide-%d", from), "ENG", r.order("ENG"), batch); err != nil {
			t.Fatalf("MoveTasks: %v", err)
		}
		r.drain()
	}
	want := boardOrder(t, r)
	if len(want) != tasks {
		t.Fatalf("the board read %d of %d tasks, so this case would compare "+
			"part of an order", len(want), tasks)
	}

	holdTheAppliersPin(t, r)
	// A CLOCK THAT MOVES BETWEEN SWEEPS, as a deployment's does: the
	// repair's operation id is derived from the tick, so two sweeps at one
	// instant would dedupe the second against the first and prove nothing
	// about what it reads.
	at := wednesday
	worker, err := maintenance.New(maintenance.Options{
		Jobs: tracker.Jobs(tracker.DutyDeps{
			DB: r.db, Writer: r.writer, NodeID: "node-a",
		}),
		Now: func() time.Time { return at },
	})
	if err != nil {
		t.Fatalf("build the tracker's maintenance worker: %v", err)
	}
	for sweep := 1; sweep <= 2; sweep++ {
		at = at.Add(time.Hour)
		if _, err := worker.Tick(t.Context()); err != nil {
			t.Fatalf("sweep %d: %v", sweep, err)
		}
		r.drain()
		if got := boardOrder(t, r); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("after sweep %d the board reads %v, want the tie order %v "+
				"— a cut sweep must leave its lower ids at the shared key, "+
				"below the higher ones it moved", sweep, got, want)
		}
	}
	// AND THE SECOND SWEEP READ WHAT THE FIRST CUT: an order that held
	// only because nothing moved would pass the comparison above.
	seen := map[tracker.Rank]bool{}
	for _, rank := range boardRanks(t, r) {
		if seen[rank] {
			t.Fatalf("after two sweeps the board still holds two tasks at %q",
				rank)
		}
		seen[rank] = true
	}
}

// A DUPLICATE AT THE TOP IS REPAIRED BELOW THE NEXT CREATE.
//
// With nothing above the shared key, the ceiling is the next integer position
// — the one the next create mints at — so the repaired card stays under every
// task a create will ever add, rather than taking that key itself.
func TestADuplicateAtTheTopIsRepairedBelowTheNextCreate(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for _, id := range []string{"t-1", "t-2"} {
		if _, err := r.writer.CreateTask(t.Context(), "op-"+id,
			newTask(id), nil); err != nil {
			t.Fatalf("CreateTask %s: %v", id, err)
		}
		r.drain()
	}
	top := boardRanks(t, r)[1]
	if _, err := r.writer.MoveTasks(t.Context(), "op-collide", "ENG", r.order("ENG"),
		[]tracker.Placement{{Task: "t-1", Rank: top}}); err != nil {
		t.Fatalf("MoveTasks: %v", err)
	}
	r.drain()

	holdTheAppliersPin(t, r)
	if _, err := trackerWorker(t, r).Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	r.drain()
	if _, err := r.writer.CreateTask(t.Context(), "op-t-3", newTask("t-3"),
		nil); err != nil {
		t.Fatalf("CreateTask t-3: %v", err)
	}
	r.drain()

	if got := boardOrder(t, r); strings.Join(got, ",") != "t-1,t-2,t-3" {
		t.Fatalf("the board reads %v, want t-1,t-2,t-3 — the repaired card "+
			"must stay below the task the next create adds", got)
	}
	ranks := boardRanks(t, r)
	if ranks[1] == ranks[2] {
		t.Fatalf("the repair and the next create share %q", ranks[1])
	}
	if ranks[1].Fraction() == "" {
		t.Errorf("the repair minted %q, a pure integer — that is a create's "+
			"shape, and the two mint sets are disjoint only while a move never "+
			"produces one", ranks[1])
	}
}

// A MARKER THAT NAMES NO TARGET IS CLEARED AND NOTHING ELSE.
//
// A mark at [tracker.MergeRecordVersion] carries its target, and the writer
// refuses one without it. A mark at the first version carries none, and names
// its target only through the `duplicates` edge it added — so one whose task
// holds no such edge names nothing the merge could be finished into.
// Cancelling the task here would close an item into nothing, and leaving the
// flag set would run this job against it on every tick for ever.
func TestTheDutyClearsAnAbandonedMerge(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for _, id := range []string{"t-1", "t-2"} {
		if _, err := r.writer.CreateTask(t.Context(), "op-"+id,
			newTask(id), nil); err != nil {
			t.Fatalf("CreateTask %s: %v", id, err)
		}
		r.drain()
	}
	// THE MARKER WITHOUT THE REST, at the first version and with no edge.
	r.appendRecord(firstVersionMark("t-1"))
	r.drain()

	swept, err := trackerWorker(t, r).Tick(t.Context())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if swept["tracker_abandoned_merges"] == 0 {
		t.Fatalf("the duty completed no abandoned merge: %v — a task left "+
			"mid-merge is visibly half-merged for ever, which is the state "+
			"the marker exists to make somebody fix", swept)
	}
	r.drain()

	var merged int
	if err := r.db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(),
			`SELECT merging FROM tracker_tasks WHERE id = 't-1'`).Scan(&merged)
	}); err != nil {
		t.Fatalf("read the marker: %v", err)
	}
	if merged != 0 {
		t.Error("the marker is still set after the duty completed the merge, " +
			"so the duty runs against it on every tick for ever")
	}
	// AND THE TASK IS STILL OPEN. Nothing was merged into anything, so
	// closing it would be the duty inventing the outcome of a gesture that
	// never linked two items.
	if got := r.task(t, "t-1").Task.Status; got == tracker.StatusCancelled {
		t.Error("the duty cancelled a task whose merge named no target — the " +
			"marker is all that landed, so there is no merge to complete")
	}
}

// THE DUTY FINISHES THE MERGE, rather than tidying away its marker.
//
// The residue a holder that died leaves is: the mark landed, some subtasks
// moved, the close did not. Clearing the marker alone left the duplicate OPEN
// and half-merged for ever — on a board, a live item linked as a duplicate of
// another live item, with its subtasks still under it. That is the exact
// state the merge exists to remove, and the repair reached it and stopped.
func TestTheDutyFinishesAnAbandonedMergeRatherThanTidyingIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		reparent bool
		want     string
	}{
		// AND IT RE-PARENTS ONLY IF THE MERGE SAID TO. A repair that
		// guessed would silently override a `move_subtasks: false`
		// somebody typed, which is the one direction that cannot be
		// undone by waiting.
		{"a walk that meant to move the subtasks", true, "keep"},
		{"a walk that was told to leave them", false, "dup"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRoundTrip(t)
			for _, id := range []string{"keep", "dup"} {
				if _, err := r.writer.CreateTask(t.Context(), "op-"+id,
					newTask(id), nil); err != nil {
					t.Fatalf("CreateTask %s: %v", id, err)
				}
				r.drain()
			}
			parent := "dup"
			kid := newTask("kid")
			kid.Parent, kid.Depth = &parent, 1
			if _, err := r.writer.CreateTask(t.Context(), "op-kid", kid, nil); err != nil {
				t.Fatalf("CreateTask kid: %v", err)
			}
			r.drain()

			// EXACTLY THE MARK [tracker.Writer.MergeDuplicates]
			// publishes, and then nothing — which is a holder that
			// died between its first append and its last.
			merging := true
			if _, err := r.writer.UpdateTask(t.Context(), "op-mark", "dup", "ENG",
				tracker.NoIfMatch, tracker.TaskPatch{
					Relate: &tracker.RelationIntent{Add: []tracker.Relation{{
						Kind: tracker.RelationDuplicates, Other: "keep",
					}}},
					Merging: &merging, MergeReparent: &tc.reparent, MergeInto: ptr("keep"),
				}, tracker.ChangeRelations, nil); err != nil {
				t.Fatalf("UpdateTask mark: %v", err)
			}
			r.drain()

			swept, err := trackerWorker(t, r).Tick(t.Context())
			if err != nil {
				t.Fatalf("Tick: %v", err)
			}
			if swept["tracker_abandoned_merges"] == 0 {
				t.Fatalf("the duty completed no abandoned merge: %v", swept)
			}
			r.drain()

			dup := r.task(t, "dup")
			if dup.Task.Status != tracker.StatusCancelled {
				t.Errorf("the duplicate is %q after the duty finished its "+
					"merge — an open item linked as a duplicate of another "+
					"open item is what the fold exists to remove",
					dup.Task.Status)
			}
			if dup.Task.Merging {
				t.Error("the marker is still set, so this job runs against " +
					"the same task on every tick for ever")
			}
			if got := parentOf(r.task(t, "kid")); got != tc.want {
				t.Errorf("the subtask's parent is %q, want %q", got, tc.want)
			}
		})
	}
}

// A DUTY REPAIR'S SWEEP SAYS WHEN IT LEFT SOMETHING BEHIND.
//
// # Why the flag has to reach the log line
//
// The repair tells at most [tracker.WalkBatch] dependents a sweep and holds
// its position on a window it could not finish, so nobody is lost — but one
// log line is the whole of what an operator ever sees of it, and a sweep that
// told 64 because 64 were owed prints the same `dependents=64` as a sweep 64
// into a thousand-strong backlog. The second is a company four hours from
// having told everybody. The scan's probe row is the only thing that knows the
// difference ([tracker.UnblockScan.Truncated]) and this line is its only
// reader, so a flag that does not reach it is a cut nothing marks.
//
// TWO-SIDED ON PURPOSE: the cut sweep must say `truncated=true` AND the sweep
// that drains the rest must say `truncated=false`, because a value that is
// always the same is evidence of nothing.
//
// BOTH REPAIRS ARE ASSERTED HERE because one fixture produces both. Every
// create below authors `waiting_on` and nothing writes the blocker's mirror,
// so the same sixty-five tasks leave sixty-five one-sided edges for
// [tracker.ScanOneSided] — and the twin's line has the identical failure if
// its own flag stops reaching the log.
func TestADutyRepairSweepSaysWhatItLeftBehind(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	// ONE MORE THAN A SWEEP CARRIES. The duty passes [tracker.WalkBatch]
	// itself rather than a number a case may choose, so this is the
	// smallest fixture that makes the real batch cut.
	dependents := tracker.WalkBatch + 1
	blocker := newTask("t-0")
	if _, err := r.writer.CreateTask(t.Context(), "op-t-0", blocker, nil); err != nil {
		t.Fatalf("CreateTask t-0: %v", err)
	}
	r.drain()
	for i := range dependents {
		id := fmt.Sprintf("d-%03d", i)
		dependent := newTask(id)
		// AN ASSIGNEE EACH, because the notice has no other recipient
		// and the scan skips a dependent with nobody to tell.
		dependent.Assignee = "alice"
		dependent.Relations = []tracker.Relation{{
			Kind: tracker.RelationWaitingOn, Other: "t-0",
		}}
		if _, err := r.writer.CreateTask(t.Context(), "op-"+id, dependent, nil); err != nil {
			t.Fatalf("CreateTask %s: %v", id, err)
		}
		// DRAINED BETWEEN THE CREATES: they mint keys from one project
		// counter, and a create cannot decide against a number this node
		// has not applied.
		r.drain()
	}
	// ONE CLOSE MAKES ALL OF THEM WORKABLE, which is what puts more
	// dependents behind a single log record than one sweep may carry.
	done := tracker.StatusDone
	if _, err := r.writer.UpdateTask(t.Context(), "op-close", "t-0", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Status: &done}, tracker.ChangeStatus,
		&tracker.Notify{Kind: tracker.ChangeStatus}); err != nil {
		t.Fatalf("close the blocker: %v", err)
	}
	r.drain()

	logged := &capturedLog{}
	worker := trackerWorkerLogging(t, r, slog.New(logged))
	if _, err := worker.Tick(t.Context()); err != nil {
		t.Fatalf("the first sweep: %v", err)
	}
	r.drain()
	cut := logged.only(t, "tracker_unblocked_told")
	if got := cut.attrs["dependents"]; got != int64(tracker.WalkBatch) {
		t.Fatalf("the sweep told %v dependents, want the batch of %d — the "+
			"fixture is not exercising the bound", got, tracker.WalkBatch)
	}
	if cut.attrs["truncated"] != true {
		t.Errorf("a sweep that carried %d of %d owed dependents logged %+v — "+
			"an operator reading this cannot tell a backlog from a healthy "+
			"tick, and the backlog is the one that takes hours to drain",
			tracker.WalkBatch, dependents, cut.attrs)
	}
	// THE TWIN, over the one-sided edges the same creates left behind.
	if mirrors := logged.only(t, "tracker_one_sided_repaired"); mirrors.attrs["truncated"] != true {
		t.Errorf("the mirror repair carried %d of %d broken edges and logged "+
			"%+v — the same line a sweep with nothing left to do writes",
			tracker.WalkBatch, dependents, mirrors.attrs)
	}

	// AND THE NEXT SWEEP DRAINS IT, and says that too.
	logged.reset()
	if _, err := worker.Tick(t.Context()); err != nil {
		t.Fatalf("the second sweep: %v", err)
	}
	r.drain()
	rest := logged.only(t, "tracker_unblocked_told")
	if got := rest.attrs["dependents"]; got != int64(1) {
		t.Errorf("the second sweep told %v dependents, want the one left "+
			"behind — a position that moved over a cut window is how the "+
			"rest stop being reachable", got)
	}
	if rest.attrs["truncated"] != false {
		t.Errorf("the sweep that drained the backlog still reports itself "+
			"cut: %+v — a flag that never clears says nothing", rest.attrs)
	}
	// AND THE TWIN CLEARS TOO. What is left of the one-sided edges by now
	// fits inside one batch, which is the boundary the probe row buys: a
	// window holding exactly the limit holds everything it has.
	if mirrors := logged.only(t, "tracker_one_sided_repaired"); mirrors.attrs["truncated"] != false {
		t.Errorf("the mirror repair reports itself cut with nothing left "+
			"past its bound: %+v", mirrors.attrs)
	}
}

// THE REPAIR'S THROUGHPUT IS THE NUMBER THE OPERATOR GUIDE GIVES.
//
// [tracker.WalkBatch] means a different thing at each of its call sites, as
// its own doc says — but at ONE of them it is a promise a document makes to an
// operator. The missed-unblocked repair publishes one record per dependent
// told and takes one batch per sweep, so `docs/guides/work-tracker.md` gives
// its throughput as at most 64 late notices a sweep, a sweep every
// [maintenance.Interval], and a backlog of a thousand draining in about four
// hours.
//
// NOTHING ELSE HOLDS THOSE TOGETHER. The guide is prose, and the constant is
// shared with the merge and re-spread walks — so somebody widening it for a
// walk moves a promise in a document they have no reason to be reading. This
// case is what makes that fail here rather than quietly there.
func TestTheRepairDrainIsWhatTheOperatorGuidePromises(t *testing.T) {
	t.Parallel()
	if tracker.WalkBatch != 64 {
		t.Errorf("WalkBatch is %d, and docs/guides/work-tracker.md tells "+
			"operators the tracker duty sends `at most 64 notices a sweep` — "+
			"move the guide's paragraph and this case with it, or put the "+
			"number back", tracker.WalkBatch)
	}
	// THE SAME 64 AS THE BULK FAN-OUT, which is the identity both
	// constants' docs claim and nothing else enforces.
	if tracker.WalkBatch != tracker.MaxBulkTasks {
		t.Errorf("WalkBatch is %d and MaxBulkTasks is %d — both docs say they "+
			"are the same quantity, how much work one bounded step hands "+
			"every node's applier at once", tracker.WalkBatch, tracker.MaxBulkTasks)
	}
	// THE CONSEQUENCE, spelled as arithmetic rather than as prose: this is
	// the sentence the guide and the constant's own doc both carry.
	const backlog = 1000
	sweeps := (backlog + tracker.WalkBatch - 1) / tracker.WalkBatch
	drain := time.Duration(sweeps) * maintenance.Interval
	if drain < 3*time.Hour || drain > 5*time.Hour {
		t.Errorf("a backlog of %d late notices drains in %s — %d sweeps of %d "+
			"notices, one sweep every %s — and both the guide and "+
			"tracker.WalkBatch's own doc say about four hours",
			backlog, drain, sweeps, tracker.WalkBatch, maintenance.Interval)
	}
}

func trackerWorker(t *testing.T, r *roundTrip) *maintenance.Worker {
	t.Helper()
	return trackerWorkerLogging(t, r, slog.New(slog.DiscardHandler))
}

func trackerWorkerLogging(t *testing.T, r *roundTrip, log *slog.Logger) *maintenance.Worker {
	t.Helper()
	return trackerWorkerWriting(t, r, log, r.writer)
}

// trackerWorkerWriting is the same duty over a writer a case chooses, which is
// how a commit is made to fail without a second implementation of the job.
func trackerWorkerWriting(t *testing.T, r *roundTrip, log *slog.Logger,
	writer *tracker.Writer) *maintenance.Worker {

	t.Helper()
	w, err := maintenance.New(maintenance.Options{
		Jobs: tracker.Jobs(tracker.DutyDeps{
			DB: r.db, Writer: writer, NodeID: "node-a", Logger: log,
			Reader: linearReader(t, r),
		}),
		Now: func() time.Time { return wednesday },
	})
	if err != nil {
		t.Fatalf("build the tracker's maintenance worker: %v", err)
	}
	return w
}

// linearReader is this harness's read path at the level the merge repair reads
// at: a barrier appended to the real log, and a wait that runs this node's
// applier until it has applied the barrier — which is what a node's own
// applier does while a read waits. Nothing else in the harness is driven: a
// write the case left on the log unapplied stays unapplied until something
// reads at this level.
func linearReader(t *testing.T, r *roundTrip) *tracker.Reader {
	t.Helper()
	index, err := statelog.NewReadIndex(tracker.Domain{}, r.log,
		tracker.EncodeBarrier, func() uint32 { return 0 }, nil)
	if err != nil {
		t.Fatalf("build the read index: %v", err)
	}
	var lag, first, floor uint64 = 0, 1, 0
	log, err := statelog.NewReader(statelog.ReaderDeps{
		Domain: tracker.Domain{}, DB: r.db.Replicated(), Index: index,
		Waiter: drainingWaiter{committed: r.waiter.Committed, drain: r.drain},
		Health: func() statelog.Health {
			at := r.waiter.Committed()
			return statelog.Health{
				Position: at, AppliedThrough: at.Seq, CaughtUp: true,
				Floor: statelog.Floor{State: statelog.FloorOK, ReadAt: time.Now()},
				Lag:   &lag, FirstSeq: &first, TrimFloor: &floor,
			}
		},
	})
	if err != nil {
		t.Fatalf("build the framework reader: %v", err)
	}
	reader, err := tracker.NewReader(r.db, log)
	if err != nil {
		t.Fatalf("build the tracker reader: %v", err)
	}
	return reader
}

// drainingWaiter is this node's applier as a linearizable read waits on it:
// it applies what the log holds until the position is reached. Its two halves
// are the harness's own — the position [roundTrip.apply] reaches, and
// [roundTrip.drain] — so the applier here is the one every case drives.
type drainingWaiter struct {
	committed func() statelog.Position
	drain     func()
}

func (w drainingWaiter) Committed() statelog.Position { return w.committed() }

func (w drainingWaiter) WaitCommitted(ctx context.Context, p statelog.Position) error {
	for w.Committed().Packed() < p.Packed() {
		if err := ctx.Err(); err != nil {
			return err
		}
		w.drain()
	}
	return nil
}

func (w drainingWaiter) WaitApplied(ctx context.Context, _ statelog.ScopeSet,
	p statelog.Position) error {
	return w.WaitCommitted(ctx, p)
}

// firstVersionMark is a merge marker as a record at the first version raises
// it: the marker alone, with no target — [tracker.MergeRecordVersion] is what
// added one — and here with no `duplicates` edge either.
func firstVersionMark(id string) tracker.MutationRecord {
	merging := true
	rec := taskRecord(id, tracker.OpPatch, tracker.TaskPatch{Merging: &merging}, nil)
	rec.OpID = "op-mark-" + id
	rec.Kind = tracker.ChangeFields
	return rec
}

// capturedLog keeps what the duty logged, because for these repairs the log
// line IS the surface: nothing else in the tree reads a scan's truncation.
type capturedLog struct {
	mu    sync.Mutex
	lines []loggedLine
}

type loggedLine struct {
	msg   string
	attrs map[string]any
}

func (c *capturedLog) Enabled(context.Context, slog.Level) bool { return true }

func (c *capturedLog) Handle(_ context.Context, rec slog.Record) error {
	line := loggedLine{msg: rec.Message, attrs: map[string]any{}}
	// COPIED OUT HERE, because a [slog.Record]'s attrs are only valid for
	// the length of this call.
	rec.Attrs(func(a slog.Attr) bool {
		line.attrs[a.Key] = a.Value.Any()
		return true
	})
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, line)
	return nil
}

func (c *capturedLog) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *capturedLog) WithGroup(string) slog.Handler      { return c }

func (c *capturedLog) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = nil
}

// only is the one line carrying this message, and it fails when there is not
// exactly one — a case asserting "the line says X" has to know it read the
// line it meant.
func (c *capturedLog) only(t *testing.T, msg string) loggedLine {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	var found []loggedLine
	for _, line := range c.lines {
		if line.msg == msg {
			found = append(found, line)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d %q lines were logged, want exactly one: %+v",
			len(found), msg, c.lines)
	}
	return found[0]
}

func flagged(t *testing.T, r *roundTrip, column string) bool {
	t.Helper()
	var set int
	if err := r.db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(),
			`SELECT `+column+` FROM tracker_projects WHERE key = 'ENG'`).Scan(&set)
	}); err != nil {
		t.Fatalf("read %s: %v", column, err)
	}
	return set == 1
}

// ONE SWEEP REPAIRS EVERY EDGE BEHIND ONE BLOCKER.
//
// # Why a shared blocker is the case that has to work
//
// The mirror is the half of a dependency that loses the race, so the edges
// that go one-sided are the ones naming a blocker several tasks wait on. Each
// mirror lands on that blocker's own subject, and two records on one subject
// cannot both be decided in one tick: the write path waits for the subject's
// anchor and refuses as `behind` when the wait expires. Published one record
// per edge, a sweep therefore repairs the FIRST edge behind a blocker and
// refuses the rest — every sweep, for ever, re-publishing and re-failing the
// same records, at an effective throughput of one mirror per blocker per
// fifteen minutes.
//
// So the tick is grouped by subject ([tracker.PlanOneSided]) and this is the
// case that says so end to end: a real duty, a real broker, one blocker.
func TestAMirrorSweepRepairsEveryEdgeBehindOneBlocker(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	const dependents = 5
	blocker := newTask("t-0")
	blocker.Assignee = "bob"
	if _, err := r.writer.CreateTask(t.Context(), "op-t-0", blocker, nil); err != nil {
		t.Fatalf("CreateTask t-0: %v", err)
	}
	r.drain()
	want := make([]string, 0, dependents)
	for i := range dependents {
		id := fmt.Sprintf("d-%03d", i)
		want = append(want, id)
		dependent := newTask(id)
		dependent.Assignee = "alice"
		// THE AUTHORED HALF ONLY, which is the residue a gesture that
		// stopped before its mirror leaves.
		dependent.Relations = []tracker.Relation{{
			Kind: tracker.RelationWaitingOn, Other: "t-0",
		}}
		if _, err := r.writer.CreateTask(t.Context(), "op-"+id, dependent, nil); err != nil {
			t.Fatalf("CreateTask %s: %v", id, err)
		}
		// DRAINED BETWEEN THE CREATES: they mint keys from one project
		// counter, and a create cannot decide against a number this node
		// has not applied.
		r.drain()
	}

	logged := &capturedLog{}
	if _, err := trackerWorkerLogging(t, r, slog.New(logged)).Tick(t.Context()); err != nil {
		t.Fatalf("the sweep: %v", err)
	}
	r.drain()

	held := r.task(t, "t-0").Task.Dependents
	for _, id := range want {
		if !slices.Contains(held, id) {
			t.Errorf("%s is still one-sided after a sweep: the blocker lists "+
				"%v — its assignee was told about %d of %d tasks now waiting "+
				"on it, and the repair's own record is the only wake that "+
				"side ever gets", id, held, len(held), dependents)
		}
	}
	line := logged.only(t, "tracker_one_sided_repaired")
	if line.attrs["edges"] != int64(dependents) || line.attrs["deferred"] != int64(0) {
		t.Errorf("the sweep logged %+v over %d broken edges behind one "+
			"blocker, want all of them repaired and none deferred",
			line.attrs, dependents)
	}
	// AND NOTHING WAS REFUSED, which is what the count alone cannot say:
	// a tick that mirrored one edge and logged four failures reports a
	// number an operator reads as progress.
	for _, entry := range logged.lines {
		if entry.msg == "tracker_one_sided_repair_failed" {
			t.Errorf("a repair was refused: %+v", entry.attrs)
		}
	}
}

// A MIRROR SWEEP SAYS HOW MUCH OF WHAT IT READ IT LEFT BEHIND.
//
// # Two different shortfalls, and one of them alone marks nothing
//
// `truncated` is the SCAN's cut: the window held more broken edges than the
// tick read at all. It says nothing about the edges the tick DID read — and a
// tick that read sixty-four and committed one prints exactly the `truncated` a
// tick that committed all sixty-four prints. That is the same unmarked cut the
// flag was added to close, one layer in, and it is the reachable one: a
// commit's failure is WARNed and stepped over, deliberately, so that one
// wedged counterparty does not hold up every other repair in the company.
//
// So the line carries `deferred` too: of the edges this scan carried, how many
// this tick did not repair. TWO-SIDED, over a tick that commits nothing and a
// tick that commits everything, because a number that is always zero is
// evidence of nothing.
func TestAMirrorSweepSaysHowMuchOfWhatItReadItLeft(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	blocker := newTask("blk")
	blocker.Assignee = "bob"
	if _, err := r.writer.CreateTask(t.Context(), "op-blk", blocker, nil); err != nil {
		t.Fatalf("CreateTask blk: %v", err)
	}
	r.drain()
	const waiting = 3
	for i := range waiting {
		id := fmt.Sprintf("d-%03d", i)
		dependent := newTask(id)
		dependent.Assignee = "alice"
		dependent.Relations = []tracker.Relation{{
			Kind: tracker.RelationWaitingOn, Other: "blk",
		}}
		if _, err := r.writer.CreateTask(t.Context(), "op-"+id, dependent, nil); err != nil {
			t.Fatalf("CreateTask %s: %v", id, err)
		}
		r.drain()
	}

	// A WRITER THAT REFUSES EVERY PUBLISH, which is the cheapest honest
	// stand-in for the counterparty this repair actually gets wedged on:
	// what the duty sees is a commit that came back with an error, and
	// what it does with it is a WARN and the next commit.
	refusing := r.writer.As("", tracker.AuthorAgent, tracker.Provenance{})
	logged := &capturedLog{}
	if _, err := trackerWorkerWriting(t, r, slog.New(logged), refusing).
		Tick(t.Context()); err != nil {
		t.Fatalf("the refused sweep: %v", err)
	}
	r.drain()
	cut := logged.only(t, "tracker_one_sided_repaired")
	if cut.attrs["edges"] != int64(0) || cut.attrs["deferred"] != int64(waiting) {
		t.Fatalf("a sweep that read %d edges and committed none logged %+v — "+
			"an operator reading `edges` and `truncated` alone concludes the "+
			"dependency graph is whole", waiting, cut.attrs)
	}
	if cut.attrs["truncated"] != false {
		t.Errorf("the scan reports itself cut over %d edges, which is inside "+
			"its bound of %d: %+v — the two marks answer different questions "+
			"and neither substitutes for the other",
			waiting, tracker.WalkBatch, cut.attrs)
	}

	// AND THE SWEEP THAT LANDS THEM SAYS SO, from the same fixture: the
	// edges are still flagged on their own rows, so nothing was lost to
	// the refusal — only the delay was, which is what the mark is for.
	logged.reset()
	if _, err := trackerWorkerLogging(t, r, slog.New(logged)).
		Tick(t.Context()); err != nil {
		t.Fatalf("the second sweep: %v", err)
	}
	r.drain()
	rest := logged.only(t, "tracker_one_sided_repaired")
	if rest.attrs["edges"] != int64(waiting) || rest.attrs["deferred"] != int64(0) {
		t.Errorf("the sweep that repaired the backlog logged %+v, want all %d "+
			"and nothing left — a `deferred` that never clears says nothing",
			rest.attrs, waiting)
	}
}

// THE DUPLICATE-RANK SWEEP SAYS WHEN IT CUT ITS OWN READ.
//
// The repair re-mints at most [tracker.WalkBatch] keys per project a sweep,
// and one log line is the whole of what an operator ever sees of it — so
// `tasks=64` is what a project holding exactly that many duplicates and a
// project holding thousands both print. Nothing is lost either way
// (`clearProbe` refuses to clear the project's flag while a duplicate is
// left), but which of the two a company is in is the difference between a
// board that is fixed and a board that is hours from it.
//
// TWO-SIDED, over two fixtures rather than two ticks of one: a value that is
// always true is evidence of nothing, and the drained side has to be a read
// that genuinely holds everything rather than one that happens to.
func TestADuplicateRankSweepSaysWhenItCutItsRead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		tasks int
		want  bool
	}{
		// ONE MORE DUPLICATE THAN A SWEEP CARRIES: the first task at a
		// shared rank keeps its key, so the losers are one fewer than
		// the tasks.
		{name: "a project past the bound says so",
			tasks: tracker.WalkBatch + 2, want: true},
		{name: "a project inside it says so too", tasks: 2, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRoundTrip(t)
			placements := make([]tracker.Placement, 0, tc.tasks)
			for i := range tc.tasks {
				id := fmt.Sprintf("t-%03d", i)
				if _, err := r.writer.CreateTask(t.Context(), "op-"+id,
					newTask(id), nil); err != nil {
					t.Fatalf("CreateTask %s: %v", id, err)
				}
				r.drain()
				placements = append(placements,
					tracker.Placement{Task: id, Rank: "a0V"})
			}
			// EVERY ONE AT ONE KEY, which is what concurrent drags into
			// the same gap produce — in records of at most
			// [tracker.MaxBulkTasks], the most one move carries.
			for from := 0; from < len(placements); from += tracker.MaxBulkTasks {
				batch := placements[from:min(from+tracker.MaxBulkTasks, len(placements))]
				if _, err := r.writer.MoveTasks(t.Context(),
					fmt.Sprintf("op-collide-%d", from), "ENG", r.order("ENG"), batch); err != nil {
					t.Fatalf("MoveTasks: %v", err)
				}
				r.drain()
			}
			if !flagged(t, r, "rank_duplicate_pending") {
				t.Fatal("the applier's own probe did not notice the collision, " +
					"so nothing hands the repair to the duty")
			}

			logged := &capturedLog{}
			holdTheAppliersPin(t, r)
			if _, err := trackerWorkerLogging(t, r, slog.New(logged)).
				Tick(t.Context()); err != nil {
				t.Fatalf("the sweep: %v", err)
			}
			r.drain()
			line := logged.only(t, "tracker_rank_duplicates_cleared")
			if line.attrs["truncated"] != tc.want {
				t.Errorf("a project holding %d duplicates logged %+v, want "+
					"truncated=%v — the count alone reads the same either way",
					tc.tasks-1, line.attrs, tc.want)
			}
		})
	}
}

// THE ABANDONED-MERGE SWEEP SAYS WHEN IT CUT ITS OWN READ.
//
// The same unmarked cut as the duplicate ranks, on the other gated job: the
// sweep walks at most [tracker.WalkBatch] mid-merge tasks and the count it
// returns is identical whether that was all of them or the first batch of
// thousands. Nothing is lost — the `merging = 1` gate is what this job selects
// on, so what a tick leaves is read again next sweep — but a company with a
// backlog of half-merged items on its boards looks exactly like a healthy one.
func TestAnAbandonedMergeSweepSaysWhenItCutItsRead(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	// ONE MORE THAN A SWEEP WALKS, which is the smallest fixture that
	// makes the real batch cut.
	const stuck = tracker.WalkBatch + 1
	for i := range stuck {
		id := fmt.Sprintf("m-%03d", i)
		task := newTask(id)
		// A MARKER WITH NO TARGET, which is the residue of a holder that
		// died between its first append and its last — and the one the
		// duty finishes with a single write, so this fixture is about
		// the bound rather than about a merge walk.
		task.Merging = true
		if _, err := r.writer.CreateTask(t.Context(), "op-"+id, task, nil); err != nil {
			t.Fatalf("CreateTask %s: %v", id, err)
		}
		r.drain()
	}

	logged := &capturedLog{}
	worker := trackerWorkerLogging(t, r, slog.New(logged))
	if _, err := worker.Tick(t.Context()); err != nil {
		t.Fatalf("the first sweep: %v", err)
	}
	r.drain()
	cut := logged.only(t, "tracker_abandoned_merges_finished")
	if cut.attrs["merges"] != int64(tracker.WalkBatch) {
		t.Fatalf("the sweep finished %v merges, want the batch of %d — the "+
			"fixture is not exercising the bound", cut.attrs["merges"],
			tracker.WalkBatch)
	}
	if cut.attrs["truncated"] != true {
		t.Errorf("a sweep that walked %d of %d abandoned merges logged %+v — "+
			"the same line a sweep with nothing left to do writes",
			tracker.WalkBatch, stuck, cut.attrs)
	}

	// AND THE NEXT SWEEP DRAINS IT, and says that too.
	logged.reset()
	if _, err := worker.Tick(t.Context()); err != nil {
		t.Fatalf("the second sweep: %v", err)
	}
	r.drain()
	rest := logged.only(t, "tracker_abandoned_merges_finished")
	if rest.attrs["merges"] != int64(1) || rest.attrs["truncated"] != false {
		t.Errorf("the sweep that drained the backlog logged %+v, want the one "+
			"it had left and no cut — a flag that never clears says nothing",
			rest.attrs)
	}
}

// A DUPLICATE REPAIR RACED BY A PLACEMENT LEAVES THAT PROJECT TO THE NEXT SWEEP.
//
// The repair mints its keys from one read of the order and publishes them
// against that read's version. A card placed in between — here, on the log
// but not yet applied when the repair reads, so the repair's own publish
// loses the race and re-decides against the order as it now is — makes those
// keys ones minted from an order that no longer exists. The repair is refused,
// and the sweep has to treat that as a race rather than a failure: it logs it,
// leaves the project flagged so the next sweep mints from the new order, and
// goes on to the other flagged projects.
func TestADuplicateRepairRacedByAPlacementLeavesItToTheNextSweep(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if _, err := r.writer.WriteDocument(t.Context(), "op-ops",
		tracker.ProjectSubject("OPS"), "", tracker.Project{
			V: 1, Key: "OPS", Name: "Operations",
			CreatedAt: wednesday, UpdatedAt: wednesday,
		}, tracker.ChangeProjectCreated, nil); err != nil {
		t.Fatalf("seed OPS: %v", err)
	}
	r.drain()
	for _, spec := range []struct{ id, project string }{
		{"t-1", "ENG"}, {"t-2", "ENG"}, {"t-3", "ENG"},
		{"o-1", "OPS"}, {"o-2", "OPS"},
	} {
		task := newTask(spec.id)
		task.Project, task.Key = spec.project, ""
		if _, err := r.writer.CreateTask(t.Context(), "op-"+spec.id, task, nil); err != nil {
			t.Fatalf("CreateTask %s: %v", spec.id, err)
		}
		r.drain()
	}
	// TWO TASKS AT ONE KEY IN EACH PROJECT, which is what two concurrent
	// drags into the same gap leave.
	for project, pair := range map[string][2]string{
		"ENG": {"t-1", "t-2"}, "OPS": {"o-1", "o-2"},
	} {
		if _, err := r.writer.MoveTasks(t.Context(), "op-collide-"+project,
			project, r.order(project), []tracker.Placement{
				{Task: pair[0], Rank: "a0V"}, {Task: pair[1], Rank: "a0V"},
			}); err != nil {
			t.Fatalf("collide in %s: %v", project, err)
		}
		r.drain()
	}
	duplicated := func(project string) bool {
		return len(r.strings(`SELECT id FROM tracker_tasks
			WHERE project_key = ? AND rank = 'a0V'`, project)) > 1
	}
	flaggedIn := func(project string) bool {
		return r.strings(`SELECT CAST(rank_duplicate_pending AS TEXT)
			FROM tracker_projects WHERE key = ?`, project)[0] == "1"
	}
	if !duplicated("ENG") || !duplicated("OPS") || !flaggedIn("ENG") || !flaggedIn("OPS") {
		t.Fatal("the fixture does not leave both projects duplicated and flagged")
	}

	// THE PLACEMENT THE REPAIR WILL RACE: t-3 to the head of ENG, published
	// and not yet applied here — so the repair reads the order without it.
	if _, err := r.writer.MoveTask(t.Context(), "op-drop", "ENG", "t-3", "",
		"t-1"); err != nil {
		t.Fatalf("the drop: %v", err)
	}
	r.applyWhileWriting()

	log := &capturedLog{}
	holdTheAppliersPin(t, r)
	worker := trackerWorkerLogging(t, r, slog.New(log))
	if _, err := worker.Tick(t.Context()); err != nil {
		t.Fatalf("a repair that lost a race failed the sweep: %v", err)
	}
	r.drain()
	raced := log.only(t, "tracker_rank_duplicates_raced")
	if raced.attrs["project"] != "ENG" {
		t.Fatalf("the race is reported for %v, want ENG", raced.attrs["project"])
	}
	if cleared := log.only(t, "tracker_rank_duplicates_cleared"); cleared.attrs["project"] != "OPS" {
		t.Fatalf("the sweep reports clearing %v, want OPS alone — nothing of "+
			"the raced repair landed", cleared.attrs["project"])
	}
	if got := boardOrder(t, r); len(got) == 0 || got[0] != "t-3" {
		t.Fatalf("ENG reads %v, want the placed card at its head", got)
	}
	if duplicated("OPS") || flaggedIn("OPS") {
		t.Fatal("OPS was not repaired: the race on ENG stopped the sweep before " +
			"it reached the rest")
	}
	if !duplicated("ENG") || !flaggedIn("ENG") {
		t.Fatal("ENG lost its duplicates' flag or had keys minted from the order " +
			"before the placement — the raced repair must land nothing")
	}

	// AND THE NEXT SWEEP MINTS FROM THE ORDER AS IT NOW IS.
	if _, err := worker.Tick(t.Context()); err != nil {
		t.Fatalf("the next sweep: %v", err)
	}
	r.drain()
	if duplicated("ENG") || flaggedIn("ENG") {
		t.Fatal("the sweep after the race did not repair ENG")
	}
	if got := boardOrder(t, r); got[0] != "t-3" {
		t.Fatalf("the repair moved the placed card: ENG reads %v", got)
	}
}

// AN ABANDONED MERGE INTO A TASK SINCE PURGED IS CLEARED, NOT COMPLETED.
//
// The duty finishes a merge whose holder died by doing what the merge would
// have: re-parent the subtasks onto the target if the merge said to, and cancel
// the duplicate. When the target has been purged in between, that gave each
// subtask a parent no row holds and closed the duplicate as merged into
// nothing. The merge did not happen and now cannot, so the repair is the one
// the duty already makes for a marker with no target at all — the marker goes,
// and the task stays open with its subtasks.
func TestAnAbandonedMergeIntoAPurgedTaskIsClearedNotCompleted(t *testing.T) {
	t.Parallel()
	for _, reparent := range []bool{true, false} {
		t.Run(fmt.Sprintf("reparent=%v", reparent), func(t *testing.T) {
			t.Parallel()
			r := newRoundTrip(t)
			for _, id := range []string{"keep", "dup"} {
				if _, err := r.writer.CreateTask(t.Context(), "op-"+id,
					newTask(id), nil); err != nil {
					t.Fatalf("CreateTask %s: %v", id, err)
				}
				r.drain()
			}
			parent := "dup"
			kid := newTask("kid")
			kid.Parent, kid.Depth = &parent, 1
			if _, err := r.writer.CreateTask(t.Context(), "op-kid", kid, nil); err != nil {
				t.Fatalf("CreateTask kid: %v", err)
			}
			r.drain()
			merging := true
			if _, err := r.writer.UpdateTask(t.Context(), "op-mark", "dup", "ENG",
				tracker.NoIfMatch, tracker.TaskPatch{
					Relate: &tracker.RelationIntent{Add: []tracker.Relation{{
						Kind: tracker.RelationDuplicates, Other: "keep",
					}}},
					Merging: &merging, MergeReparent: &reparent, MergeInto: ptr("keep"),
				}, tracker.ChangeRelations, nil); err != nil {
				t.Fatalf("UpdateTask mark: %v", err)
			}
			r.drain()
			if _, err := r.writer.PurgeTask(t.Context(), "op-purge", "keep", "ENG",
				"filed twice"); err != nil {
				t.Fatalf("purge the target: %v", err)
			}
			r.drain()

			log := &capturedLog{}
			holdTheAppliersPin(t, r)
			r.applyWhileWriting()
			if _, err := trackerWorkerLogging(t, r, slog.New(log)).Tick(t.Context()); err != nil {
				t.Fatalf("Tick: %v", err)
			}
			r.drain()
			if line := log.only(t, "tracker_merge_target_purged"); line.attrs["target"] != "keep" {
				t.Fatalf("the warning names target %v, want keep", line.attrs["target"])
			}
			dup := r.task(t, "dup")
			if dup.Task.Merging || dup.Task.Status == tracker.StatusCancelled {
				t.Errorf("the duplicate reads merging=%v and status %q, want the "+
					"marker cleared and the task left open", dup.Task.Merging,
					dup.Task.Status)
			}
			if got := parentOf(r.task(t, "kid")); got != "dup" {
				t.Errorf("the subtask's parent is %q, want it left under dup — "+
					"the target it would move onto no longer exists", got)
			}
		})
	}
}

// A PURGE THAT LANDS WHILE THE DUTY FINISHES A MERGE IS SEEN BY ITS NEXT STEP.
//
// The sweep reads which merges were abandoned and what each was folding into,
// and then makes the rest of each one's appends. Whether the target is still
// there is read by those appends, each in its own snapshot, and not by the
// sweep's read, which is older than all of them — so a purge landing after the
// first subtask moved stops the second: it stays under the duplicate, nothing
// hangs from the purged target, and the duplicate is left open with its marker
// cleared.
func TestAPurgeDuringTheDutysMergeIsSeenByItsNextStep(t *testing.T) {
	t.Parallel()
	r, hooked := newHookedRoundTrip(t)
	for _, id := range []string{"keep", "dup"} {
		if _, err := r.writer.CreateTask(t.Context(), "op-"+id, newTask(id), nil); err != nil {
			t.Fatalf("CreateTask %s: %v", id, err)
		}
		r.drain()
	}
	parent := "dup"
	for _, id := range []string{"kid-1", "kid-2"} {
		kid := newTask(id)
		kid.Parent, kid.Depth = &parent, 1
		if _, err := r.writer.CreateTask(t.Context(), "op-"+id, kid, nil); err != nil {
			t.Fatalf("CreateTask %s: %v", id, err)
		}
		r.drain()
	}
	merging, reparent := true, true
	if _, err := r.writer.UpdateTask(t.Context(), "op-mark", "dup", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{
			Relate: &tracker.RelationIntent{Add: []tracker.Relation{{
				Kind: tracker.RelationDuplicates, Other: "keep",
			}}},
			Merging: &merging, MergeReparent: &reparent, MergeInto: ptr("keep"),
		}, tracker.ChangeRelations, nil); err != nil {
		t.Fatalf("UpdateTask mark: %v", err)
	}
	r.drain()
	// THE FIRST SUBTASK'S MOVE IS WHERE IT LANDS: the sweep has read the
	// merge and moved one child, and the second is still to come.
	hooked.arm(func(subject, _ string) bool {
		return strings.HasSuffix(subject, ".task.kid-1")
	}, func() { purgeNow(t, r, "keep") })

	log := &capturedLog{}
	holdTheAppliersPin(t, r)
	r.applyWhileWriting()
	if _, err := trackerWorkerLogging(t, r, slog.New(log)).Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	r.drain()
	if !hooked.didFire() {
		t.Fatal("the purge never landed during the sweep, so this case is not " +
			"the shape it names")
	}
	if line := log.only(t, "tracker_merge_target_purged"); line.attrs["target"] != "keep" {
		t.Fatalf("the warning names target %v, want keep", line.attrs["target"])
	}
	dup := r.task(t, "dup")
	if dup.Task.Merging || dup.Task.Status == tracker.StatusCancelled {
		t.Errorf("the duplicate reads merging=%v and status %q, want the marker "+
			"cleared and the task left open", dup.Task.Merging, dup.Task.Status)
	}
	if got := parentOf(r.task(t, "kid-2")); got != "dup" {
		t.Errorf("the second subtask's parent is %q, want it left under dup — "+
			"its move was decided after the target was gone", got)
	}
	if named := r.strings(`SELECT id FROM tracker_tasks WHERE parent_id = 'keep'`); len(named) != 0 {
		t.Errorf("%v hang from the purged target", named)
	}
}

// THE SWEEP PASSES OVER A MERGE THAT IS STILL WALKING.
//
// The marker stands for the whole of every merge, a live one included, so the
// row alone cannot tell a walk whose holder died from one still running — the
// merge's claim can. A sweep landing in the middle of a merge on this node
// leaves it, and the merge finishes as if alone. Run beside it instead, the
// sweep re-read the subtasks the walk had not moved yet and published a second
// close of the duplicate.
func TestTheSweepPassesOverAMergeThatIsStillWalking(t *testing.T) {
	t.Parallel()
	r, hooked := newHookedRoundTrip(t)
	r.applyWhileWriting()
	filedTask(t, r, "keep")
	filedTask(t, r, "dup")
	parent := "dup"
	kid := newTask("kid")
	kid.Parent, kid.Depth = &parent, 1
	if _, err := r.writer.CreateTask(t.Context(), "op-kid", kid, nil); err != nil {
		t.Fatalf("CreateTask kid: %v", err)
	}
	r.drain()
	var swept map[string]int64
	var tickErr error
	hooked.arm(func(_, opID string) bool { return opID == "op-merge.mark" },
		func() {
			// THE MARK APPLIED FIRST, so the sweep's own gate and scan see
			// the duplicate mid-merge exactly as a peer's would.
			r.drain()
			swept, tickErr = trackerWorker(t, r).Tick(t.Context())
		})

	if _, err := r.writer.MergeDuplicates(t.Context(), "op-merge", "dup", "keep",
		true, nil); err != nil {
		t.Fatalf("MergeDuplicates: %v", err)
	}
	if !hooked.didFire() {
		t.Fatal("the sweep never ran inside the merge, so this case is not the " +
			"shape it names")
	}
	if tickErr != nil {
		t.Fatalf("Tick: %v", tickErr)
	}
	if n := swept["tracker_abandoned_merges"]; n != 0 {
		t.Errorf("the sweep finished %d merge(s) while the merge was still "+
			"walking", n)
	}
	r.drain()
	if closes := r.strings(`SELECT id FROM tracker_history
		WHERE subject_id = 'dup' AND kind = 'status'`); len(closes) != 1 {
		t.Errorf("the duplicate was closed %d times, want once", len(closes))
	}
	dup := r.task(t, "dup")
	if dup.Task.Status != tracker.StatusCancelled || dup.Task.Merging {
		t.Errorf("the duplicate reads status %q and merging=%v after its merge "+
			"finished", dup.Task.Status, dup.Task.Merging)
	}
	if got := parentOf(r.task(t, "kid")); got != "keep" {
		t.Errorf("the subtask's parent is %q, want keep", got)
	}
}

// AND THE SWEEP GIVES A MERGE'S CLAIM BACK.
//
// It takes the claim to finish a merge, and a claim it kept would read, to
// every later sweep and to every merge of that item on this node, as a walk
// still running — so a merge abandoned a second time would be passed over for
// ever.
func TestTheSweepGivesAMergesClaimBack(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "keep")
	filedTask(t, r, "dup")
	for round := 1; round <= 2; round++ {
		merging, reparent := true, false
		if _, err := r.writer.UpdateTask(t.Context(), fmt.Sprintf("op-mark-%d", round),
			"dup", "ENG", tracker.NoIfMatch, tracker.TaskPatch{
				Relate: &tracker.RelationIntent{Add: []tracker.Relation{{
					Kind: tracker.RelationDuplicates, Other: "keep",
				}}},
				Merging: &merging, MergeReparent: &reparent, MergeInto: ptr("keep"),
			}, tracker.ChangeRelations, nil); err != nil {
			t.Fatalf("round %d: UpdateTask mark: %v", round, err)
		}
		r.drain()
		swept, err := trackerWorker(t, r).Tick(t.Context())
		if err != nil {
			t.Fatalf("round %d: Tick: %v", round, err)
		}
		if n := swept["tracker_abandoned_merges"]; n != 1 {
			t.Fatalf("round %d: the sweep finished %d merge(s), want the one "+
				"abandoned", round, n)
		}
		r.drain()
	}
}

// A MERGE ANOTHER NODE IS WALKING IS PASSED OVER TOO, AND LEFT CLAIMABLE HERE.
//
// Across nodes it is the lease that says the walk is live, and the sweep asks
// for this node's half of the claim before it asks for the lease. So a lease a
// peer holds has to hand that half back — kept, it would read on this node as
// a walk still running long after the peer's merge was done, and a merge of
// the item abandoned later would be passed over for ever.
func TestTheSweepPassesOverAMergeAnotherNodeIsWalking(t *testing.T) {
	t.Parallel()
	r, hooked := newHookedRoundTrip(t)
	r.applyWhileWriting()
	filedTask(t, r, "keep")
	filedTask(t, r, "dup")
	var swept map[string]int64
	var tickErr error
	hooked.arm(func(_, opID string) bool { return opID == "op-merge.mark" },
		func() {
			r.drain()
			swept, tickErr = trackerWorker(t, r).Tick(t.Context())
		})
	if _, err := r.peer("node-b").MergeDuplicates(t.Context(), "op-merge", "dup",
		"keep", true, nil); err != nil {
		t.Fatalf("node-b's MergeDuplicates: %v", err)
	}
	if !hooked.didFire() {
		t.Fatal("the sweep never ran inside node-b's merge, so this case is not " +
			"the shape it names")
	}
	if tickErr != nil {
		t.Fatalf("Tick: %v", tickErr)
	}
	if n := swept["tracker_abandoned_merges"]; n != 0 {
		t.Fatalf("the sweep finished %d merge(s) while node-b was walking one", n)
	}
	r.drain()

	// AND ABANDONED LATER, the same item's merge is this node's to finish.
	merging, reparent := true, false
	if _, err := r.writer.UpdateTask(t.Context(), "op-mark-again", "dup", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{
			Relate: &tracker.RelationIntent{Add: []tracker.Relation{{
				Kind: tracker.RelationDuplicates, Other: "keep",
			}}},
			Merging: &merging, MergeReparent: &reparent, MergeInto: ptr("keep"),
		}, tracker.ChangeRelations, nil); err != nil {
		t.Fatalf("UpdateTask mark: %v", err)
	}
	r.drain()
	swept, err := trackerWorker(t, r).Tick(t.Context())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if n := swept["tracker_abandoned_merges"]; n != 1 {
		t.Errorf("the sweep finished %d merge(s), want the one abandoned — this "+
			"node's half of the claim was kept when node-b's lease refused it", n)
	}
}

// A MERGE CLOSED ON THE LOG BUT NOT YET HERE IS NOT CLOSED AGAIN.
//
// A walk that closes on another node gives its claim back before this node
// applies the close, so the sweep can take the claim of a merge its own rows
// still show mid-merge. The close it would make reads the merge again in its
// own decide; decided from rows older than the log it is refused by the broker
// and decided again from rows that have the close, and a merge that has ended
// writes nothing and is not counted.
func TestTheSweepDoesNotCloseAMergeThatAlreadyClosed(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "keep")
	filedTask(t, r, "dup")
	merging, reparent := true, false
	if _, err := r.writer.UpdateTask(t.Context(), "op-mark", "dup", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{
			Relate: &tracker.RelationIntent{Add: []tracker.Relation{{
				Kind: tracker.RelationDuplicates, Other: "keep",
			}}},
			Merging: &merging, MergeReparent: &reparent, MergeInto: ptr("keep"),
		}, tracker.ChangeRelations, nil); err != nil {
		t.Fatalf("UpdateTask mark: %v", err)
	}
	r.drain()
	// THE CLOSE ON THE LOG AND NOT IN THIS NODE'S ROWS: published, and not
	// applied until something here waits for it.
	cancelled, done := tracker.StatusCancelled, false
	if _, err := r.writer.UpdateTask(t.Context(), "op-close", "dup", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Status: &cancelled, Merging: &done},
		tracker.ChangeStatus, nil); err != nil {
		t.Fatalf("UpdateTask close: %v", err)
	}
	if !r.task(t, "dup").Task.Merging {
		t.Fatal("the close was applied before the sweep ran, so this case is " +
			"not the shape it names")
	}

	r.applyWhileWriting()
	swept, err := trackerWorker(t, r).Tick(t.Context())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if n := swept["tracker_abandoned_merges"]; n != 0 {
		t.Errorf("the sweep finished %d merge(s), and the one it read had "+
			"already closed", n)
	}
	r.drain()
	if closes := r.strings(`SELECT id FROM tracker_history
		WHERE subject_id = 'dup' AND kind = 'status'`); len(closes) != 1 {
		t.Errorf("the duplicate was closed %d times, want once", len(closes))
	}
}

// AND A MERGE GIVEN UP ON THE LOG BUT NOT YET HERE IS NOT GIVEN UP AGAIN.
//
// The give-up is the close's twin for a merge whose target was purged, and it
// is read the same way: it clears the marker only while the merge is still
// running, in its own decide, so a give-up another node already published is
// not published a second time from rows that predate it.
func TestTheSweepDoesNotGiveUpAMergeAlreadyGivenUp(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "keep")
	filedTask(t, r, "dup")
	merging, reparent := true, false
	if _, err := r.writer.UpdateTask(t.Context(), "op-mark", "dup", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{
			Relate: &tracker.RelationIntent{Add: []tracker.Relation{{
				Kind: tracker.RelationDuplicates, Other: "keep",
			}}},
			Merging: &merging, MergeReparent: &reparent, MergeInto: ptr("keep"),
		}, tracker.ChangeRelations, nil); err != nil {
		t.Fatalf("UpdateTask mark: %v", err)
	}
	r.drain()
	purgeNow(t, r, "keep")
	// THE GIVE-UP ON THE LOG AND NOT IN THIS NODE'S ROWS.
	done := false
	if _, err := r.writer.UpdateTask(t.Context(), "op-give-up", "dup", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Merging: &done},
		tracker.ChangeFields, nil); err != nil {
		t.Fatalf("UpdateTask give-up: %v", err)
	}
	if !r.task(t, "dup").Task.Merging {
		t.Fatal("the give-up was applied before the sweep ran, so this case is " +
			"not the shape it names")
	}

	log := &capturedLog{}
	r.applyWhileWriting()
	swept, err := trackerWorkerLogging(t, r, slog.New(log)).Tick(t.Context())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if n := swept["tracker_abandoned_merges"]; n != 0 {
		t.Errorf("the sweep finished %d merge(s), and the one it read had "+
			"already been given up", n)
	}
	r.drain()
	if clears := r.strings(`SELECT id FROM tracker_history
		WHERE subject_id = 'dup' AND kind = 'fields'`); len(clears) != 1 {
		t.Errorf("the marker was cleared %d times, want once", len(clears))
	}
}

// AND A MARKER WITH NO TARGET, CLEARED ON THE LOG BUT NOT YET HERE, IS NOT
// CLEARED AGAIN — the third of the repair's appends, read the same way.
func TestTheSweepDoesNotClearAMarkerAlreadyCleared(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "dup")
	done := false
	r.appendRecord(firstVersionMark("dup"))
	r.drain()
	if _, err := r.writer.UpdateTask(t.Context(), "op-clear", "dup", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Merging: &done},
		tracker.ChangeFields, nil); err != nil {
		t.Fatalf("UpdateTask clear: %v", err)
	}
	if !r.task(t, "dup").Task.Merging {
		t.Fatal("the clear was applied before the sweep ran, so this case is " +
			"not the shape it names")
	}

	r.applyWhileWriting()
	swept, err := trackerWorker(t, r).Tick(t.Context())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if n := swept["tracker_abandoned_merges"]; n != 0 {
		t.Errorf("the sweep cleared %d marker(s), and the one it read had "+
			"already been cleared", n)
	}
	r.drain()
	if writes := r.strings(`SELECT id FROM tracker_history
		WHERE subject_id = 'dup' AND kind = 'fields'`); len(writes) != 2 {
		t.Errorf("the task carries %d marker writes, want the mark and one clear",
			len(writes))
	}
}

// AN ABANDONED MERGE INTO AN ITEM SINCE PUT IN THE TRASH IS GIVEN UP, NOT
// RETRIED FOR EVER.
//
// A target in the trash refuses every step the duty would take — each subtask
// move as a parent in the trash, the close as a merge target in it — and none
// of those clears with time. Retried, the duty met the same refusal on every
// tick, and returned at it before any abandoned merge that sorted after it. So
// it is given up as a purged target's is: the marker cleared, the duplicate
// left open with its subtasks, and its own warning, because a removal has a
// remedy a purge does not.
//
// Mutation: give up on a purged target alone and the tick fails with the
// duplicate still marked.
func TestAnAbandonedMergeIntoAnItemInTheTrashIsGivenUp(t *testing.T) {
	t.Parallel()
	for _, reparent := range []bool{true, false} {
		t.Run(fmt.Sprintf("reparent=%v", reparent), func(t *testing.T) {
			t.Parallel()
			r := newRoundTrip(t)
			for _, id := range []string{"keep", "dup"} {
				if _, err := r.writer.CreateTask(t.Context(), "op-"+id,
					newTask(id), nil); err != nil {
					t.Fatalf("CreateTask %s: %v", id, err)
				}
				r.drain()
			}
			parent := "dup"
			kid := newTask("kid")
			kid.Parent, kid.Depth = &parent, 1
			if _, err := r.writer.CreateTask(t.Context(), "op-kid", kid, nil); err != nil {
				t.Fatalf("CreateTask kid: %v", err)
			}
			r.drain()
			merging := true
			if _, err := r.writer.UpdateTask(t.Context(), "op-mark", "dup", "ENG",
				tracker.NoIfMatch, tracker.TaskPatch{
					Relate: &tracker.RelationIntent{Add: []tracker.Relation{{
						Kind: tracker.RelationDuplicates, Other: "keep",
					}}},
					Merging: &merging, MergeReparent: &reparent, MergeInto: ptr("keep"),
				}, tracker.ChangeRelations, nil); err != nil {
				t.Fatalf("UpdateTask mark: %v", err)
			}
			r.drain()
			if _, err := r.writer.RemoveTask(t.Context(), "op-remove", "keep",
				"ENG", false, nil); err != nil {
				t.Fatalf("remove the target: %v", err)
			}
			r.drain()

			log := &capturedLog{}
			holdTheAppliersPin(t, r)
			r.applyWhileWriting()
			if _, err := trackerWorkerLogging(t, r, slog.New(log)).Tick(t.Context()); err != nil {
				t.Fatalf("Tick: %v", err)
			}
			r.drain()
			if line := log.only(t, "tracker_merge_target_removed"); line.attrs["target"] != "keep" {
				t.Fatalf("the warning names target %v, want keep", line.attrs["target"])
			}
			dup := r.task(t, "dup")
			if dup.Task.Merging || dup.Task.Status == tracker.StatusCancelled {
				t.Errorf("the duplicate reads merging=%v and status %q, want the "+
					"marker cleared and the task left open", dup.Task.Merging,
					dup.Task.Status)
			}
			if got := parentOf(r.task(t, "kid")); got != "dup" {
				t.Errorf("the subtask's parent is %q, want it left under dup — "+
					"the target it would move onto is in the trash", got)
			}
		})
	}
}

// ONE MERGE THE DUTY CANNOT FINISH DOES NOT HOLD UP THE REST.
//
// The sweep reads the abandoned merges in id order and finishes each under its
// own claim, on its own subjects — nothing one does depends on another. So a
// merge whose step fails is logged, counted and left marked for the next
// sweep, and the sweep goes on to the next one. Returning at the first failure
// instead held every abandoned merge sorting after a merge that fails on every
// tick for as long as it kept failing, with nothing but that one error to say
// so.
//
// Mutation: return at the first failure and m-b and m-c stay marked.
func TestOneMergeTheDutyCannotFinishDoesNotHoldUpTheRest(t *testing.T) {
	t.Parallel()
	broker := &refusingAppender{subject: tracker.TaskSubject("m-a").Wire()}
	r := newRoundTripAppending(t, func(a statelog.Appender) statelog.Appender {
		broker.Appender = a
		return broker
	})
	// MARKERS WITH NO TARGET, which the duty finishes with a single write
	// each — so the case is about the sweep's walk over them rather than
	// about any one merge's steps.
	for _, id := range []string{"m-a", "m-b", "m-c"} {
		task := newTask(id)
		task.Merging = true
		if _, err := r.writer.CreateTask(t.Context(), "op-"+id, task, nil); err != nil {
			t.Fatalf("CreateTask %s: %v", id, err)
		}
		r.drain()
	}
	broker.refusing(true)

	logged := &capturedLog{}
	swept, err := trackerWorkerLogging(t, r, slog.New(logged)).Tick(t.Context())
	r.drain()
	if err == nil || !strings.Contains(err.Error(), "m-a") {
		t.Errorf("a sweep with a merge it could not finish answered %v, want the "+
			"failure reported naming it", err)
	}
	if swept["tracker_abandoned_merges"] != 2 {
		t.Errorf("the sweep finished %d merges, want the 2 it could", swept["tracker_abandoned_merges"])
	}
	for id, want := range map[string]bool{"m-a": true, "m-b": false, "m-c": false} {
		if got := r.task(t, id).Task.Merging; got != want {
			t.Errorf("%s reads merging=%v after the sweep, want %v", id, got, want)
		}
	}
	if line := logged.only(t, "tracker_merge_finish_failed"); line.attrs["task"] != "m-a" {
		t.Errorf("the failure's line names %v, want m-a", line.attrs["task"])
	}
	summary := logged.only(t, "tracker_abandoned_merges_finished")
	if summary.attrs["merges"] != int64(2) || summary.attrs["failed"] != int64(1) {
		t.Errorf("the sweep's line says %+v, want 2 finished and 1 failed",
			summary.attrs)
	}
}

// A LAGGING NODE'S SWEEP DOES NOT ACT ON A MERGE THAT CLOSED BEFORE IT.
//
// A walk that closes on another node gives its claim back once the close is
// acknowledged, so the sweep here can take the claim while its own rows still
// show the merge running. Decided from those rows, the sweep moves a subtask
// filed under the duplicate after the walk read its batch — a subject nothing
// else wrote, so the broker accepts it — and the subtask lands on the target
// after the merge closed without it. The sweep reads the task linearizably
// after taking the claim instead, so its rows hold the close and it does
// nothing.
//
// Mutation: read the mid-merge task at `stale` in abandonedMerge and the late
// subtask is moved onto the target.
func TestALaggingSweepDoesNotActOnAMergeThatClosedBeforeIt(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "keep")
	filedTask(t, r, "dup")
	parent, keep := "dup", "keep"
	for _, id := range []string{"kid-1", "kid-late"} {
		kid := newTask(id)
		kid.Parent, kid.Depth = &parent, 1
		if id == "kid-late" {
			// THE WALK: the mark, and its move of the one subtask its
			// batch read. The late one is filed after that batch.
			merging, reparent := true, true
			if _, err := r.writer.UpdateTask(t.Context(), "op-mark", "dup", "ENG",
				tracker.NoIfMatch, tracker.TaskPatch{
					Relate: &tracker.RelationIntent{Add: []tracker.Relation{{
						Kind: tracker.RelationDuplicates, Other: "keep",
					}}},
					Merging: &merging, MergeReparent: &reparent, MergeInto: &keep,
				}, tracker.ChangeRelations, nil); err != nil {
				t.Fatalf("UpdateTask mark: %v", err)
			}
			r.drain()
			if _, err := r.writer.UpdateTask(t.Context(), "op-move", "kid-1", "ENG",
				tracker.NoIfMatch, tracker.TaskPatch{Parent: &keep},
				tracker.ChangeReparented, nil); err != nil {
				t.Fatalf("move kid-1: %v", err)
			}
			r.drain()
		}
		if _, err := r.writer.CreateTask(t.Context(), "op-"+id, kid, nil); err != nil {
			t.Fatalf("CreateTask %s: %v", id, err)
		}
		r.drain()
	}
	// THE CLOSE ON THE LOG AND NOT IN THIS NODE'S ROWS.
	cancelled, done := tracker.StatusCancelled, false
	if _, err := r.writer.UpdateTask(t.Context(), "op-close", "dup", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Status: &cancelled, Merging: &done},
		tracker.ChangeStatus, nil); err != nil {
		t.Fatalf("UpdateTask close: %v", err)
	}
	if !r.task(t, "dup").Task.Merging {
		t.Fatal("the close was applied before the sweep ran, so this case is " +
			"not the shape it names")
	}

	r.applyWhileWriting()
	swept, err := trackerWorker(t, r).Tick(t.Context())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if n := swept["tracker_abandoned_merges"]; n != 0 {
		t.Errorf("the sweep finished %d merge(s), and the one it read had "+
			"closed before it", n)
	}
	r.drain()
	if got := parentOf(r.task(t, "kid-late")); got != "dup" {
		t.Errorf("the subtask filed after the walk read its batch is under %q; "+
			"the merge had closed without it, so it stays under dup", got)
	}
}

// THE SWEEP FINISHES A MERGE INTO THE TASK ITS MARK NAMED.
//
// The mark states its target on the task, and that is what the sweep reads —
// not the `duplicates` relation, which can say something else by the time a
// merge is found abandoned: an edit to the relations while the merge runs
// replaces the edge the mark added. Read off the relations, the sweep folded
// the duplicate into whatever the last edit named.
//
// Mutation: read the target off the relations in abandonedMerge and the
// subtask ends under `other`.
func TestTheSweepFinishesAMergeIntoTheTaskItsMarkNamed(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for _, id := range []string{"keep", "other", "dup"} {
		filedTask(t, r, id)
	}
	parent, keep := "dup", "keep"
	kid := newTask("kid")
	kid.Parent, kid.Depth = &parent, 1
	if _, err := r.writer.CreateTask(t.Context(), "op-kid", kid, nil); err != nil {
		t.Fatalf("CreateTask kid: %v", err)
	}
	r.drain()
	merging, reparent := true, true
	if _, err := r.writer.UpdateTask(t.Context(), "op-mark", "dup", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{
			Relate: &tracker.RelationIntent{Add: []tracker.Relation{{
				Kind: tracker.RelationDuplicates, Other: "keep",
			}}},
			Merging: &merging, MergeReparent: &reparent, MergeInto: &keep,
		}, tracker.ChangeRelations, nil); err != nil {
		t.Fatalf("UpdateTask mark: %v", err)
	}
	r.drain()
	// WHILE IT IS MID-MERGE, somebody says it duplicates another item.
	if _, err := r.writer.UpdateTask(t.Context(), "op-relink", "dup", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{
			Relate: &tracker.RelationIntent{Add: []tracker.Relation{{
				Kind: tracker.RelationDuplicates, Other: "other",
			}}},
		}, tracker.ChangeRelations, nil); err != nil {
		t.Fatalf("UpdateTask relink: %v", err)
	}
	r.drain()

	r.applyWhileWriting()
	swept, err := trackerWorker(t, r).Tick(t.Context())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if swept["tracker_abandoned_merges"] != 1 {
		t.Fatalf("the sweep finished %d merges, want the one", swept["tracker_abandoned_merges"])
	}
	r.drain()
	if got := parentOf(r.task(t, "kid")); got != "keep" {
		t.Errorf("the subtask is under %q; the mark folded dup into keep", got)
	}
	if dup := r.task(t, "dup"); dup.Task.Status != tracker.StatusCancelled ||
		dup.Task.Merging || dup.Task.MergeInto != "" {
		t.Errorf("the duplicate reads status %q, merging=%v, target %q — want "+
			"it closed with the marker and its target cleared", dup.Task.Status,
			dup.Task.Merging, dup.Task.MergeInto)
	}
}

// A MARK AT THE FIRST VERSION NAMES ITS TARGET ONLY THROUGH ITS EDGE.
//
// Such a mark carries no target of its own, so the sweep finishes it into the
// task's `duplicates` edge when there is exactly one — the edge that mark
// added. With two, nothing says which the merge was folding into, and the
// sweep gives it up rather than guess: the marker cleared, the task left open
// with its subtasks, and the edges named on its warning.
//
// Mutation: take the last edge when there are several and the second case
// folds the duplicate into `other`.
func TestAMarkAtTheFirstVersionIsFinishedOnlyIntoItsOneEdge(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		edges []string
		// want is where the subtask ends, and closed whether the
		// duplicate is cancelled.
		want   string
		closed bool
	}{
		{"one edge is its target", []string{"keep"}, "keep", true},
		{"two edges name none", []string{"keep", "other"}, "dup", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRoundTrip(t)
			for _, id := range []string{"keep", "other", "dup"} {
				filedTask(t, r, id)
			}
			parent := "dup"
			kid := newTask("kid")
			kid.Parent, kid.Depth = &parent, 1
			if _, err := r.writer.CreateTask(t.Context(), "op-kid", kid, nil); err != nil {
				t.Fatalf("CreateTask kid: %v", err)
			}
			r.drain()
			relations := make([]tracker.Relation, 0, len(tc.edges))
			for _, other := range tc.edges {
				relations = append(relations, tracker.Relation{
					Kind: tracker.RelationDuplicates, Other: other,
				})
			}
			merging, reparent := true, true
			mark := taskRecord("dup", tracker.OpPatch, tracker.TaskPatch{
				Relations: &relations, Merging: &merging, MergeReparent: &reparent,
			}, nil)
			mark.OpID, mark.Kind = "op-mark", tracker.ChangeRelations
			r.appendRecord(mark)
			r.drain()

			log := &capturedLog{}
			r.applyWhileWriting()
			if _, err := trackerWorkerLogging(t, r, slog.New(log)).Tick(t.Context()); err != nil {
				t.Fatalf("Tick: %v", err)
			}
			r.drain()
			if got := parentOf(r.task(t, "kid")); got != tc.want {
				t.Errorf("the subtask is under %q, want %q", got, tc.want)
			}
			dup := r.task(t, "dup")
			if dup.Task.Merging || (dup.Task.Status == tracker.StatusCancelled) != tc.closed {
				t.Errorf("the duplicate reads merging=%v and status %q", dup.Task.Merging,
					dup.Task.Status)
			}
			if !tc.closed {
				line := log.only(t, "tracker_merge_marker_without_target")
				if got := fmt.Sprint(line.attrs["duplicates"]); got != "[keep other]" {
					t.Errorf("the warning names %s, want both edges", got)
				}
			}
		})
	}
}

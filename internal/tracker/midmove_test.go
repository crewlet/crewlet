package tracker_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// fileBehind files a subtask under one of the moving subtree's descendants the
// moment the root's move lands and before this node applies it — which is
// exactly what a node behind the log decides from: the root unmarked, and its
// parent still in ENG. then, when there is one, runs inside the same hook once
// the task is filed.
func fileBehind(t *testing.T, r *roundTrip, log *lossyLog, id, under string,
	then func()) {

	t.Helper()
	log.afterAppendTo("m-root", func() {
		late := newTask(id)
		late.Key = ""
		late.Parent, late.Depth = &under, 2
		late.Tags = []string{"late-tag"}
		if _, err := r.writer.CreateTask(t.Context(), "op-"+id, late, nil); err != nil {
			t.Errorf("file the task behind the move: %v", err)
		}
		if then != nil {
			then()
		}
	})
}

// lateTagFixture is [moveFixture] with a tag ENG declares and OPS does not, for
// a task filed behind the move to carry.
func lateTagFixture(t *testing.T) *roundTrip {
	t.Helper()
	r := moveFixture(t, "m-kid-a", "m-kid-b")
	if _, err := r.writer.WriteTags(t.Context(), "op-tag", "ENG",
		tracker.TagEdit{Add: []tracker.Tag{{Slug: "late-tag", Label: "Late"}}},
		tracker.TagAuthority{}); err != nil {
		t.Fatalf("declare ENG's tag: %v", err)
	}
	r.drain()
	return r
}

// A TASK FILED BEHIND A MOVE BY A NODE THE MARK HAD NOT REACHED IS CARRIED BY
// THE MOVE, tags and all.
//
// A move carries the subtree it read before its first append, and a create
// under a descendant is refused mid-move only on a node that has applied the
// root's move. This one is decided before the root's move has applied here —
// the one ordering the refusal cannot see — so it lands in ENG under a
// descendant the walk then carries to OPS. The walk's last append used to take
// the mark down anyway, leaving the task in ENG under a root in OPS for good.
// That append now finds it in its own snapshot and keeps the mark up, and the
// walk runs the pass that carries it — declaring the tag it holds in OPS, as
// the first run declares the subtree's, rather than moving it holding a slug
// OPS never declared.
func TestAMoveCarriesATaskFiledBehindItsWalk(t *testing.T) {
	t.Parallel()
	r := lateTagFixture(t)
	lossy, log := r.lossyWriter(t)
	fileBehind(t, r, log, "m-late", "m-kid-a", nil)
	if _, err := lossy.MoveTaskToProject(t.Context(),
		statelog.NewOpID(time.Now(), "move"), "m-root", "OPS", nil); err != nil {
		t.Fatalf("a move with a task filed behind it: %v", err)
	}
	r.drain()
	late := oneTask(t, r, "m-late")
	if late.Project != "OPS" || !strings.HasPrefix(late.Key, "OPS-") {
		t.Errorf("m-late is %s in %q, want an OPS key in OPS — left behind in "+
			"ENG under a root in OPS", late.Key, late.Project)
	}
	if oneTask(t, r, "m-root").Moving {
		t.Error("the root is still marked mid-move once everything under it moved")
	}
	if got := r.strings(`SELECT id FROM tracker_tasks WHERE inconsistent_project = 1`); len(got) != 0 {
		t.Errorf("tasks %v are still in another project than their root", got)
	}
	if got := r.strings(`SELECT label FROM tracker_tags
		WHERE project_key = 'OPS' AND slug = 'late-tag'`); len(got) != 1 || got[0] != "Late" {
		t.Errorf("OPS declares late-tag as %v, want it as ENG does — the task "+
			"arrived carrying a slug its new project never declared", got)
	}
}

// AND ONE THE MOVE CANNOT CARRY KEEPS ITS MARK UP, NAMED, until it can.
//
// Filed behind the walk and then put in the trash, the task is frozen: nothing
// moves it until somebody restores it. The walk's last append used to take the
// mark down over it all the same, so once restored it was live in ENG under a
// root in OPS with nothing left to carry it. The mark now stays up, the move
// says which task it is waiting for, and the duty carries it after the
// restore.
func TestAMoveKeepsItsMarkOverATaskFiledBehindItThatItCannotCarry(t *testing.T) {
	t.Parallel()
	r := lateTagFixture(t)
	lossy, log := r.lossyWriter(t)
	fileBehind(t, r, log, "m-late", "m-kid-a", func() {
		r.drain()
		if _, err := r.writer.RemoveTask(t.Context(), "op-remove-late", "m-late",
			"ENG", false, nil); err != nil {
			t.Errorf("put the task in the trash: %v", err)
		}
	})
	_, err := lossy.MoveTaskToProject(t.Context(),
		statelog.NewOpID(time.Now(), "move"), "m-root", "OPS", nil)
	if err == nil || !strings.Contains(err.Error(), "m-late") {
		t.Fatalf("a move over a task in the trash it cannot carry = %v, want an "+
			"error naming m-late", err)
	}
	r.drain()
	if !oneTask(t, r, "m-root").Moving {
		t.Fatal("the root's mark came down over a task still in ENG")
	}
	// THE ROOT MOVED, so this is a stop naming what it waits for — the two
	// tasks the walk read went, and the one in the trash did not.
	var stopped *tracker.MoveStopped
	if !errors.As(err, &stopped) {
		t.Fatalf("the move answered %v, want a *tracker.MoveStopped", err)
	}
	if late := oneTask(t, r, "m-late"); stopped.Waiting != late.Key ||
		stopped.Followed != 2 || stopped.Of != 3 {
		t.Errorf("the stop is %+v, want it waiting for %s with 2 of 3 followed",
			*stopped, late.Key)
	}

	if _, err := r.writer.RestoreTask(t.Context(), "op-restore-late", "m-late",
		"ENG", nil); err != nil {
		t.Fatalf("restore the task: %v", err)
	}
	r.drain()
	if _, err := trackerWorker(t, r).Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	r.drain()
	if late := oneTask(t, r, "m-late"); late.Project != "OPS" {
		t.Errorf("m-late is in %q after the restore and the duty, want OPS",
			late.Project)
	}
	if oneTask(t, r, "m-root").Moving {
		t.Error("the root is still marked mid-move once everything under it moved")
	}
}

// A TASK A LIVE MOVE HAS NOT CARRIED KEEPS ITS PARENT UNTIL IT HAS.
//
// The walk carries every task it read, however that task has been filed since —
// so one moved out from under the subtree in the middle of the walk was still
// re-keyed into OPS, under a parent it had just been filed under in ENG. Both
// a re-parent and making it a root are unavailable while the mark says the
// walk has not reached it, and the walk then carries it with its subtree.
func TestATaskAMoveHasNotCarriedKeepsItsParent(t *testing.T) {
	t.Parallel()
	r := moveFixture(t, "m-kid-a", "m-kid-b")
	if _, err := r.writer.CreateTask(t.Context(), "op-other", newTask("other"), nil); err != nil {
		t.Fatalf("file another ENG task: %v", err)
	}
	r.drain()
	lossy, log := r.lossyWriter(t)
	log.afterAppendTo("m-root", func() {
		// THIS NODE SEES THE MARK: the case is the one the refusal is
		// for, not the ordering only the walk's last append can catch.
		r.drain()
		for name, parent := range map[string]string{
			"a re-parent": "other", "making it a root": "",
		} {
			_, err := r.writer.UpdateTask(t.Context(), "op-"+name, "m-kid-b", "ENG",
				tracker.NoIfMatch, tracker.TaskPatch{Parent: &parent},
				tracker.ChangeReparented, nil)
			if !errors.Is(err, statelog.ErrUnavailable) {
				t.Errorf("%s of a task the move has not carried = %v, want "+
					"statelog.ErrUnavailable", name, err)
			}
		}
	})
	if _, err := lossy.MoveTaskToProject(t.Context(),
		statelog.NewOpID(time.Now(), "move"), "m-root", "OPS", nil); err != nil {
		t.Fatalf("the move: %v", err)
	}
	r.drain()
	kid := oneTask(t, r, "m-kid-b")
	if kid.Project != "OPS" || kid.Parent == nil || *kid.Parent != "m-root" {
		t.Errorf("m-kid-b is in %q under %v, want in OPS under m-root", kid.Project,
			kid.Parent)
	}
	if got := r.strings(`SELECT id FROM tracker_tasks WHERE inconsistent_project = 1`); len(got) != 0 {
		t.Errorf("tasks %v are in another project than their root", got)
	}

	// AND ONCE CARRIED IT IS A TASK LIKE ANY OTHER: filed under a task in
	// the project it is in now, it goes.
	umbrella := newTask("umbrella")
	umbrella.Key, umbrella.Project = "", "OPS"
	if _, err := r.writer.CreateTask(t.Context(), "op-umbrella", umbrella, nil); err != nil {
		t.Fatalf("file an OPS task: %v", err)
	}
	r.drain()
	onto := "umbrella"
	if _, err := r.writer.UpdateTask(t.Context(), "op-onto", "m-kid-b", "OPS",
		tracker.NoIfMatch, tracker.TaskPatch{Parent: &onto},
		tracker.ChangeReparented, nil); err != nil {
		t.Fatalf("re-parent the carried task: %v", err)
	}
}

// THE MARK HOLDS ITS SUBTREE WHEREVER THE MARKED TASK IS FILED.
//
// The refusal used to ask the task's ROOT whether it was mid-move, and the mark
// is on the task that MOVED — so once that task was filed under another, every
// task its walk had not carried read as settled and took a new subtask the walk
// would never carry. It is asked of every task above, and the duty still
// finishes the walk from the marked task.
func TestAMarkedTaskFiledUnderAnotherStillHoldsItsSubtree(t *testing.T) {
	t.Parallel()
	r := moveFixture(t, "m-kid-a", "m-kid-b")
	stopMoveAt(t, r, "m-kid-b")
	umbrella := newTask("umbrella")
	umbrella.Key, umbrella.Project = "", "OPS"
	if _, err := r.writer.CreateTask(t.Context(), "op-umbrella", umbrella, nil); err != nil {
		t.Fatalf("file an OPS task: %v", err)
	}
	r.drain()
	onto := "umbrella"
	if _, err := r.writer.UpdateTask(t.Context(), "op-onto", "m-root", "OPS",
		tracker.NoIfMatch, tracker.TaskPatch{Parent: &onto},
		tracker.ChangeReparented, nil); err != nil {
		t.Fatalf("file the marked root under another task: %v", err)
	}
	r.drain()
	if root := r.strings(`SELECT root_id FROM tracker_tasks WHERE id = ?`,
		"m-kid-b"); len(root) != 1 || root[0] != "umbrella" {
		t.Fatalf("the premise: m-kid-b's root is %q, want umbrella — the marked "+
			"task is no longer anybody's root", root)
	}

	behind := newTask("m-late")
	behind.Key = ""
	under := "m-kid-b"
	behind.Parent, behind.Depth = &under, 3
	if _, err := r.writer.CreateTask(t.Context(), "op-late", behind, nil); !errors.Is(err, statelog.ErrUnavailable) {
		t.Fatalf("a subtask under a task the move has not carried, below a "+
			"marked task that is no longer a root = %v, want "+
			"statelog.ErrUnavailable", err)
	}

	if _, err := trackerWorker(t, r).Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	r.drain()
	if kid := oneTask(t, r, "m-kid-b"); kid.Project != "OPS" {
		t.Errorf("m-kid-b is in %q after the duty, want OPS", kid.Project)
	}
	if oneTask(t, r, "m-root").Moving {
		t.Error("the marked task is still marked once its walk finished")
	}
}

// A MERGE THAT WOULD MOVE SUBTASKS WAITS FOR A MOVE TO CARRY EITHER END, and a
// merge with nothing to carry does not.
//
// The walk carries the duplicate and its subtasks whatever the merge does in
// between, so re-parenting them onto a survivor in the old project splits them
// across two projects, and onto a survivor the walk has not carried is a child
// it never read. Which project each end is in is about to change, so it is
// unavailable rather than refused as a cross-project merge.
func TestAMergeWaitsForAMoveToCarryItsEnds(t *testing.T) {
	t.Parallel()
	r := moveFixture(t, "m-kid-a", "m-kid-b")
	// m-kid-b gets a subtask of its own before the move, so it has
	// something for a merge to carry.
	grand := newTask("m-grand")
	grand.Key = ""
	parent := "m-kid-b"
	grand.Parent, grand.Depth = &parent, 2
	if _, err := r.writer.CreateTask(t.Context(), "op-grand", grand, nil); err != nil {
		t.Fatalf("file m-kid-b's subtask: %v", err)
	}
	r.drain()
	if _, err := r.writer.CreateTask(t.Context(), "op-keep", newTask("keep"), nil); err != nil {
		t.Fatalf("file the survivor: %v", err)
	}
	r.drain()
	stopMoveAt(t, r, "m-kid-b")

	end := r.logEnd(t)
	_, err := r.writer.MergeDuplicates(t.Context(),
		statelog.NewOpID(time.Now(), "merge"), "m-kid-b", "keep", true, nil)
	if !errors.Is(err, statelog.ErrUnavailable) {
		t.Fatalf("a merge moving the subtasks of a task the move has not "+
			"carried = %v, want statelog.ErrUnavailable", err)
	}
	if errors.Is(err, tracker.ErrReparentAcrossProjects) {
		t.Errorf("the merge was refused as across projects, which the move is "+
			"about to change: %v", err)
	}
	if got := r.logEnd(t); got != end {
		t.Errorf("the merge that waited put %d record(s) on the log", got-end)
	}

	// NOTHING TO CARRY, NOTHING TO WAIT FOR: m-grand has no subtask, and
	// folding it moves nothing onto the survivor.
	if _, err := r.writer.MergeDuplicates(t.Context(),
		statelog.NewOpID(time.Now(), "merge-leaf"), "m-grand", "keep", true,
		nil); err != nil {
		t.Fatalf("a merge with no subtask to carry: %v", err)
	}
}

// A CHART APPLY RECONCILES EVERY PROJECT IT CAN, and names every one it could
// not.
//
// The loop said one project's refusal must not leave the rest of the company
// unable to file anything, and returned at the first one — so a chart whose
// first project met a full stream reconciled nothing after it, on every apply,
// for as long as that one kept failing.
func TestAChartApplyReconcilesEveryProjectItCan(t *testing.T) {
	t.Parallel()
	r := newRoundTripWithoutProject(t)
	lossy, log := r.lossyWriter(t)
	log.refuse("AAA")

	wrote, err := lossy.ApplyChart(t.Context(), activatedAt(100), []tracker.ChartProject{
		{Key: "AAA", Name: "Refused", Unit: "Eng"},
		{Key: "BBB", Name: "Written", Unit: "Eng"},
	})
	if err == nil || !strings.Contains(err.Error(), "AAA") {
		t.Fatalf("the apply = %v, want an error naming AAA", err)
	}
	if len(wrote) != 1 || wrote[0] != "BBB" {
		t.Fatalf("the apply wrote %v, want [BBB] — the project after the "+
			"refused one", wrote)
	}
	r.drain()
	if name := r.projectName("BBB"); name != "Written" {
		t.Errorf("BBB is named %q", name)
	}
}

// A GENERATION RECORD CARRIES WHO RAN THE REANCHOR, THEIR KIND AND THE
// CREDENTIAL THEY RAN IT THROUGH — the three columns every other record does.
//
// It recorded every operator as kind `operator` with their NAME as the
// credential, so a person's reanchor through a machine token read as a token
// named after them, and the credential that ran it was on no record.
func TestAGenerationRecordNamesItsOperatorsKindAndCredential(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		facts                  statelog.GenerationFacts
		author, kind, operator string
	}{
		"a person through a machine token": {
			facts: statelog.GenerationFacts{By: "jane.doe", ByKind: "human",
				OperatorID: "pat:0193"},
			author: "jane.doe", kind: "human", operator: "pat:0193",
		},
		"a principal's kind": {
			facts:  statelog.GenerationFacts{By: "jane.doe", ByKind: "person"},
			author: "jane.doe", kind: "human",
		},
		"a token under its own login": {
			facts:  statelog.GenerationFacts{By: "token:ops", ByKind: "operator"},
			author: "token:ops", kind: "operator", operator: "token:ops",
		},
		"a kind this build does not know": {
			facts:  statelog.GenerationFacts{By: "ops-1"},
			author: "ops-1", kind: "operator", operator: "ops-1",
		},
		"nobody named": {
			facts:  statelog.GenerationFacts{},
			author: "node-a", kind: "system",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			facts := tc.facts
			facts.Generation, facts.Writer, facts.At = 2, "node-a", wednesday
			generation, _, err := tracker.GenerationRecord{}.GenerationRecord(facts)
			if err != nil {
				t.Fatalf("encode the generation: %v", err)
			}
			record, err := tracker.Decode(generation.Payload)
			if err != nil {
				t.Fatalf("decode the generation: %v", err)
			}
			if record.Actor != tc.author || string(record.ActorKind) != tc.kind ||
				record.OperatorID != tc.operator {
				t.Errorf("the record names %q (%s) through %q, want %q (%s) "+
					"through %q", record.Actor, record.ActorKind,
					record.OperatorID, tc.author, tc.kind, tc.operator)
			}
		})
	}
}

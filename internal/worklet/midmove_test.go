package worklet_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// fileBehind files a subtask under a descendant of the moving subtree after
// the root's move lands but before this node applies it, as a node behind the
// log would decide: root unmarked, parent still in ENG. then, if set, runs in
// the same hook once the task is filed.
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
		worklet.TagEdit{Add: []worklet.Tag{{Slug: "late-tag", Label: "Late"}}},
		worklet.TagAuthority{}); err != nil {
		t.Fatalf("declare ENG's tag: %v", err)
	}
	r.drain()
	return r
}

// A TASK FILED BEHIND A MOVE BY A NODE THE MARK HAD NOT REACHED IS CARRIED BY
// THE MOVE, tags and all.
//
// A create under a descendant is refused mid-move only on a node that has
// applied the root's move; this one is decided before, so it lands in ENG
// under a descendant the walk carries to OPS. The walk's last append must find
// it in its snapshot, keep the mark up and carry it in another pass, declaring
// its tag in OPS rather than moving it with a slug OPS never declared.
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
// Filed behind the walk and then trashed, the task is frozen until restored.
// Taking the mark down over it would leave it, once restored, in ENG under a
// root in OPS with nothing to carry it; so the mark stays up, the move names
// the task it waits for, and the duty carries it after the restore.
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
	// NAMED BY ITS KEY, which is how the person reading it knows it.
	late := oneTask(t, r, "m-late").Key
	if err == nil || late == "" || !strings.Contains(err.Error(), late) {
		t.Fatalf("a move over a task in the trash it cannot carry = %v, want an "+
			"error naming m-late by its key %q", err, late)
	}
	r.drain()
	if !oneTask(t, r, "m-root").Moving {
		t.Fatal("the root's mark came down over a task still in ENG")
	}
	// THE ROOT MOVED, so this is a stop naming what it waits for — the two
	// tasks the walk read went, and the one in the trash did not.
	var stopped *worklet.MoveStopped
	if !errors.As(err, &stopped) {
		t.Fatalf("the move answered %v, want a *worklet.MoveStopped", err)
	}
	if late := oneTask(t, r, "m-late"); stopped.Waiting != late.Key ||
		stopped.Followed != 2 || stopped.Of != 3 {
		t.Errorf("the stop is %+v, want it waiting for %s with 2 of 3 followed",
			*stopped, late.Key)
	}

	if _, err := r.writer.RestoreTask(t.Context(), "op-restore-late", "m-late",
		"ENG", false, nil); err != nil {
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
// The walk carries every task it read, however it has been filed since, so a
// re-parent or making it a root is unavailable while the mark says the walk
// has not reached it; it would be re-keyed into OPS under an ENG parent.
func TestATaskAMoveHasNotCarriedKeepsItsParent(t *testing.T) {
	t.Parallel()
	r := moveFixture(t, "m-kid-a", "m-kid-b")
	if _, err := r.writer.CreateTask(t.Context(), "op-other", newTask("other"), nil); err != nil {
		t.Fatalf("file another ENG task: %v", err)
	}
	r.drain()
	lossy, log := r.lossyWriter(t)
	log.afterAppendTo("m-root", func() {
		// THIS NODE SEES THE MARK: the refusal's case, not the ordering only
		// the walk's last append can catch.
		r.drain()
		for name, parent := range map[string]string{
			"a re-parent": "other", "making it a root": "",
		} {
			_, err := r.writer.UpdateTask(t.Context(), "op-"+name, "m-kid-b", "ENG",
				worklet.NoIfMatch, worklet.TaskPatch{Parent: &parent},
				worklet.ChangeReparented, nil)
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

	// AND ONCE CARRIED IT IS A TASK LIKE ANY OTHER in its new project.
	umbrella := newTask("umbrella")
	umbrella.Key, umbrella.Project = "", "OPS"
	if _, err := r.writer.CreateTask(t.Context(), "op-umbrella", umbrella, nil); err != nil {
		t.Fatalf("file an OPS task: %v", err)
	}
	r.drain()
	onto := "umbrella"
	if _, err := r.writer.UpdateTask(t.Context(), "op-onto", "m-kid-b", "OPS",
		worklet.NoIfMatch, worklet.TaskPatch{Parent: &onto},
		worklet.ChangeReparented, nil); err != nil {
		t.Fatalf("re-parent the carried task: %v", err)
	}
}

// THE MARK HOLDS ITS SUBTREE WHEREVER THE MARKED TASK IS FILED.
//
// The mark is on the task that MOVED, not the root, so once it is filed under
// another task the refusal must ask every task above, not only the root; the
// duty still finishes the walk from the marked task.
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
		worklet.NoIfMatch, worklet.TaskPatch{Parent: &onto},
		worklet.ChangeReparented, nil); err != nil {
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
// The walk carries the duplicate and its subtasks whatever the merge does, so
// re-parenting them would split them across projects or add a child the walk
// never read. Each end's project is about to change, so it is unavailable
// rather than refused as a cross-project merge.
func TestAMergeWaitsForAMoveToCarryItsEnds(t *testing.T) {
	t.Parallel()
	r := moveFixture(t, "m-kid-a", "m-kid-b")
	// m-kid-b gets a subtask, so a merge has something to carry.
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
	if errors.Is(err, worklet.ErrReparentAcrossProjects) {
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
// One project's refusal must not stop the projects after it from reconciling.
func TestAChartApplyReconcilesEveryProjectItCan(t *testing.T) {
	t.Parallel()
	r := newRoundTripWithoutProject(t)
	lossy, log := r.lossyWriter(t)
	log.refuse("AAA")

	wrote, err := lossy.ApplyChart(t.Context(), activatedAt(100), []worklet.ChartProject{
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
// A person's reanchor through a machine token records the person, kind human,
// and the token as the credential, not a token named after them.
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
		// A NAME OF SPACES NAMES NOBODY, as on every other log's generation
		// record ([statelog.GenerationFacts.Author]).
		"a name of spaces": {
			facts:  statelog.GenerationFacts{By: "  ", ByKind: "human"},
			author: "node-a", kind: "system",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			facts := tc.facts
			facts.Generation, facts.Writer, facts.At = 2, "node-a", wednesday
			generation, _, err := worklet.GenerationRecord{}.GenerationRecord(facts)
			if err != nil {
				t.Fatalf("encode the generation: %v", err)
			}
			record, err := worklet.Decode(generation.Payload)
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

// A MOVED TASK IS FOUND BY ITS TAGS IN THE PROJECT IT MOVED TO, AND ONLY THERE.
//
// A project's tag filter seeks the slug and project together, so it is right
// only while every tag row carries its task's CURRENT project: the applier
// rewrites a task's tag rows with the task on every apply.
func TestAMovedTaskIsFoundByItsTagsInItsNewProject(t *testing.T) {
	t.Parallel()
	r := lateTagFixture(t)
	tagged := newTask("m-tagged")
	tagged.Key = "ENG-tagged"
	tagged.Tags = []string{"late-tag"}
	if _, err := r.writer.CreateTask(t.Context(), "op-tagged", tagged, nil); err != nil {
		t.Fatalf("file the tagged task: %v", err)
	}
	r.drain()
	if _, err := r.writer.MoveTaskToProject(t.Context(),
		statelog.NewOpID(time.Now(), "move"), "m-tagged", "OPS", nil); err != nil {
		t.Fatalf("move the tagged task: %v", err)
	}
	r.drain()
	for container, want := range map[string]int{"project:OPS": 1, "project:ENG": 0} {
		for _, filter := range []string{"late-tag", "all:late-tag"} {
			got := ids(r.ask(map[string]any{"container": container, "tag": filter,
				"subtasks": "separate"}))
			if len(got) != want || (want == 1 && got[0] != "m-tagged") {
				t.Errorf("tag=%s in %s answers %v after the move, want %d "+
					"task(s) — the tag rows still carry the project it left",
					filter, container, got, want)
			}
		}
	}
}

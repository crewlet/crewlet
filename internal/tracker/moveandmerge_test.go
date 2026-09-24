package tracker_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A MOVE CARRIES ITS SUBTREE'S TAGS INTO THE TARGET, as its source declares
// them, and leaves the target's own alone.
//
// The step took the tags from its caller, and every caller passed none — so it
// never ran, and a moved task arrived in its new project carrying a label the
// project had never declared: a grouping on the card that no filter in that
// project offers. And had it run, it wrote the target's WHOLE set composed
// from a read outside any snapshot.
func TestAMoveCarriesItsSubtreesTagsIntoTheTarget(t *testing.T) {
	t.Parallel()
	r := moveFixture(t, "m-kid")
	for project, tag := range map[string]tracker.Tag{
		"ENG": {Slug: "api", Label: "API", Color: "#336699"},
		"OPS": {Slug: "ops-only", Label: "Ops only"},
	} {
		if _, err := r.writer.WriteTags(t.Context(), "op-tags-"+project, project,
			tracker.TagEdit{Add: []tracker.Tag{tag}}, tracker.TagAuthority{}); err != nil {
			t.Fatalf("declare %s's tag: %v", project, err)
		}
		r.drain()
	}
	tags := []string{"api"}
	if _, err := r.writer.UpdateTask(t.Context(), "op-tag-kid", "m-kid", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Tags: &tags}, tracker.ChangeTags, nil); err != nil {
		t.Fatalf("tag the subtask: %v", err)
	}
	r.drain()

	if _, err := r.writer.MoveTaskToProject(t.Context(),
		statelog.NewOpID(time.Now(), "move"), "m-root", "OPS", nil); err != nil {
		t.Fatalf("the move: %v", err)
	}
	r.drain()
	detail := r.project(tracker.ProjectDetailQuery{Project: "OPS"})
	var api *tracker.Tag
	var slugs []string
	for i, tag := range detail.Tags {
		slugs = append(slugs, tag.Slug)
		if tag.Slug == "api" {
			api = &detail.Tags[i]
		}
	}
	if !slices.Contains(slugs, "ops-only") {
		t.Errorf("OPS lost its own tag to the move: %v", slugs)
	}
	if api == nil || api.Label != "API" || api.Color != "#336699" {
		t.Errorf("OPS declares %v after the move, want the subtask's `api` "+
			"with the label and colour ENG gave it", detail.Tags)
	}
}

// A MOVE WAKES THE PEOPLE ON THE ROOT. Its step was published with no
// notification, though the kind routes and falls back to a lead exactly as a
// status change does — so a task could leave its project, change its key, and
// nobody on it heard.
func TestAMoveWakesThePeopleOnTheRoot(t *testing.T) {
	t.Parallel()
	r := moveFixture(t)
	if _, err := r.writer.MoveTaskToProject(t.Context(),
		statelog.NewOpID(time.Now(), "move"), "m-root", "OPS", &tracker.Notify{
			Kind:     tracker.ChangeMoved,
			Snapshot: tracker.Snapshot{Key: "ENG-1", Project: "OPS", Assignee: "ana"},
		}); err != nil {
		t.Fatalf("the move: %v", err)
	}
	r.drain()
	if !historyNotified(t, r, "task", "m-root") {
		t.Error("the root's move carries no notification, so nobody on the " +
			"task heard it had left its project")
	}
}

// A MERGE REFUSES TO CARRY SUBTASKS INTO ANOTHER PROJECT, before its first
// append. A subtask under an item in another project is drawn under it on
// neither board and sits in the attention queue as `inconsistent_project`,
// which only a move of its root clears — and the docs told a reader to "move
// it" when nothing could.
func TestAMergeRefusesToCarrySubtasksIntoAnotherProject(t *testing.T) {
	t.Parallel()
	r := moveFixture(t, "m-kid")
	keep := newTask("keep")
	keep.Project = "OPS"
	if _, err := r.writer.CreateTask(t.Context(), "op-keep", keep, nil); err != nil {
		t.Fatalf("file the survivor: %v", err)
	}
	r.drain()
	end := r.logEnd(t)

	_, err := r.writer.MergeDuplicates(t.Context(), statelog.NewOpID(time.Now(), "merge"),
		"m-root", "keep", true, nil)
	if !errors.Is(err, tracker.ErrReparentAcrossProjects) {
		t.Fatalf("a merge re-parenting ENG subtasks onto an OPS item = %v, want "+
			"ErrReparentAcrossProjects", err)
	}
	if got := r.logEnd(t); got != end {
		t.Fatalf("the refused merge put %d record(s) on the log", got-end)
	}

	// WITHOUT RE-PARENTING there is nothing to carry, and it folds.
	if _, err := r.writer.MergeDuplicates(t.Context(), statelog.NewOpID(time.Now(), "merge"),
		"m-root", "keep", false, nil); err != nil {
		t.Fatalf("a merge that moves no subtask: %v", err)
	}
	r.drain()
	if kid := oneTask(t, r, "m-kid"); kid.Parent == nil || *kid.Parent != "m-root" {
		t.Errorf("m-kid moved to %v on a merge that re-parents nothing", kid.Parent)
	}
}

// AND A SUBTASK FILED UNDER THE DUPLICATE AFTER THAT CHECK IS NOT CARRIED
// EITHER: the walk selects only children in the survivor's own project, so the
// invariant does not rest on a read taken before the first append.
func TestAMergeWalkNeverCarriesASubtaskAcrossProjects(t *testing.T) {
	t.Parallel()
	r := moveFixture(t)
	keep := newTask("keep")
	keep.Project = "OPS"
	if _, err := r.writer.CreateTask(t.Context(), "op-keep", keep, nil); err != nil {
		t.Fatalf("file the survivor: %v", err)
	}
	r.drain()
	lossy, log := r.lossyWriter(t)
	// THE LATE SUBTASK arrives once the merge's mark has landed — after the
	// pre-flight found the duplicate childless.
	log.afterAppendTo("m-root", func() {
		late := newTask("m-late")
		parent := "m-root"
		late.Parent, late.Depth = &parent, 1
		if _, err := r.writer.CreateTask(t.Context(), "op-late", late, nil); err != nil {
			t.Errorf("file the late subtask: %v", err)
		}
		r.drain()
	})
	if _, err := lossy.MergeDuplicates(t.Context(), statelog.NewOpID(time.Now(), "merge"),
		"m-root", "keep", true, nil); err != nil {
		t.Fatalf("the merge: %v", err)
	}
	r.drain()
	late := oneTask(t, r, "m-late")
	if late.Parent == nil || *late.Parent != "m-root" {
		t.Errorf("the late ENG subtask was re-parented onto %v, an item in OPS",
			late.Parent)
	}
}

// A MOVE WHOSE TAG CLASHES WITH THE TARGET'S IS REFUSED BEFORE ITS FIRST APPEND,
// naming the clash — never half-made, and never a sentence nobody can act on.
//
// Two projects may each have a tag labelled "API" under different slugs, and a
// subtree carrying ENG's cannot declare it in OPS without making two tags
// nobody could tell apart. The refusal came from the declaration — the move's
// first append — as a bare error the tool rendered as "the change was NOT
// made". It is now a TagClash naming both tags, and the move itself is not
// touched: no alias, no key, no mark. Declared in OPS under a label of its own,
// the same move goes through.
func TestAMoveWhoseTagClashesWithTheTargetsIsRefusedNamingIt(t *testing.T) {
	t.Parallel()
	r := moveFixture(t, "m-kid")
	for project, tag := range map[string]tracker.Tag{
		"ENG": {Slug: "api", Label: "API"},
		"OPS": {Slug: "backend-api", Label: "API"},
	} {
		if _, err := r.writer.WriteTags(t.Context(), "op-tags-"+project, project,
			tracker.TagEdit{Add: []tracker.Tag{tag}}, tracker.TagAuthority{}); err != nil {
			t.Fatalf("declare %s's tag: %v", project, err)
		}
		r.drain()
	}
	tags := []string{"api"}
	if _, err := r.writer.UpdateTask(t.Context(), "op-tag-kid", "m-kid", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Tags: &tags}, tracker.ChangeTags, nil); err != nil {
		t.Fatalf("tag the subtask: %v", err)
	}
	r.drain()

	_, err := r.writer.MoveTaskToProject(t.Context(),
		statelog.NewOpID(time.Now(), "move"), "m-root", "OPS", nil)
	var clash *tracker.TagClash
	if !errors.As(err, &clash) {
		t.Fatalf("the move = %v, want a TagClash naming the two tags", err)
	}
	if clash.Project != "OPS" || clash.Slug != "api" || clash.Other.Slug != "backend-api" {
		t.Errorf("the clash names %+v, want ENG's api against OPS's backend-api", clash)
	}
	r.drain()
	if got := oneTask(t, r, "m-root"); got.Project != "ENG" || got.Moving {
		t.Errorf("the refused move left the root in %q, mid-move %v", got.Project, got.Moving)
	}

	// DECLARED IN OPS UNDER A LABEL OF ITS OWN, the same move goes through
	// — the add is then a tag OPS already has, which is no clash.
	if _, err := r.writer.WriteTags(t.Context(), "op-tags-ops-api", "OPS",
		tracker.TagEdit{Add: []tracker.Tag{{Slug: "api", Label: "API (from ENG)"}}},
		tracker.TagAuthority{}); err != nil {
		t.Fatalf("declare api in OPS: %v", err)
	}
	r.drain()
	if _, err := r.writer.MoveTaskToProject(t.Context(),
		statelog.NewOpID(time.Now(), "move"), "m-root", "OPS", nil); err != nil {
		t.Fatalf("the move after the remedy: %v", err)
	}
	r.drain()
	if got := oneTask(t, r, "m-kid"); got.Project != "OPS" {
		t.Errorf("the subtask is in %q after the move, want OPS", got.Project)
	}
}

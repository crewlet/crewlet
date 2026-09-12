package tracker_test

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// removed reports whether the reader still holds the task as removed.
func removed(t *testing.T, r *roundTrip, id string) bool {
	t.Helper()
	detail, err := r.reader.Task(t.Context(), id, tracker.DetailWants{},
		statelog.ReadStale)
	if err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	return detail.Task.Removed != nil
}

// THE TRASH IS REACHABLE FROM BOTH ENDS.
//
// Tombstone was defined, applied, routed as a `removed` notification and
// readable with `removed=true` — and nothing in the tree could produce one. A
// task could be PURGED, destroying every row on every node with no inverse,
// and could not be removed; the restore path the applier carries could never
// run at all.
func TestATaskCanBeRemovedAndRestored(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if _, err := r.writer.CreateTask(t.Context(), "op-create",
		newTask("t-gone"), nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r.drain()

	if _, err := r.writer.RemoveTask(t.Context(), "op-remove", "t-gone", "ENG",
		false, nil); err != nil {
		t.Fatalf("RemoveTask: %v", err)
	}
	r.drain()
	if !removed(t, r, "t-gone") {
		t.Fatal("the task is not removed after a removal")
	}

	// AND IT IS OUT OF EVERY ORDINARY LIST, which is the whole of what a
	// removal does — while `removed=true` is the only thing that shows it.
	if got := ids(r.ask(map[string]any{"container": "project:ENG"})); len(got) != 0 {
		t.Errorf("the board still lists %v after the removal", got)
	}
	if got := ids(r.ask(map[string]any{
		"container": "project:ENG", "removed": "true",
	})); len(got) != 1 || got[0] != "t-gone" {
		t.Errorf("the trash lists %v, want the removed task", got)
	}

	// AND IT IS FROZEN: an ordinary patch is refused rather than silently
	// editing something nobody can see.
	title := "a new title"
	if _, err := r.writer.UpdateTask(t.Context(), "op-edit", "t-gone", "ENG", 0,
		tracker.TaskPatch{Title: &title}, nil); err == nil {
		t.Error("a removed task took an ordinary patch, so the tombstone is a " +
			"flag rather than a freeze")
	}

	// AND IT COMES BACK, at any age and with one commit.
	if _, err := r.writer.RestoreTask(t.Context(), "op-restore", "t-gone",
		"ENG", nil); err != nil {
		t.Fatalf("RestoreTask: %v", err)
	}
	r.drain()
	if removed(t, r, "t-gone") {
		t.Fatal("the task is still removed after a restore")
	}
	if got := ids(r.ask(map[string]any{"container": "project:ENG"})); len(got) != 1 {
		t.Errorf("the board lists %v after the restore, want the task back", got)
	}
}

// A SUBTREE LEAVES TOGETHER AND COMES BACK TOGETHER — and brings back only
// what it took.
//
// Removing a parent and leaving its children reachable is the shape that makes
// orphans somebody then has to find. `removed_with` is what makes the inverse
// true: a task already in the trash for its own reasons is not restored by
// somebody restoring its parent.
func TestRemovingASubtreeIsReversibleAndBringsBackOnlyWhatItTook(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	root := newTask("t-root")
	if _, err := r.writer.CreateTask(t.Context(), "op-root", root, nil); err != nil {
		t.Fatalf("CreateTask root: %v", err)
	}
	r.drain()
	for _, id := range []string{"t-kid", "t-other"} {
		kid := newTask(id)
		kid.Key = "ENG-" + id
		parent := "t-root"
		kid.Parent = &parent
		kid.Depth = 1
		if _, err := r.writer.CreateTask(t.Context(), "op-"+id, kid, nil); err != nil {
			t.Fatalf("CreateTask %s: %v", id, err)
		}
		r.drain()
	}

	// ONE CHILD IS ALREADY IN THE TRASH FOR ITS OWN REASONS.
	if _, err := r.writer.RemoveTask(t.Context(), "op-own", "t-other", "ENG",
		false, nil); err != nil {
		t.Fatalf("remove the other child: %v", err)
	}
	r.drain()

	if _, err := r.writer.RemoveTask(t.Context(), "op-subtree", "t-root", "ENG",
		true, nil); err != nil {
		t.Fatalf("RemoveTask subtree: %v", err)
	}
	r.drain()
	for _, id := range []string{"t-root", "t-kid", "t-other"} {
		if !removed(t, r, id) {
			t.Errorf("%s survived the subtree removal", id)
		}
	}

	if _, err := r.writer.RestoreTask(t.Context(), "op-restore", "t-root",
		"ENG", nil); err != nil {
		t.Fatalf("RestoreTask: %v", err)
	}
	r.drain()
	if removed(t, r, "t-root") || removed(t, r, "t-kid") {
		t.Error("the subtree removal's own tasks did not come back with the root")
	}
	if !removed(t, r, "t-other") {
		t.Error("a task that was already in the trash for its own reasons was " +
			"restored by somebody restoring its parent — `removed_with` is " +
			"what stops that, and it is the whole reason a restore takes one " +
			"argument")
	}
}

// REMOVING A REMOVED TASK IS NOTHING TO DO, AND IT SUCCEEDS.
//
// A subtree removal that half-finished has to be able to be re-run, so
// "already in the state you asked for" is a success rather than a conflict.
func TestRemovingAndRestoringAreIdempotent(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if _, err := r.writer.CreateTask(t.Context(), "op-create",
		newTask("t-twice"), nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r.drain()
	for _, op := range []string{"op-a", "op-b"} {
		if _, err := r.writer.RemoveTask(t.Context(), op, "t-twice", "ENG",
			false, nil); err != nil {
			t.Fatalf("RemoveTask %s: %v", op, err)
		}
		r.drain()
	}
	if !removed(t, r, "t-twice") {
		t.Fatal("the task is not removed after two removals")
	}
	for _, op := range []string{"op-c", "op-d"} {
		if _, err := r.writer.RestoreTask(t.Context(), op, "t-twice", "ENG",
			nil); err != nil {
			t.Fatalf("RestoreTask %s: %v", op, err)
		}
		r.drain()
	}
	if removed(t, r, "t-twice") {
		t.Fatal("the task is still removed after two restores")
	}
}

// A CROSS-PROJECT MOVE CARRIES THE SUBTREE, and until now it could not read
// one.
//
// readSubtree's recursive CTE named the column `parent`, and the column is
// `parent_id` — so every walk that had a descendant to carry failed at the
// first statement with a parse error, and the only reason nothing noticed is
// that MoveTaskToProject had no test at all. It is the trash's own walk too:
// a subtree removal reads the same function.
func TestACrossProjectMoveCarriesTheSubtree(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if _, err := r.writer.WriteDocument(t.Context(), "op-ops",
		tracker.ProjectSubject("OPS"), "", tracker.Project{
			V: 1, Key: "OPS", Name: "Operations",
			CreatedAt: wednesday, UpdatedAt: wednesday,
		}, nil); err != nil {
		t.Fatalf("seed the target project: %v", err)
	}
	r.drain()

	if _, err := r.writer.CreateTask(t.Context(), "op-root",
		newTask("m-root"), nil); err != nil {
		t.Fatalf("CreateTask root: %v", err)
	}
	r.drain()
	kid := newTask("m-kid")
	kid.Key = "ENG-2"
	parent := "m-root"
	kid.Parent, kid.Depth = &parent, 1
	if _, err := r.writer.CreateTask(t.Context(), "op-kid", kid, nil); err != nil {
		t.Fatalf("CreateTask kid: %v", err)
	}
	r.drain()

	if _, err := r.writer.MoveTaskToProject(t.Context(), "op-move", "m-root",
		"OPS", nil); err != nil {
		t.Fatalf("MoveTaskToProject: %v", err)
	}
	r.drain()

	for _, id := range []string{"m-root", "m-kid"} {
		got := oneTask(t, r, id)
		if got.Project != "OPS" {
			t.Errorf("%s is in project %q after the move, want OPS — the "+
				"descendant walk is what carries a child across", id, got.Project)
		}
		if !strings.HasPrefix(got.Key, "OPS-") {
			t.Errorf("%s keeps the key %q after the move, and a key names the "+
				"project it is in", id, got.Key)
		}
	}
}

// THE TRASH IS ORDERED BY WHEN WORK WAS REMOVED, never by board rank.
//
// A removed task's rank is its position on a board it is no longer on, so a
// trash listing ordered by it is ordered by a stale number — and the only
// index over removed rows is the partial one on `removed_at`.
func TestTheTrashIsOrderedByRemoval(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for _, id := range []string{"first", "second", "third"} {
		inSprint(t, r, id, nil)
	}
	// REMOVED OUT OF RANK ORDER, each at its own instant, so an answer
	// ordered by rank and one ordered by removal are different lists. A
	// removal's stamp is an AUTHORED instant — it is displayed, and §5's
	// rule is that every displayed instant is the one somebody typed —
	// so moving the writer's clock is what separates them.
	for i, id := range []string{"second", "third", "first"} {
		r.at = wednesday.Add(time.Duration(i) * time.Minute)
		if _, err := r.writer.RemoveTask(t.Context(), "op-rm-"+id, id, "ENG",
			false, nil); err != nil {
			t.Fatalf("RemoveTask %s: %v", id, err)
		}
		r.drain()
	}
	r.at = wednesday

	got := ids(r.ask(map[string]any{
		"container": "project:ENG", "removed": "true",
	}))
	want := []string{"first", "third", "second"}
	if len(got) != len(want) {
		t.Fatalf("the trash answers %v, want the three removed tasks", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("the trash answers %v, want %v — newest removal first, "+
				"not the rank each task held on a board it has left", got, want)
		}
	}
}

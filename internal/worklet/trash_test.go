package worklet_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// removed reports whether the reader still holds the task as removed.
func removed(t *testing.T, r *roundTrip, id string) bool {
	t.Helper()
	detail, err := r.reader.Task(t.Context(), id, worklet.DetailWants{},
		statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	return detail.Task.Removed != nil
}

// THE TRASH IS REACHABLE FROM BOTH ENDS.
//
// Tombstone was defined, applied, routed as a `removed` notification and
// readable with `removed=true` — and nothing in the tree could produce one. A
// task could be PURGED, destroying every row on every data node with no
// inverse, and could not be removed; the restore path the applier carries
// could never run at all.
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
		worklet.TaskPatch{Title: &title}, worklet.ChangeFields, nil); err == nil {
		t.Error("a removed task took an ordinary patch, so the tombstone is a " +
			"flag rather than a freeze")
	}

	// AND IT COMES BACK, at any age and with one commit.
	if _, err := r.writer.RestoreTask(t.Context(), "op-restore", "t-gone",
		"ENG", false, nil); err != nil {
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
		"ENG", false, nil); err != nil {
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

// A RESTORE ASKED FOR ALONE BRINGS BACK ONE TASK OR NOTHING.
//
// Whoever filed an item may take it out of the trash, and what went in with
// it at somebody else's hand is not theirs to bring back — so a restore asked
// for the task alone is refused, writing nothing, when its removal took
// others, and lands like any restore when it took none. The writer decides it
// over the same read the walk is made from, so the refusal and the walk cannot
// disagree about what a restore would touch.
func TestARestoreAskedForAloneBringsBackOneTaskOrNothing(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for _, id := range []string{"t-lone", "t-root"} {
		if _, err := r.writer.CreateTask(t.Context(), "op-"+id, newTask(id), nil); err != nil {
			t.Fatalf("CreateTask %s: %v", id, err)
		}
		r.drain()
	}
	kid := newTask("t-kid")
	kid.Key = "ENG-t-kid"
	parent := "t-root"
	kid.Parent, kid.Depth = &parent, 1
	if _, err := r.writer.CreateTask(t.Context(), "op-kid", kid, nil); err != nil {
		t.Fatalf("CreateTask t-kid: %v", err)
	}
	r.drain()
	if _, err := r.writer.RemoveTask(t.Context(), "op-lone-out", "t-lone", "ENG",
		false, nil); err != nil {
		t.Fatalf("remove t-lone: %v", err)
	}
	if _, err := r.writer.RemoveTask(t.Context(), "op-subtree-out", "t-root", "ENG",
		true, nil); err != nil {
		t.Fatalf("remove the subtree: %v", err)
	}
	r.drain()

	_, err := r.writer.RestoreTask(t.Context(), "op-alone", "t-root", "ENG", true, nil)
	if !errors.Is(err, worklet.ErrRestoresOthers) {
		t.Fatalf("a restore of t-root alone, which took t-kid with it, = %v, "+
			"want ErrRestoresOthers", err)
	}
	if got := worklet.Sentence(err); strings.HasPrefix(got, "worklet:") ||
		!strings.Contains(got, "1 other tasks") {
		t.Errorf("the refusal reads %q to a person, want the writer's own "+
			"sentence naming how many went with it", got)
	}
	r.drain()
	if !removed(t, r, "t-root") || !removed(t, r, "t-kid") {
		t.Fatal("a refused restore brought something back")
	}

	if _, err := r.writer.RestoreTask(t.Context(), "op-lone-in", "t-lone", "ENG",
		true, nil); err != nil {
		t.Fatalf("a restore of a task removed on its own, asked for alone: %v", err)
	}
	r.drain()
	if removed(t, r, "t-lone") {
		t.Error("a task removed on its own did not come back when restored alone")
	}
}

// REMOVING A REMOVED TASK IS NOTHING TO DO, AND IT SUCCEEDS.
//
// A subtree removal that half-finished has to be able to be re-run, so
// "already in the state you asked for" is a success rather than a conflict.
// A restore is the same for a root already back with tasks still in the trash
// behind it — that is exactly what a stopped restore leaves — and for its own
// operation answered again; a NEW restore of a live task with nothing behind
// it is told there is nothing to restore rather than that it restored
// something (see TestARestoreWithNothingToBringBackSaysSo).
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
	for range 2 {
		if _, err := r.writer.RestoreTask(t.Context(), "op-c", "t-twice", "ENG",
			false, nil); err != nil {
			t.Fatalf("RestoreTask op-c: %v", err)
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
		worklet.ProjectSubject("OPS"), "", worklet.Project{
			V: 1, Key: "OPS", Name: "Operations",
			CreatedAt: wednesday, UpdatedAt: wednesday,
		}, worklet.ChangeProjectCreated, nil); err != nil {
		t.Fatalf("seed the target project: %v", err)
	}
	r.drain()

	// THE APPLIER RUNS, because a move with a subtree writes the root's
	// own subject twice — its move, and the mark coming down after the
	// walk — and the second waits for the first. See applyWhileWriting.
	r.applyWhileWriting()
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

// A WRITE DECIDED ON A PROJECT THE TASK HAS LEFT IS A CONFLICT, not a write.
//
// Every caller reads a task's project off the row BEFORE the write and decides
// on it who may act — a removal and a restore are that project lead's, and a
// re-route is that project's own — so the project it passes is the premise of
// the authority, not a label. A task moved in between is another project's to
// remove, restore or re-route, and the write used to land anyway under a scope
// naming the container the task had left. This moves the task between the
// caller's read (ENG) and its write, which is exactly that race.
func TestAWriteDecidedOnTheProjectATaskLeftIsAConflict(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if _, err := r.writer.WriteDocument(t.Context(), "op-ops",
		worklet.ProjectSubject("OPS"), "", worklet.Project{
			V: 1, Key: "OPS", Name: "Operations",
			CreatedAt: wednesday, UpdatedAt: wednesday,
		}, worklet.ChangeProjectCreated, nil); err != nil {
		t.Fatalf("seed the target project: %v", err)
	}
	r.drain()
	if _, err := r.writer.CreateTask(t.Context(), "op-create",
		newTask("t-moves"), nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r.drain()

	// THE CALLER READ IT IN ENG — and then it moved.
	read := oneTask(t, r, "t-moves").Project
	if read != "ENG" {
		t.Fatalf("the task was filed under %q, want ENG", read)
	}
	if _, err := r.writer.MoveTaskToProject(t.Context(), "op-move", "t-moves",
		"OPS", nil); err != nil {
		t.Fatalf("MoveTaskToProject: %v", err)
	}
	r.drain()

	conflict := func(verb string, err error) {
		t.Helper()
		switch {
		case err == nil:
			t.Errorf("%s decided on %s landed on a task now in OPS", verb, read)
		case !errors.Is(err, statelog.ErrConflict):
			t.Errorf("%s on a moved task = %v, want statelog.ErrConflict — "+
				"the caller must read again and decide again", verb, err)
		case !strings.Contains(err.Error(), "OPS"):
			t.Errorf("%s's conflict %q does not name the project the task is "+
				"in now, which is the one the caller must decide on", verb, err)
		}
	}

	_, err := r.writer.RemoveTask(t.Context(), "op-remove", "t-moves", read,
		false, nil)
	conflict("a removal", err)
	r.drain()
	if removed(t, r, "t-moves") {
		t.Fatal("the task is in the trash on the authority of a project it left")
	}

	title := "renamed on ENG's say-so"
	_, err = r.writer.UpdateTask(t.Context(), "op-edit", "t-moves", read,
		worklet.NoIfMatch, worklet.TaskPatch{Title: &title},
		worklet.ChangeFields, nil)
	conflict("an update", err)
	r.drain()
	if got := oneTask(t, r, "t-moves").Title; got == title {
		t.Error("the task took a patch decided on a project it left")
	}

	// AND THE RESTORE, which needs a task in the trash: removed on the
	// project it IS in, then restored on the one it was read in.
	if _, err := r.writer.RemoveTask(t.Context(), "op-remove-ops", "t-moves",
		"OPS", false, nil); err != nil {
		t.Fatalf("RemoveTask on the task's own project: %v", err)
	}
	r.drain()
	_, err = r.writer.RestoreTask(t.Context(), "op-restore", "t-moves", read, false, nil)
	conflict("a restore", err)
	r.drain()
	if !removed(t, r, "t-moves") {
		t.Error("the task left the trash on the authority of a project it left")
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
		filedTask(t, r, id)
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

// THE TRASH ANSWERS THE SAME AT EITHER SCOPE, THROUGH EVERY STATEMENT OVER IT.
//
// Every statement over a trash is read through an index that holds only
// removed rows ([taskSource]), named in the statement — so a statement whose
// predicate stopped implying that index's would fail rather than answer, and
// one reading another scope's index would answer another scope's trash. The
// plan test holds what each statement reads; this holds what it ANSWERS: the
// page, the count beside it, a total and a board's columns, at the workspace
// and inside the project, in the default subtask mode (where each row probes
// the removal it rides along with) and flat.
func TestTheTrashAnswersTheSameAtEitherScope(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for _, id := range []string{"kept", "first", "second"} {
		filedTask(t, r, id)
	}
	for i, id := range []string{"first", "second"} {
		r.at = wednesday.Add(time.Duration(i) * time.Minute)
		if _, err := r.writer.RemoveTask(t.Context(), "op-rm-"+id, id, "ENG",
			false, nil); err != nil {
			t.Fatalf("RemoveTask %s: %v", id, err)
		}
		r.drain()
	}
	r.at = wednesday

	want := []string{"second", "first"}
	for _, container := range []string{"workspace", "project:ENG"} {
		for _, mode := range []string{"collapsed", "separate"} {
			params := func(extra map[string]any) map[string]any {
				out := map[string]any{"container": container, "removed": "true",
					"show_closed": "true", "subtasks": mode}
				for key, value := range extra {
					out[key] = value
				}
				return out
			}
			what := container + " " + mode
			answer := r.ask(params(map[string]any{"totals": "tasks:count"}))
			if got := ids(answer); !slices.Equal(got, want) {
				t.Errorf("%s: the trash answers %v, want %v", what, got, want)
			}
			if answer.TotalHint != len(want) {
				t.Errorf("%s: the trash counts %d beside its page, want %d",
					what, answer.TotalHint, len(want))
			}
			if len(answer.Totals) != 1 || answer.Totals[0].Value == nil ||
				*answer.Totals[0].Value != float64(len(want)) {
				t.Errorf("%s: the trash totals %v, want a count of %d", what,
					answer.Totals, len(want))
			}
			board := r.ask(params(map[string]any{"group_by": "status"}))
			counted := 0
			for _, column := range board.Groups {
				counted += column.Count
			}
			if counted != len(want) {
				t.Errorf("%s: a board over the trash counts %d across its "+
					"columns, want %d", what, counted, len(want))
			}
		}
	}
}

// AND A READER MAY ASK FOR THE OTHER END OF IT.
//
// The default above is what an unsorted trash gets. `sort=removed` is the
// column asked for by name, and it was REFUSED by the parser for as long as
// the trash had no tab: the order existed, and nothing could name it.
//
// Both directions, because only one of them can be wrong at a time and each
// is wrong in a way that reads as working. A key the parser admits and
// [sortColumns] lacks is silently DROPPED — the answer comes back in the
// default order, which for this query is descending, so an ascending request
// that was thrown away is indistinguishable from one that was honoured.
func TestTheTrashCanBeAskedForTheOldestRemovalFirst(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for _, id := range []string{"first", "second", "third"} {
		filedTask(t, r, id)
	}
	for i, id := range []string{"second", "third", "first"} {
		r.at = wednesday.Add(time.Duration(i) * time.Minute)
		if _, err := r.writer.RemoveTask(t.Context(), "op-rm-"+id, id, "ENG",
			false, nil); err != nil {
			t.Fatalf("RemoveTask %s: %v", id, err)
		}
		r.drain()
	}
	r.at = wednesday

	for _, c := range []struct {
		sort string
		want []string
	}{
		{"removed", []string{"second", "third", "first"}},
		{"-removed", []string{"first", "third", "second"}},
	} {
		got := ids(r.ask(map[string]any{
			"container": "project:ENG", "removed": "true", "sort": c.sort,
		}))
		if !slices.Equal(got, c.want) {
			t.Errorf("sort=%s answers %v, want %v", c.sort, got, c.want)
		}
	}
}

// A REMOVAL OR A RESTORE NAMING ANOTHER PROJECT IS REFUSED.
//
// Its record is filed and probed under the project the caller named, so a
// removal naming the wrong one was written under a container the task is not
// in: a record deferred on the task's real project did not hold it back, and
// one deferred on the named project held back a record that never touched it.
// An edit already refused this; a removal and a restore did not.
func TestARemovalOrARestoreNamingAnotherProjectIsRefused(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if _, err := r.writer.CreateTask(t.Context(), "op-create", newTask("t-here"), nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r.drain()
	seedProject(t, r, worklet.Project{Key: "OPS", Name: "Operations"})

	_, err := r.writer.RemoveTask(t.Context(), "op-remove-ops", "t-here", "OPS", false, nil)
	if err == nil || !strings.Contains(err.Error(), "is in project ENG, not OPS") {
		t.Fatalf("a removal naming OPS for a task in ENG answered %v, want it refused "+
			"naming the project the task is in", err)
	}
	r.drain()
	if removed(t, r, "t-here") {
		t.Fatal("a removal refused for naming the wrong project removed the task")
	}

	if _, err := r.writer.RemoveTask(t.Context(), "op-remove", "t-here", "ENG", false, nil); err != nil {
		t.Fatalf("RemoveTask: %v", err)
	}
	r.drain()
	_, err = r.writer.RestoreTask(t.Context(), "op-restore-ops", "t-here", "OPS", false, nil)
	if err == nil || !strings.Contains(err.Error(), "is in project ENG, not OPS") {
		t.Fatalf("a restore naming OPS for a task in ENG answered %v, want it refused "+
			"naming the project the task is in", err)
	}
	r.drain()
	if !removed(t, r, "t-here") {
		t.Fatal("a restore refused for naming the wrong project restored the task")
	}
}

// THE TRASH LISTS EVERY REMOVAL UNDER THE TASK IT WAS MADE ON.
//
// What somebody restores is a removal — the task a gesture named, and what it
// took with it — so in the default subtask mode the trash lists each removal
// once, with what went with it riding along, and filters the removal rather
// than the tree. Three shapes, each one a removal of its own:
//
//   - a subtask removed alone while its parent stayed live, which is what an
//     author removing their own item does;
//   - a subtree removed together, an epic and the task under it;
//   - a subtask already in the trash when its root was removed with its
//     subtree, which that removal does not take and does not re-stamp.
//
// Anchored on the tree's root instead, the default trash asked whether the
// ROOT was removed, and the subtask removed alone under a live parent was in
// no default-mode trash at either scope.
func TestTheTrashListsEveryRemovalUnderItsOwnRoot(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	file := func(id, kind, parent string) {
		t.Helper()
		task := newTask(id)
		task.Key = "ENG-" + id
		task.Type = kind
		if parent != "" {
			task.Parent = &parent
			task.Depth = 1
		}
		if _, err := r.writer.CreateTask(t.Context(), "op-"+id, task, nil); err != nil {
			t.Fatalf("CreateTask %s: %v", id, err)
		}
		r.drain()
	}
	remove := func(id string, subtree bool) {
		t.Helper()
		if _, err := r.writer.RemoveTask(t.Context(), "op-rm-"+id, id, "ENG",
			subtree, nil); err != nil {
			t.Fatalf("RemoveTask %s: %v", id, err)
		}
		r.drain()
	}
	file("root-x", "task", "")
	file("sub-x", "task", "root-x")
	remove("sub-x", false)

	file("root-w", "epic", "")
	file("sub-w", "task", "root-w")
	remove("root-w", true)

	file("root-n", "epic", "")
	file("sub-n", "task", "root-n")
	remove("sub-n", false)
	remove("root-n", true)
	detail, err := r.reader.Task(t.Context(), "sub-n", worklet.DetailWants{},
		statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		t.Fatalf("read sub-n: %v", err)
	}
	if detail.Task.Removed == nil || detail.Task.Removed.RemovedWith != nil {
		t.Fatalf("sub-n's tombstone is %+v after its root's subtree removal, "+
			"want its own removal untouched", detail.Task.Removed)
	}

	every := []string{"root-w", "sub-w", "sub-x", "root-n", "sub-n"}
	for _, container := range []string{"project:ENG", "workspace"} {
		for _, mode := range []string{"collapsed", "expanded", "separate"} {
			ask := func(extra map[string]any) []string {
				params := map[string]any{"container": container,
					"removed": "true", "show_closed": "true", "subtasks": mode}
				for key, value := range extra {
					params[key] = value
				}
				return ids(r.ask(params))
			}
			what := container + " " + mode
			if got := ask(nil); !sameSet(got, every) {
				t.Errorf("%s: the trash lists %v, want every removed task %v",
					what, got, every)
			}
			// THE REMOVAL IS FILTERED AND WHAT IT TOOK RIDES ALONG, in a
			// rolled-up mode; flat, every task answers for itself.
			epics, tasks := []string{"root-w", "root-n", "sub-w"}, []string{"sub-x", "sub-n"}
			if mode == "separate" {
				epics, tasks = []string{"root-w", "root-n"}, []string{"sub-w", "sub-x", "sub-n"}
			}
			if got := ask(map[string]any{"type": "epic"}); !sameSet(got, epics) {
				t.Errorf("%s: the trash's epics are %v, want %v", what, got, epics)
			}
			if got := ask(map[string]any{"type": "task"}); !sameSet(got, tasks) {
				t.Errorf("%s: the trash's tasks are %v, want %v", what, got, tasks)
			}
		}
	}
}

// sameSet reports whether got holds exactly want, in any order.
func sameSet(got, want []string) bool {
	return len(got) == len(want) &&
		!slices.ContainsFunc(want, func(id string) bool { return !slices.Contains(got, id) })
}

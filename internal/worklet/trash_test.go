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

// THE TRASH IS REACHABLE FROM BOTH ENDS: a task can be removed (not only
// purged, which has no inverse), and the applier's restore path can run.
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

	// AND IT IS OUT OF EVERY ORDINARY LIST; only `removed=true` shows it.
	if got := ids(r.ask(map[string]any{"container": "project:ENG"})); len(got) != 0 {
		t.Errorf("the board still lists %v after the removal", got)
	}
	if got := ids(r.ask(map[string]any{
		"container": "project:ENG", "removed": "true",
	})); len(got) != 1 || got[0] != "t-gone" {
		t.Errorf("the trash lists %v, want the removed task", got)
	}

	// AND IT IS FROZEN: an ordinary patch is refused rather than editing
	// something nobody can see.
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

// A SUBTREE REMOVED TOGETHER COMES BACK TOGETHER, and brings back only what
// it took: by `removed_with`, a task already in the trash for its own reasons
// is not restored with its parent.
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

// A RESTORE ASKED FOR ALONE BRINGS BACK ONE TASK OR NOTHING: what went in
// with a task at somebody else's hand is not its filer's to bring back, so the
// restore is refused, writing nothing, when the removal took others, and lands
// when it took none. The writer decides over the read the walk is made from,
// so refusal and walk agree on what a restore would touch.
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

// REMOVING A REMOVED TASK IS NOTHING TO DO, AND IT SUCCEEDS, so a
// half-finished subtree removal can be re-run. A restore is the same for a
// root already back with tasks still in the trash behind it (what a stopped
// restore leaves) and for its own operation answered again; a NEW restore with
// nothing behind it is told so (TestARestoreWithNothingToBringBackSaysSo).
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

// A CROSS-PROJECT MOVE CARRIES THE SUBTREE: readSubtree, which a subtree
// removal walks too, reads every descendant.
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

	// THE APPLIER RUNS: a move with a subtree writes the root's subject twice
	// (its move, then the mark after the walk), and the second waits for the
	// first. See applyWhileWriting.
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
// A caller decides who may act from the project it read off the row (a
// removal and a restore are that project lead's, a re-route that project's),
// so the project it passes is the premise of the authority, not a label: a
// task moved since is another project's to act on. This moves the task between
// the caller's read (ENG) and its write.
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

	// AND THE RESTORE: removed on the project it IS in, then restored on the
	// one it was read in.
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

// THE TRASH IS ORDERED BY WHEN WORK WAS REMOVED, never by board rank: a
// removed task's rank is stale, and the only index over removed rows is the
// partial one on `removed_at`.
func TestTheTrashIsOrderedByRemoval(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for _, id := range []string{"first", "second", "third"} {
		filedTask(t, r, id)
	}
	// REMOVED OUT OF RANK ORDER, each at its own instant, so rank order and
	// removal order differ. The stamp is an AUTHORED instant (a displayed
	// one, §5), so moving the writer's clock is what separates them.
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
// Each statement names a removed-rows-only index ([taskSource]), so one reading
// another scope's index would answer another scope's trash. The plan test holds
// what each reads; this holds what it ANSWERS: page, count, total and board
// columns, at the workspace and in the project, rolled up and flat.
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

// AND A READER MAY ASK FOR THE OTHER END OF IT, by `sort=removed`, in both
// directions: a key the parser admits and [sortColumns] lacks is silently
// DROPPED to the default (descending) order, which a descending request alone
// could not tell from one honoured.
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

// A REMOVAL OR A RESTORE NAMING ANOTHER PROJECT IS REFUSED, as an edit is:
// its record is filed and probed under the named project, so a wrong one would
// slip past a record deferred on the task's real project and be held back by
// one deferred on a project it never touched.
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

// THE TRASH LISTS EVERY REMOVAL UNDER THE TASK IT WAS MADE ON: what somebody
// restores is a removal, so the default subtask mode lists each once, what it
// took riding along, and filters the removal rather than the tree (anchored on
// the root, a subtask removed alone under a live parent would be in no trash).
// Three removals:
//
//   - a subtask removed alone while its parent stayed live;
//   - a subtree removed together, an epic and the task under it;
//   - a subtask already in the trash when its root was removed with its
//     subtree, which that removal does not take and does not re-stamp.
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
			// THE REMOVAL IS FILTERED AND WHAT IT TOOK RIDES ALONG, rolled
			// up; flat, every task answers for itself.
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

// A TASK WHOSE ANCHOR IS ACROSS THE TRASH IS FILTERED ON ITS OWN: a row rides
// along with its anchor (its root, or in the trash its removal), and probed for
// the anchor's answer it would be in no default-mode listing. Two shapes:
//
//   - the residue of a restore that stopped after its root: a subtask still
//     in the trash whose tombstone names a root that is back;
//   - a parent removed alone over a live subtask.
//
// Each is the root of what is left, so it is listed, and filtered, on its own.
func TestATaskWhoseAnchorIsAcrossTheTrashIsFilteredOnItsOwn(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	file := func(id, kind, parent string, tombstone *worklet.Tombstone) {
		t.Helper()
		task := newTask(id)
		task.Key = "ENG-" + id
		task.Type = kind
		if parent != "" {
			task.Parent = &parent
			task.Depth = 1
		}
		task.Removed = tombstone
		if _, err := r.writer.CreateTask(t.Context(), "op-"+id, task, nil); err != nil {
			t.Fatalf("CreateTask %s: %v", id, err)
		}
		r.drain()
	}
	// THE RESIDUE, seeded with the tombstone a stopped restore leaves: what is
	// under test is the predicate that reads it.
	root := "root-y"
	file(root, "task", "", nil)
	file("sub-y", "bug", root, &worklet.Tombstone{
		By: "ana", Kind: worklet.AuthorHuman, At: wednesday, RemovedWith: &root,
	})
	// AND A PARENT REMOVED ALONE OVER A LIVE SUBTASK.
	file("root-z", "task", "", nil)
	file("sub-z", "bug", "root-z", nil)
	if _, err := r.writer.RemoveTask(t.Context(), "op-rm-root-z", "root-z", "ENG",
		false, nil); err != nil {
		t.Fatalf("RemoveTask root-z: %v", err)
	}
	r.drain()

	for _, container := range []string{"project:ENG", "workspace"} {
		for _, mode := range []string{"collapsed", "expanded"} {
			ask := func(extra map[string]any) []string {
				params := map[string]any{"container": container, "subtasks": mode}
				for key, value := range extra {
					params[key] = value
				}
				return ids(r.ask(params))
			}
			what := container + " " + mode
			if got := ask(map[string]any{"removed": "true"}); !sameSet(got, []string{"root-z", "sub-y"}) {
				t.Errorf("%s: the trash lists %v, want the removed parent and "+
					"the subtask its restore left behind", what, got)
			}
			if got := ask(map[string]any{"removed": "true", "type": "bug"}); !sameSet(got, []string{"sub-y"}) {
				t.Errorf("%s: the trash's bugs are %v, want the subtask filtered "+
					"on its own", what, got)
			}
			if got := ask(nil); !sameSet(got, []string{"root-y", "sub-z"}) {
				t.Errorf("%s: the board lists %v, want the restored root and the "+
					"subtask its parent's removal left", what, got)
			}
			if got := ask(map[string]any{"type": "bug"}); !sameSet(got, []string{"sub-z"}) {
				t.Errorf("%s: the board's bugs are %v, want the subtask filtered "+
					"on its own", what, got)
			}
		}
	}
}

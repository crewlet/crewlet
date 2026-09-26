package tracker_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// removed reports whether the reader still holds the task as removed.
func removed(t *testing.T, r *roundTrip, id string) bool {
	t.Helper()
	detail, err := r.reader.Task(t.Context(), id, tracker.DetailWants{},
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
		tracker.TaskPatch{Title: &title}, tracker.ChangeFields, nil); err == nil {
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
	// THE APPLIER RUNS, because a restore's descendants each read the parent
	// the commit before them restored, and wait for it. See
	// applyWhileWriting.
	r.applyWhileWriting()
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
		}, tracker.ChangeProjectCreated, nil); err != nil {
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

// subtreeOfThree is a root with two children, t-a and t-b, in that order under
// the (depth, id) order both trash walks read.
func subtreeOfThree(t *testing.T, r *roundTrip) {
	t.Helper()
	if _, err := r.writer.CreateTask(t.Context(), "op-t-root", newTask("t-root"),
		nil); err != nil {
		t.Fatalf("CreateTask t-root: %v", err)
	}
	r.drain()
	for _, id := range []string{"t-a", "t-b"} {
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
}

// A RESTORE RE-RUN AFTER A PARTIAL RESTORE BRINGS BACK EVERY TASK IT TOOK.
//
// A restore walks the tasks still in the trash with its root, so a re-run's
// list is shorter by every task the first run restored. Each step is named by
// the task it writes; a step named by its place in that list maps the re-run's
// first task onto the first run's first step, whose ledger row answers
// `applied` for a restore that never happened.
//
// Mutation: name the restore's steps by index and t-b stays in the trash after
// the re-run reports success.
func TestARestoreRerunBringsBackEveryTaskItTook(t *testing.T) {
	t.Parallel()
	broker := &refusingAppender{subject: tracker.TaskSubject("t-b").Wire()}
	r := newRoundTripAppending(t, func(a statelog.Appender) statelog.Appender {
		broker.Appender = a
		return broker
	})
	r.applyWhileWriting()
	subtreeOfThree(t, r)
	if _, err := r.writer.RemoveTask(t.Context(), "op-remove", "t-root", "ENG",
		true, nil); err != nil {
		t.Fatalf("RemoveTask: %v", err)
	}
	r.drain()

	broker.refusing(true)
	if _, err := r.writer.RestoreTask(t.Context(), "op-restore", "t-root", "ENG",
		nil); err == nil {
		t.Fatal("the first restore wrote the step the broker refused, so this " +
			"case is not the shape it names")
	}
	r.drain()
	broker.refusing(false)
	if removed(t, r, "t-a") || !removed(t, r, "t-b") {
		t.Fatal("the first restore did not stop between t-a and t-b, so this " +
			"case is not the shape it names")
	}

	if _, err := r.writer.RestoreTask(t.Context(), "op-restore", "t-root", "ENG",
		nil); err != nil {
		t.Fatalf("the re-run: %v", err)
	}
	r.drain()
	for _, id := range []string{"t-root", "t-a", "t-b"} {
		if removed(t, r, id) {
			t.Errorf("%s is still in the trash after a re-run that reported "+
				"success — its step was answered from another task's ledger row", id)
		}
	}
}

// silentAppender answers no append on one subject, and no probe of it either,
// while it is silent — which is how a write meets a broker that stopped
// answering in the middle of a gesture, and what makes its outcome `unknown`.
type silentAppender struct {
	statelog.Appender
	subject string

	mu     sync.Mutex
	silent bool
}

func (a *silentAppender) quiet(on bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.silent = on
}

func (a *silentAppender) silentOn(subject string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.silent && subject == a.subject
}

func (a *silentAppender) Append(ctx context.Context, subject, msgID string,
	expect *uint64, body []byte) (uint64, bool, error) {

	if a.silentOn(subject) {
		return 0, false, errors.New("no answer from the broker")
	}
	return a.Appender.Append(ctx, subject, msgID, expect, body)
}

func (a *silentAppender) LastSeq(ctx context.Context, subject string) (uint64, bool, error) {
	if a.silentOn(subject) {
		return 0, false, errors.New("no answer from the broker")
	}
	return a.Appender.LastSeq(ctx, subject)
}

// A TRASH WALK STOPS AT A STEP WHOSE OUTCOME IS UNKNOWN.
//
// `unknown` is not an error — the record may be on the log — so a walk that
// read only the error stepped past it and reported the gesture done, while
// the task it could not vouch for stayed where it was with nothing asking
// anybody to run the gesture again.
//
// Mutation: drop the unknown check from either walk and that walk's first
// call reports success with t-b where it started.
func TestATrashWalkStopsAtAnUnresolvedStep(t *testing.T) {
	t.Parallel()
	broker := &silentAppender{subject: tracker.TaskSubject("t-b").Wire()}
	r := newRoundTripAppending(t, func(a statelog.Appender) statelog.Appender {
		broker.Appender = a
		return broker
	})
	r.applyWhileWriting()
	subtreeOfThree(t, r)

	for _, walk := range []struct {
		verb string
		run  func() error
		done func() bool
	}{
		{"removal", func() error {
			_, err := r.writer.RemoveTask(t.Context(), "op-remove", "t-root",
				"ENG", true, nil)
			return err
		}, func() bool { return removed(t, r, "t-b") }},
		{"restore", func() error {
			_, err := r.writer.RestoreTask(t.Context(), "op-restore", "t-root",
				"ENG", nil)
			return err
		}, func() bool { return !removed(t, r, "t-b") }},
	} {
		broker.quiet(true)
		err := walk.run()
		r.drain()
		broker.quiet(false)
		if err == nil {
			t.Fatalf("a %s whose step for t-b went unanswered reported success "+
				"(t-b moved: %v)", walk.verb, walk.done())
		}
		if !strings.Contains(err.Error(), "t-b") ||
			!strings.Contains(err.Error(), "re-run the "+walk.verb) {
			t.Errorf("the %s's refusal does not name the unresolved task and the "+
				"re-run: %v", walk.verb, err)
		}

		if err := walk.run(); err != nil {
			t.Fatalf("the %s's re-run: %v", walk.verb, err)
		}
		r.drain()
		if !walk.done() {
			t.Errorf("t-b has not followed the %s after its re-run", walk.verb)
		}
	}
}

// A MOVE STOPS AT A DESCENDANT WHOSE OUTCOME IS UNKNOWN, and says so.
//
// Nothing completes a split subtree on its own and a re-issued move is
// refused once the root has moved, so the move's answer is the only place
// the split is ever reported. A walk that read only the error stepped past an
// unresolved descendant and answered with the root's new key, as though the
// whole subtree had followed.
//
// Mutation: drop the unknown arm from the move's walk and the move reports
// success with m-kid still in ENG.
func TestAMoveStopsAtAnUnresolvedDescendant(t *testing.T) {
	t.Parallel()
	broker := &silentAppender{subject: tracker.TaskSubject("m-kid").Wire()}
	r := newRoundTripAppending(t, func(a statelog.Appender) statelog.Appender {
		broker.Appender = a
		return broker
	})
	r.applyWhileWriting()
	if _, err := r.writer.WriteDocument(t.Context(), "op-ops",
		tracker.ProjectSubject("OPS"), "", tracker.Project{
			V: 1, Key: "OPS", Name: "Operations",
			CreatedAt: wednesday, UpdatedAt: wednesday,
		}, tracker.ChangeProjectCreated, nil); err != nil {
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

	broker.quiet(true)
	_, err := r.writer.MoveTaskToProject(t.Context(), "op-move", "m-root", "OPS", nil)
	r.drain()
	broker.quiet(false)
	if err == nil {
		t.Fatalf("a move whose step for m-kid went unanswered reported success "+
			"(m-kid is in %s)", oneTask(t, r, "m-kid").Project)
	}
	if !strings.Contains(err.Error(), "m-kid is unresolved") {
		t.Errorf("the refusal does not name the unresolved descendant: %v", err)
	}
}

// NOTHING IS PLACED UNDER A PARENT IN THE TRASH.
//
// A live child under a removed parent is the orphan a subtree removal is
// ordered to avoid: on every list, under a parent none of them shows. A
// create naming such a parent, and a re-parent onto one, are both refused
// with the remedy — restore it first — and succeed once it is restored.
//
// Mutation: drop the removed-parent refusal from either path and that write
// lands a live task under t-gone.
func TestNothingIsPlacedUnderAParentInTheTrash(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for _, id := range []string{"t-gone", "t-live"} {
		task := newTask(id)
		task.Key = "ENG-" + id
		if _, err := r.writer.CreateTask(t.Context(), "op-"+id, task, nil); err != nil {
			t.Fatalf("CreateTask %s: %v", id, err)
		}
		r.drain()
	}
	if _, err := r.writer.RemoveTask(t.Context(), "op-remove", "t-gone", "ENG",
		false, nil); err != nil {
		t.Fatalf("RemoveTask: %v", err)
	}
	r.drain()

	parent := "t-gone"
	kid := newTask("t-kid")
	kid.Key = "ENG-t-kid"
	kid.Parent, kid.Depth = &parent, 1
	if _, err := r.writer.CreateTask(t.Context(), "op-kid", kid, nil); err == nil ||
		!strings.Contains(err.Error(), "restore it") {
		t.Fatalf("a create under a parent in the trash = %v, want it refused "+
			"naming the restore", err)
	}
	if _, err := r.writer.UpdateTask(t.Context(), "op-reparent", "t-live", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Parent: &parent},
		tracker.ChangeFields, nil); err == nil ||
		!strings.Contains(err.Error(), "restore it") {
		t.Fatalf("a re-parent onto a parent in the trash = %v, want it refused "+
			"naming the restore", err)
	}
	r.drain()
	if got := oneTask(t, r, "t-live"); got.Parent != nil {
		t.Fatalf("t-live is under %s after a refused re-parent", *got.Parent)
	}

	if _, err := r.writer.RestoreTask(t.Context(), "op-restore", "t-gone", "ENG",
		nil); err != nil {
		t.Fatalf("RestoreTask: %v", err)
	}
	r.drain()
	if _, err := r.writer.CreateTask(t.Context(), "op-kid-again", kid, nil); err != nil {
		t.Fatalf("a create under the restored parent: %v", err)
	}
}

// NOTHING IS RESTORED UNDER A PARENT STILL IN THE TRASH.
//
// A restore is the third way to put a live task under a removed parent — the
// orphan a create and a re-parent are both refused — so each restore reads its
// task's parent in the snapshot it is decided from. A task restored on its own
// while the removal that took it still holds its parent is refused, naming
// that parent, with nothing written; once the parent is back it is restored.
//
// Mutation: drop the parent check from clearTombstone and t-a comes back
// under t-root while t-root is still in the trash.
func TestNothingIsRestoredUnderAParentInTheTrash(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.applyWhileWriting()
	subtreeOfThree(t, r)
	if _, err := r.writer.RemoveTask(t.Context(), "op-remove", "t-root", "ENG",
		true, nil); err != nil {
		t.Fatalf("RemoveTask: %v", err)
	}
	r.drain()
	before := r.consumed

	_, err := r.writer.RestoreTask(t.Context(), "op-restore-a", "t-a", "ENG", nil)
	if err == nil || !strings.Contains(err.Error(), "restore t-root first") {
		t.Fatalf("a restore under a parent in the trash = %v, want it refused "+
			"naming the parent to restore first", err)
	}
	r.drain()
	if r.consumed != before {
		t.Errorf("the refused restore appended %d record(s)", r.consumed-before)
	}
	if !removed(t, r, "t-a") {
		t.Fatal("t-a is out of the trash under a parent that is still in it")
	}

	if _, err := r.writer.RestoreTask(t.Context(), "op-restore-root", "t-root",
		"ENG", nil); err != nil {
		t.Fatalf("restore the parent: %v", err)
	}
	r.drain()
	for _, id := range []string{"t-root", "t-a", "t-b"} {
		if removed(t, r, id) {
			t.Errorf("%s is still in the trash after its root was restored", id)
		}
	}
}

// A RESTORE BRINGS BACK WHAT IT CAN AND NAMES WHAT IT CANNOT.
//
// A task removed on its own leaves its children live beneath it; a later
// subtree removal above it takes those children, while the task itself was
// already in the trash on its own account. Restoring that subtree then meets a
// child whose parent is still in the trash: it stays there, and so does
// everything below it, while every other task the removal took comes back —
// its siblings are no less restorable for it. The answer is a partial one that
// a re-run alone does not finish, naming the parent: restore it, run the
// restore again, and the rest follows.
//
// Mutation: stop the walk at the first refusal and t-d, which sorts after
// t-c, stays in the trash; drop the parent check and t-c comes back under
// t-p while t-p is in the trash.
func TestARestoreBringsBackWhatItCanAndNamesWhatItCannot(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.applyWhileWriting()
	file := func(id, parent string, depth int) {
		t.Helper()
		task := newTask(id)
		task.Key = "ENG-" + id
		if parent != "" {
			task.Parent, task.Depth = &parent, depth
		}
		if _, err := r.writer.CreateTask(t.Context(), "op-"+id, task, nil); err != nil {
			t.Fatalf("CreateTask %s: %v", id, err)
		}
		r.drain()
	}
	// t-root ─┬─ t-p ── t-c
	//         └─ t-q ── t-d
	file("t-root", "", 0)
	file("t-p", "t-root", 1)
	file("t-q", "t-root", 1)
	file("t-c", "t-p", 2)
	file("t-d", "t-q", 2)
	if _, err := r.writer.RemoveTask(t.Context(), "op-remove-p", "t-p", "ENG",
		false, nil); err != nil {
		t.Fatalf("remove t-p on its own: %v", err)
	}
	r.drain()
	if _, err := r.writer.RemoveTask(t.Context(), "op-remove-root", "t-root",
		"ENG", true, nil); err != nil {
		t.Fatalf("remove t-root's subtree: %v", err)
	}
	r.drain()

	_, err := r.writer.RestoreTask(t.Context(), "op-restore", "t-root", "ENG", nil)
	var stopped *tracker.PartialError
	if !errors.As(err, &stopped) || stopped.Rerun ||
		!strings.Contains(err.Error(), "restore t-p first") {
		t.Fatalf("a restore meeting a child under a parent in the trash = %v, "+
			"want a partial answer that a re-run alone does not finish, naming "+
			"t-p", err)
	}
	r.drain()
	for id, want := range map[string]bool{
		"t-root": false, "t-q": false, "t-d": false, "t-p": true, "t-c": true,
	} {
		if got := removed(t, r, id); got != want {
			t.Errorf("%s reads removed=%v after the restore, want %v", id, got, want)
		}
	}

	if _, err := r.writer.RestoreTask(t.Context(), "op-restore-p", "t-p", "ENG",
		nil); err != nil {
		t.Fatalf("restore t-p: %v", err)
	}
	r.drain()
	if _, err := r.writer.RestoreTask(t.Context(), "op-restore", "t-root", "ENG",
		nil); err != nil {
		t.Fatalf("the restore run again: %v", err)
	}
	r.drain()
	if removed(t, r, "t-c") {
		t.Error("t-c is still in the trash after its parent was restored and " +
			"the restore run again")
	}
}

// AN UNRESOLVED ROOT STOPS A TRASH WALK BEFORE ANY DESCENDANT IS TOUCHED.
//
// The root goes first so that no child is ever removed under a live parent,
// or restored under a removed one. When the root's own commit is `unknown` —
// its record may be on the log and may not — the gesture answers that outcome
// as it is, for its caller to retry under the same op id, and writes nothing
// below it: a walk that went on would decide every descendant against a root
// it cannot vouch for.
//
// Mutation: drop the unknown check after the root from either walk, and the
// removal takes t-a and t-b with the root unresolved, or the restore answers
// an error rather than the unknown outcome.
func TestAnUnresolvedRootStopsATrashWalkBeforeItsDescendants(t *testing.T) {
	t.Parallel()
	broker := &silentAppender{subject: tracker.TaskSubject("t-root").Wire()}
	r := newRoundTripAppending(t, func(a statelog.Appender) statelog.Appender {
		broker.Appender = a
		return broker
	})
	r.applyWhileWriting()
	subtreeOfThree(t, r)

	broker.quiet(true)
	got, err := r.writer.RemoveTask(t.Context(), "op-remove", "t-root", "ENG",
		true, nil)
	broker.quiet(false)
	r.drain()
	if err != nil || got.Outcome != statelog.OutcomeUnknown {
		t.Fatalf("a removal whose root went unanswered = %+v, %v; want the "+
			"unknown outcome and no error", got, err)
	}
	for _, id := range []string{"t-a", "t-b"} {
		if removed(t, r, id) {
			t.Errorf("%s was removed although its root's removal is unresolved", id)
		}
	}

	if _, err := r.writer.RemoveTask(t.Context(), "op-remove", "t-root", "ENG",
		true, nil); err != nil {
		t.Fatalf("the removal's retry: %v", err)
	}
	r.drain()
	broker.quiet(true)
	got, err = r.writer.RestoreTask(t.Context(), "op-restore", "t-root", "ENG", nil)
	broker.quiet(false)
	r.drain()
	if err != nil || got.Outcome != statelog.OutcomeUnknown {
		t.Fatalf("a restore whose root went unanswered = %+v, %v; want the "+
			"unknown outcome and no error", got, err)
	}
	for _, id := range []string{"t-a", "t-b"} {
		if !removed(t, r, id) {
			t.Errorf("%s was restored although its root's restore is unresolved", id)
		}
	}
}

// A TASK MID-MERGE IS NOT PUT IN THE TRASH UNTIL ITS MERGE HAS ENDED.
//
// Every step a merge still owes its duplicate is a patch on it — the close, or
// the give-up that clears the marker — and a task in the trash refuses every
// patch. A removal landing mid-merge therefore left a marker nothing could
// clear: the merge's caller failed, and the tracker duty met the same refusal
// on every sweep. The removal is refused instead, and lands once the merge has
// ended.
//
// Mutation: drop the merging refusal from the tombstone and dup goes in the
// trash still marked mid-merge.
func TestATaskMidMergeIsNotPutInTheTrash(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "keep")
	filedTask(t, r, "dup")
	merging := true
	if _, err := r.writer.UpdateTask(t.Context(), "op-mark", "dup", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{
			Relate: &tracker.RelationIntent{Add: []tracker.Relation{{
				Kind: tracker.RelationDuplicates, Other: "keep",
			}}},
			Merging: &merging,
		}, tracker.ChangeRelations, nil); err != nil {
		t.Fatalf("mark dup mid-merge: %v", err)
	}
	r.drain()

	if _, err := r.writer.RemoveTask(t.Context(), "op-remove", "dup", "ENG",
		false, nil); err == nil || !strings.Contains(err.Error(), "being merged") {
		t.Fatalf("a removal of a task mid-merge = %v, want it refused until the "+
			"merge has finished", err)
	}
	r.drain()
	if removed(t, r, "dup") {
		t.Fatal("a task mid-merge went in the trash, where nothing can clear its marker")
	}

	done := false
	if _, err := r.writer.UpdateTask(t.Context(), "op-end", "dup", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Merging: &done},
		tracker.ChangeFields, nil); err != nil {
		t.Fatalf("end the merge: %v", err)
	}
	r.drain()
	if _, err := r.writer.RemoveTask(t.Context(), "op-remove-after", "dup", "ENG",
		false, nil); err != nil {
		t.Fatalf("a removal once the merge had ended: %v", err)
	}
	r.drain()
	if !removed(t, r, "dup") {
		t.Error("dup is not in the trash after its merge ended and it was removed")
	}
}

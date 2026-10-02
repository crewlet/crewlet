package tracker_test

import (
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// restoreCounter puts a project's key counter back to zero, which is what a
// counter restored beside newer work looks like to every write after it: the
// next create mints a number a task already holds.
func (r *roundTrip) restoreCounter(project string) {
	r.t.Helper()
	if err := r.db.Replicated().Tx(r.t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(r.t.Context(),
			`UPDATE tracker_counters SET last = 0 WHERE project_key = ?`, project)
		return err
	}); err != nil {
		r.t.Fatalf("restore %s's counter: %v", project, err)
	}
}

// wokenBy is the wake the newest record on the log hands one recipient: the
// record off the log, through the change feed's translator and the parser,
// exactly as the node that wins its delivery reads it.
func (r *roundTrip) wokenBy(handle string, parties *notify.Registry) notify.Inbound {
	r.t.Helper()
	last, err := r.log.End(r.t.Context())
	if err != nil {
		r.t.Fatalf("read the log's end: %v", err)
	}
	_, payload, _, ok, err := r.log.At(r.t.Context(), last)
	if err != nil || !ok {
		r.t.Fatalf("read record %d: %v", last, err)
	}
	delivery, wakes, err := tracker.NewTranslator().Translate(r.t.Context(),
		changefeed.Record{Payload: r.open(last, payload)})
	if err != nil || !wakes {
		r.t.Fatalf("translate record %d: wakes=%v, %v", last, wakes, err)
	}
	routed, err := tracker.NewParser(tracker.ParserOptions{}).Parse(r.t.Context(),
		types.RawWebhook{Body: delivery.Body}, parties)
	if err != nil {
		r.t.Fatalf("parse record %d: %v", last, err)
	}
	for _, wake := range routed {
		if wake.To.Handle == handle {
			return wake.Inbound
		}
	}
	r.t.Fatalf("record %d reached %+v and not %s", last, routed, handle)
	return notify.Inbound{}
}

// A WAKE ABOUT THE DUPLICATE OF A KEY SENDS THE SEAT TO ITS ID.
//
// A counter restored beside newer work mints numbers tasks already hold, and
// every read resolves such a key to the task that claimed it first. A seat
// woken about the duplicate was told its KEY — in the subject, under
// **Task:**, and in the "read this first" block — so it read the claimant,
// answered on the claimant, and reassigned the claimant. The flag that tells
// the prompt to say otherwise is derived by the WRITER, in the decide's own
// snapshot, for every record that carries a wake; and the create's own
// receipt says it too, because a create after a restore is what makes the
// duplicate in the first place.
//
// THROUGH A REAL WRITER, THE LOG, THE TRANSLATOR AND THE PARSER, because the
// flag is what has to travel: a case asserting the writer's struct would pass
// for a field that never reached the wire.
//
// Mutation: derive nothing in [tracker.Writer]'s decide and the duplicate's
// comment wake names ENG-1; drop the create's own reading and its receipt
// claims ENG-1 is its own; render the context block from the key and the
// prompt sends the seat to the claimant.
func TestAWakeAboutAKeysDuplicateNamesItByItsID(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	parties := registry(t, "ana", "cy")
	file := func(id string) tracker.WriteResult {
		t.Helper()
		task := newTask(id)
		task.Assignee = "cy"
		got, err := r.writer.CreateTask(t.Context(), "op-"+id, task,
			tracker.Wake{Kind: tracker.ChangeCreated, After: task}.Notify(nil))
		if err != nil {
			t.Fatalf("file %s: %v", id, err)
		}
		r.drain()
		return got
	}
	claimant := file("t-1")
	if wake := r.lastWake(); wake == nil || wake.Snapshot.KeyCollision {
		t.Fatalf("the claimant's own create carries wake %+v — the key is its own", wake)
	}
	r.restoreCounter("ENG")
	duplicate := file("t-2")
	if claimant.Key != "ENG-1" || duplicate.Key != "ENG-1" {
		t.Fatalf("the two creates took %q and %q, want ENG-1 twice — the case "+
			"needs a key two tasks hold", claimant.Key, duplicate.Key)
	}
	if claimant.KeyCollision {
		t.Error("the claimant's receipt says its key opens another task")
	}
	if !duplicate.KeyCollision {
		t.Error("the duplicate's receipt says ENG-1 is its own — the caller acts " +
			"on the claimant with its next call")
	}
	if wake := r.lastWake(); wake == nil || !wake.Snapshot.KeyCollision {
		t.Errorf("the duplicate's create carries wake %+v, which does not say "+
			"ENG-1 opens another task", wake)
	}
	if detail := r.task(t, "t-2"); !detail.KeyCollision || detail.Address() != "t-2" {
		t.Fatalf("the applier left t-2 flagged=%v addressed %q — the case is "+
			"about a task the rows say is a duplicate", detail.KeyCollision,
			detail.Address())
	}

	comment := func(id string) string {
		t.Helper()
		before := r.task(t, id).Task
		note := &tracker.Comment{ID: "c-" + id, Task: id, Author: "ana",
			AuthorKind: tracker.AuthorHuman, Body: "is this the one?",
			CreatedAt: wednesday}
		if _, err := r.writer.UpdateTask(t.Context(), "op-c-"+id, id, "ENG",
			tracker.NoIfMatch, tracker.TaskPatch{Comment: note},
			tracker.ChangeComment, tracker.Wake{
				Kind: tracker.ChangeComment, Before: before, After: before,
				Comment: note,
			}.Notify(nil)); err != nil {
			t.Fatalf("comment on %s: %v", id, err)
		}
		r.drain()
		woken := r.wokenBy("cy", parties)
		return tracker.Prompt{}.Build(woken, parties)
	}

	dup := comment("t-2")
	for _, want := range []string{
		"**Task:** t-2 comment", "**Key:** ENG-1", "Read **t-2** with `get_work_item`",
	} {
		if !strings.Contains(dup, want) {
			t.Errorf("the duplicate's wake does not say %q:\n%s", want, dup)
		}
	}
	if strings.Contains(dup, "Read **ENG-1**") {
		t.Errorf("the duplicate's wake sends the seat to ENG-1, which opens the "+
			"claimant:\n%s", dup)
	}
	own := comment("t-1")
	if !strings.Contains(own, "Read **ENG-1** with `get_work_item`") ||
		strings.Contains(own, "**Key:**") {
		t.Errorf("the claimant's wake does not name it by its own key, or "+
			"explains a collision it does not have:\n%s", own)
	}
}

// A PRIORITY LIST TOPPED BY A DUPLICATE, AND A PURGE OF ONE, NAME IT BY ITS ID.
//
// Both wakes carry an EXCERPT naming the task — the line a seat is handed as
// "what changed", and the line a lead reads on the card — and both named the
// key: a seat told to take up ENG-1 took up the claimant, and a lead told
// "ENG-1 was purged" opened ENG-1 and found it still there.
//
// Mutation: name the task by its key in either excerpt, or render the
// priorities prompt's pointer from the key, and this fails.
func TestAnExcerptNamesAKeysDuplicateByItsID(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for _, id := range []string{"t-1", "t-2"} {
		if _, err := r.writer.CreateTask(t.Context(), "op-"+id, newTask(id), nil); err != nil {
			t.Fatalf("file %s: %v", id, err)
		}
		r.drain()
		r.restoreCounter("ENG")
	}
	if !r.task(t, "t-2").KeyCollision {
		t.Fatal("t-2 is not a duplicate of ENG-1, so this case shows nothing")
	}

	named := "t-2 (its key ENG-1 opens another task)"
	lead := r.writer.As("bob", tracker.AuthorHuman, tracker.Provenance{})
	if _, err := lead.WritePriorities(t.Context(), "op-prio", "cy", []string{"t-2"},
		tracker.PersonAuthority{Authorized: true}); err != nil {
		t.Fatalf("write cy's priorities: %v", err)
	}
	r.drain()
	if wake := r.lastWake(); wake == nil || !strings.Contains(wake.Excerpt, named) {
		t.Errorf("the priorities wake carries %+v, want an excerpt naming %q",
			wake, named)
	}
	prompt := tracker.Prompt{}.Build(r.wokenBy("cy", registry(t, "bob", "cy")), nil)
	for _, want := range []string{
		"**Top of your list:** t-2", "**Key:** ENG-1", "Read **t-2** with `get_work_item`",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the priorities wake does not say %q:\n%s", want, prompt)
		}
	}

	r.writer.Leads = fixedLeads{project: "eng-lead"}
	operator := r.writer.As("ops-1", tracker.AuthorOperator, tracker.Provenance{})
	if _, err := operator.PurgeTask(t.Context(), "op-purge", "t-2", "ENG",
		"a restored duplicate"); err != nil {
		t.Fatalf("purge t-2: %v", err)
	}
	r.drain()
	if got := historyExcerpt(t, r, "task", "t-2"); !strings.HasPrefix(got,
		named+" was purged") {
		t.Errorf("the purge is recorded as %q, want it to name %q — ENG-1 "+
			"still opens the claimant, which nobody purged", got, named)
	}
}

// A MOVE THAT LANDS A TASK ON A KEY ANOTHER TASK HOLDS SAYS SO ON ITS RECEIPT.
//
// A move mints its new key from the target's counter exactly as a create
// does, so a counter restored there hands the moved task a number another
// task already holds — and the receipt is the first thing that names it. So
// does the receipt of the same move asked again, which is answered from the
// root as it stands in the target.
//
// Mutation: drop the reading from the move's root step, or from the re-run's
// read of the root, and the receipt claims OPS-1 is the moved task's own.
func TestAMoveOntoAKeyAnotherTaskHoldsSaysSo(t *testing.T) {
	t.Parallel()
	r := moveFixture(t)
	held := newTask("o-1")
	held.Project = "OPS"
	if _, err := r.writer.CreateTask(t.Context(), "op-o-1", held, nil); err != nil {
		t.Fatalf("file OPS-1: %v", err)
	}
	r.drain()
	r.restoreCounter("OPS")

	op := statelog.NewOpID(wednesday, "move")
	moved, err := r.writer.MoveTaskToProject(t.Context(), op, "m-root", "OPS", nil)
	if err != nil {
		t.Fatalf("the move: %v", err)
	}
	r.drain()
	if moved.Key != "OPS-1" || !moved.KeyCollision {
		t.Errorf("the move answers key %q collision=%v, want OPS-1 flagged — "+
			"o-1 claimed OPS-1 first, so the key opens o-1", moved.Key,
			moved.KeyCollision)
	}
	if detail := r.task(t, "m-root"); !detail.KeyCollision {
		t.Fatal("the applier did not flag the moved root, so the receipt and " +
			"the rows disagree about which task OPS-1 is")
	}
	again, err := r.writer.MoveTaskToProject(t.Context(), op, "m-root", "OPS", nil)
	if err != nil {
		t.Fatalf("the same move again: %v", err)
	}
	if again.Key != "OPS-1" || !again.KeyCollision {
		t.Errorf("the move asked again answers key %q collision=%v, want OPS-1 "+
			"flagged", again.Key, again.KeyCollision)
	}
}

// A STOPPED MOVE NAMES THE TASK IT WAITS FOR AS A RESTORE WOULD OPEN IT.
//
// The task a move waits for is in the trash, and what the stop hands its
// caller is the name to restore it by. A task holding a key another task
// claimed first is reached only by its id, so a stop that named the key told
// the caller to restore the claimant — here the moving root itself.
//
// Mutation: name the waiting task by its key and this fails.
func TestAStoppedMoveNamesTheTaskItWaitsForByItsAddress(t *testing.T) {
	t.Parallel()
	r := lateTagFixture(t)
	// THE TASK FILED BEHIND THE WALK TAKES ENG-1, which the root claimed.
	r.restoreCounter("ENG")
	lossy, log := r.lossyWriter(t)
	fileBehind(t, r, log, "m-late", "m-kid-a", func() {
		r.drain()
		if _, err := r.writer.RemoveTask(t.Context(), "op-remove-late", "m-late",
			"ENG", false, nil); err != nil {
			t.Errorf("put the task in the trash: %v", err)
		}
	})
	_, err := lossy.MoveTaskToProject(t.Context(),
		statelog.NewOpID(wednesday, "move"), "m-root", "OPS", nil)
	r.drain()
	var stopped *tracker.MoveStopped
	if !errors.As(err, &stopped) {
		t.Fatalf("the move answered %v, want a *tracker.MoveStopped", err)
	}
	if late := r.task(t, "m-late"); !late.KeyCollision {
		t.Fatalf("m-late holds %s unflagged, so this case shows nothing",
			late.Task.Key)
	}
	if stopped.Waiting != "m-late" {
		t.Errorf("the stop waits for %q, want m-late — its key opens the "+
			"root, so a restore of the key restores nothing it meant",
			stopped.Waiting)
	}
}

// A DUPLICATE OF A KEY CAN BE MOVED TO ANOTHER PROJECT, which is how it gets a
// key of its own.
//
// A cross-project move claims the key it leaves as an alias, so a key pasted
// into chat keeps opening the task after the move. The claim refused when the
// directory named another task for that key — and for a duplicate it always
// does: the key was never the duplicate's address, it opened the claimant. So
// the move refused every duplicate, the one task that most needs a new key,
// with "key ENG-1 belongs to task m-root". There is nothing for such a move to
// keep resolving: the key goes on opening the claimant, as it always did.
//
// Mutation: refuse the alias over a key another task holds and the move fails.
func TestADuplicateOfAKeyCanBeMovedToAnotherProject(t *testing.T) {
	t.Parallel()
	r := moveFixture(t)
	r.restoreCounter("ENG")
	if _, err := r.writer.CreateTask(t.Context(), "op-dup", newTask("m-dup"), nil); err != nil {
		t.Fatalf("file the duplicate: %v", err)
	}
	r.drain()
	if !r.task(t, "m-dup").KeyCollision {
		t.Fatal("m-dup is not a duplicate of ENG-1, so this case shows nothing")
	}
	moved, err := r.writer.MoveTaskToProject(t.Context(),
		statelog.NewOpID(wednesday, "move"), "m-dup", "OPS", nil)
	if err != nil {
		t.Fatalf("moving the duplicate of ENG-1 to OPS: %v", err)
	}
	r.drain()
	dup := r.task(t, "m-dup")
	if dup.Task.Project != "OPS" || dup.Task.Key != moved.Key || dup.KeyCollision ||
		moved.KeyCollision {
		t.Errorf("the duplicate is %s in %s flagged=%v (receipt %s flagged=%v), "+
			"want a key of its own in OPS", dup.Task.Key, dup.Task.Project,
			dup.KeyCollision, moved.Key, moved.KeyCollision)
	}
	if got := r.task(t, "ENG-1"); got.Task.ID != "m-root" {
		t.Errorf("ENG-1 opens %s after the duplicate moved, want m-root — the "+
			"key it left was never the duplicate's", got.Task.ID)
	}
}

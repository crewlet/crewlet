package tracker_test

import (
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/tracker"
)

// A TURN'S SPEND LANDS ON ITS TASK, ONCE, AND EVERY READER SEES IT.
//
// The eight `spend_*` columns were written by the applier from turn records
// that nothing published, so every task reported it had cost nothing — and the
// detail read took its spend from the task's DOCUMENT, which no turn ever
// writes, so it would have gone on saying so after the columns moved. What
// this holds is the whole path: the write, the apply, the columns, the detail,
// and the operation id that makes a repeat count nothing.
func TestATurnsSpendLandsOnItsTaskOnce(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	task := r.createTask("Ship the release")

	first := tracker.TurnRecord{
		Task: task.ID, Seat: "swe", TurnID: "run-1",
		Trigger: "external_notification", Outcome: "delivered",
		Phases: []string{"execute", "review"},
		Spend: tracker.TurnSpend{
			Turns: 1, Rounds: 2, Input: 1000, Output: 200,
			CacheRead: 600, CacheWrite: 100, WallMs: 4200,
		},
	}
	if _, err := r.writer.RecordTurn(t.Context(), "op-turn-1", first); err != nil {
		t.Fatalf("record a turn: %v", err)
	}
	r.drain()
	want := tracker.Spend{Turns: 1, Rounds: 2, Input: 1000, Output: 200,
		CacheRead: 600, CacheWrite: 100, WallMs: 4200, Tokens: 1200}
	if got := r.task(t, task.ID).Task.Spend; got != want {
		t.Fatalf("after one turn the task reports %+v, want %+v", got, want)
	}

	// THE SAME OPERATION AGAIN — a retry whose first copy landed — counts
	// nothing.
	if _, err := r.writer.RecordTurn(t.Context(), "op-turn-1", first); err != nil {
		t.Fatalf("repeat the turn's write: %v", err)
	}
	r.drain()
	if got := r.task(t, task.ID).Task.Spend; got != want {
		t.Fatalf("a repeated operation moved the spend to %+v, want %+v", got, want)
	}

	// A SECOND TURN adds, and the resumed half of a turn adds its rounds
	// and tokens without counting the turn twice.
	second := first
	second.TurnID, second.Spend = "run-2", tracker.TurnSpend{
		Turns: 0, Rounds: 1, Input: 300, Output: 50, WallMs: 800,
	}
	if _, err := r.writer.RecordTurn(t.Context(), "op-turn-2", second); err != nil {
		t.Fatalf("record the second half: %v", err)
	}
	r.drain()
	want = tracker.Spend{Turns: 1, Rounds: 3, Input: 1300, Output: 250,
		CacheRead: 600, CacheWrite: 100, WallMs: 5000, Tokens: 1550}
	if got := r.task(t, task.ID).Task.Spend; got != want {
		t.Fatalf("after a resumed half the task reports %+v, want %+v", got, want)
	}
	if got := r.strings(`SELECT spend_tokens FROM tracker_tasks WHERE id = ?`, task.ID); len(got) != 1 || got[0] != "1550" {
		t.Fatalf("the sort column reads %v, want 1550", got)
	}
}

// A TURN ON A TASK THAT IS NOT THERE IS REFUSED BY THE WRITER, AND ONE THAT
// LANDS AFTER ITS TASK'S PURGE APPLIES NOWHERE.
//
// The applier adds a turn's spend to the task's row and stops the log when
// there is no row, because under a strict replay that is a writer's bug. Two
// writers make it anyway: a turn records its spend when it ENDS, after
// whatever it did — its own task's purge included. So the writer refuses a task
// it cannot see, and the deletion gate reads a turn as the record about a task
// it is: without that, the turn published before the purge applied and landed
// after it would stop every node's log.
func TestATurnOnAPurgedTaskAppliesNowhere(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	task := r.createTask("Short lived")

	// THE PURGE IS PUBLISHED AND NOT YET APPLIED, so the turn's own
	// decision still sees the task and publishes after it.
	if _, err := r.writer.PurgeTask(t.Context(), "op-purge", task.ID, task.Project,
		"filed by mistake"); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if _, err := r.writer.RecordTurn(t.Context(), "op-late-turn", tracker.TurnRecord{
		Task: task.ID, Seat: "swe", TurnID: "run-late",
		Spend: tracker.TurnSpend{Turns: 1, Rounds: 1, Input: 10, Output: 1},
	}); err != nil {
		t.Fatalf("record a turn racing the purge: %v", err)
	}
	r.drain()
	if got := r.strings(`SELECT id FROM tracker_turns WHERE task_id = ?`, task.ID); len(got) != 0 {
		t.Fatalf("a turn that landed after its task's purge wrote %v", got)
	}

	// AND ONE DECIDED AFTER THE PURGE APPLIED is refused, as final.
	_, err := r.writer.RecordTurn(t.Context(), "op-after", tracker.TurnRecord{
		Task: task.ID, Seat: "swe", TurnID: "run-after",
		Spend: tracker.TurnSpend{Turns: 1},
	})
	if !errors.Is(err, tracker.ErrNoTask) {
		t.Fatalf("a turn on a purged task answered %v, want ErrNoTask", err)
	}
}

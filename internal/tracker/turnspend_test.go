package tracker_test

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
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

// A TURN THAT LANDS AFTER ITS TASK'S PURGE IS REFUSED `deleted`, ON THE NODE
// THAT WROTE IT, WHEN THAT NODE APPLIES WHILE IT WAITS.
//
// The applier drops the turn under the task's deletion marker and writes no
// ledger row, so the write's resolution asks the gate reader why — and a
// reader that covered only the task's own subject answered "nothing gates
// it" about the turn's, which the resolution can only read as a ledger that
// lost a row it vouched for: the writer was told the store broke a contract,
// and its refusal was counted under `error` rather than `deleted`. The two
// sides of the gate read one rule ([tracker.Applier.Gated] and
// [tracker.Gates.GatedAt] over the same subjects), and this is the case the
// other turn test cannot reach, because there nothing applies until the
// write has already answered `pending`.
func TestATurnLandingAfterItsTasksPurgeIsRefusedDeleted(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	task := r.createTask("Short lived")

	// THE PURGE IS PUBLISHED AND NOT YET APPLIED, so the turn's own
	// decision still sees the task and appends above the purge; from here
	// the applier runs inside the write's own wait, as a live node's does.
	if _, err := r.writer.PurgeTask(t.Context(), "op-purge", task.ID, task.Project,
		"filed by mistake"); err != nil {
		t.Fatalf("purge: %v", err)
	}
	r.applyWhileWriting()
	_, err := r.writer.RecordTurn(t.Context(), "op-late-turn", tracker.TurnRecord{
		Task: task.ID, Seat: "swe", TurnID: "run-late",
		Spend: tracker.TurnSpend{Turns: 1, Rounds: 1, Input: 10, Output: 1},
	})
	var refused *statelog.Unavailable
	switch {
	case !errors.As(err, &refused):
		t.Fatalf("a turn landing after its task's purge answered %v, want a "+
			"refusal naming the gate that dropped it", err)
	case refused.Reason != statelog.ReasonDeleted:
		t.Fatalf("a turn landing after its task's purge was refused %q, want %q",
			refused.Reason, statelog.ReasonDeleted)
	case refused.OpID != "op-late-turn":
		t.Fatalf("the refusal names the operation %q, want op-late-turn", refused.OpID)
	}
	if got := r.strings(`SELECT id FROM tracker_turns WHERE task_id = ?`, task.ID); len(got) != 0 {
		t.Fatalf("a turn that landed after its task's purge wrote %v", got)
	}
}

// A TURN ON A TASK NO RECORD EVER CREATED IS REFUSED AS FINAL; ONE ON A TASK
// THIS NODE HAS NOT APPLIED YET IS NOT.
//
// "No row here" is two facts, and a caller holding its work until a spend is
// settled acts on them oppositely: a create this node has not applied yet is
// one a retry — or a moment's wait — finds, and a create that no node will ever
// apply (an abandoned generation voids one and writes no marker, and a task id
// is never minted twice) is a retry for ever. Both answered "not on this node",
// so a coding run on such a task held its seat indefinitely. The log's END is
// what tells them apart: read there, a task still absent — with no record this
// node could not decode that might be its create — is gone for good.
func TestATurnOnATaskTheLogNeverCreatedIsFinal(t *testing.T) {
	t.Parallel()
	a := newRoundTrip(t)
	a.applyWhileWriting()
	turn := func(task string) tracker.TurnRecord {
		return tracker.TurnRecord{Task: task, Seat: "swe", TurnID: "run-" + task,
			Spend: tracker.TurnSpend{Input: 40, Output: 2}}
	}

	// NEVER CREATED: the log's end holds no create for it.
	_, err := a.writer.RecordTurn(t.Context(), "op-nowhere", turn(uuid.NewString()))
	if !errors.Is(err, tracker.ErrNoTask) || errors.Is(err, statelog.ErrUnavailable) {
		t.Fatalf("a turn on a task no record creates answered %v, want the final "+
			"ErrNoTask", err)
	}

	// NOT APPLIED YET: a second node over the same log, behind a create the
	// first filed. Its turn waits for the log's end, finds the task there and
	// lands.
	dbNode, db := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node-b.db"), store.Options{}, 1)
	t.Cleanup(func() { _ = dbNode.Close() })
	b := newRoundTripOn(t, a.broker, a.log, dbNode, db, "node-b")
	filed := a.createTask("Filed on node a")
	b.applyWhileWriting()
	if _, err := b.writer.RecordTurn(t.Context(), "op-behind", turn(filed.ID)); err != nil {
		t.Fatalf("a turn on a task the log creates, on a node that had not applied "+
			"it yet, was refused: %v", err)
	}
	b.drain()
	if got := b.task(t, filed.ID).Task.Spend.Tokens; got != 42 {
		t.Fatalf("the turn landed on node b as %d tokens, want 42", got)
	}

	// A RECORD THIS NODE CANNOT DECODE may be the create it is missing, so
	// the absence is "not yet", which a retry after an upgrade answers.
	a.deferRecordOn(uuid.NewString(), "ENG")
	_, err = a.writer.RecordTurn(t.Context(), "op-undecoded", turn(uuid.NewString()))
	if !errors.Is(err, statelog.ErrUnavailable) || errors.Is(err, tracker.ErrNoTask) {
		t.Fatalf("a turn on a missing task beside a record this node cannot decode "+
			"answered %v, want the retryable ErrUnavailable", err)
	}
}

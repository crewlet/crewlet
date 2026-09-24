package tracker_test

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// Spend is on the task: a turn charged to a work item adds to that item's own
// counters, through the one additive write, and nothing about the race with a
// purge can stop the applier.

// taskSpend reads a task's spend counters and its reopen count.
func (r *roundTrip) taskSpend(id string) map[string]int64 {
	r.t.Helper()
	columns := append(append([]string{}, tracker.SpendColumns...), "reopens")
	out := map[string]int64{}
	if err := r.db.Replicated().Read(r.t.Context(), func(tx *sql.Tx) error {
		values := make([]int64, len(columns))
		targets := make([]any, len(columns))
		for i := range values {
			targets[i] = &values[i]
		}
		if err := tx.QueryRowContext(r.t.Context(),
			`SELECT `+strings.Join(columns, ", ")+` FROM tracker_tasks WHERE id = ?`,
			id).Scan(targets...); err != nil {
			return err
		}
		for i, column := range columns {
			out[column] = values[i]
		}
		return nil
	}); err != nil {
		r.t.Fatalf("read the spend of %s: %v", id, err)
	}
	return out
}

func sampleTurn(task string) tracker.TurnRecord {
	return tracker.TurnRecord{
		Task: task, Seat: "dev", TurnID: "run-1", Trigger: "work_item",
		Outcome: "done", Phases: []string{"execute", "review"},
		Spend: tracker.TurnSpend{
			Turns: 1, Rounds: 3, Input: 1000, Output: 400, CacheRead: 600,
			CacheWrite: 50, WallMs: 9000, Workers: 2, SentBack: 1,
		},
	}
}

// THE WRITER AND THE APPLIER SHARE ONE TURN SHAPE.
//
// They were two anonymous structs spelling the same keys, and nothing wrote
// the record at all — so a key the writer grew and the applier never read
// would have gone unnoticed for exactly as long as the counters stayed zero.
// Every field goes in through RecordTurn and must come out on the rows.
func TestTheWriterAndApplierShareOneTurnShape(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if _, err := r.writer.CreateTask(t.Context(), "op-1", newTask("t-1"), nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r.drain()

	turn := sampleTurn("t-1")
	if _, err := r.writer.RecordTurn(t.Context(), "turn/run-1/dispatch", turn); err != nil {
		t.Fatalf("RecordTurn: %v", err)
	}
	r.drain()

	got := r.taskSpend("t-1")
	for column, want := range map[string]int64{
		"spend_turns": 1, "spend_rounds": 3, "spend_input": 1000,
		"spend_output": 400, "spend_cache_read": 600, "spend_cache_write": 50,
		"spend_wall_ms": 9000, "spend_tokens": 1400, "spend_workers": 2,
		"spend_sent_back": 1,
	} {
		if got[column] != want {
			t.Errorf("%s = %d after one turn, want %d", column, got[column], want)
		}
	}

	var seat, turnID, trigger, outcome, phases string
	if err := r.db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), `
			SELECT seat, turn_id, trigger, outcome, phases_json
			FROM tracker_turns WHERE id = ?`, "turn/run-1/dispatch").
			Scan(&seat, &turnID, &trigger, &outcome, &phases)
	}); err != nil {
		t.Fatalf("read the turn row: %v", err)
	}
	if seat != "dev" || turnID != "run-1" || trigger != "work_item" ||
		outcome != "done" || phases != `["execute","review"]` {
		t.Errorf("the turn row reads seat=%q turn=%q trigger=%q outcome=%q "+
			"phases=%s — every field the writer states is one the row must hold",
			seat, turnID, trigger, outcome, phases)
	}
}

// A REDELIVERED TURN COUNTS ONCE.
//
// The op id is the turn row's own id, so a segment recorded twice — a retried
// resume, a lost acknowledgement — and a record the broker delivers twice all
// land on one row, and the counters move with the row's insert.
func TestARedeliveredTurnCountsOnce(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if _, err := r.writer.CreateTask(t.Context(), "op-1", newTask("t-1"), nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r.drain()

	first, err := r.writer.RecordTurn(t.Context(), "turn/run-1/dispatch", sampleTurn("t-1"))
	if err != nil {
		t.Fatalf("RecordTurn: %v", err)
	}
	r.drain()
	if _, err := r.writer.RecordTurn(t.Context(), "turn/run-1/dispatch", sampleTurn("t-1")); err != nil {
		t.Fatalf("RecordTurn again: %v", err)
	}
	r.drain()
	r.redeliver(first.Position.Seq)

	if got := r.taskSpend("t-1"); got["spend_turns"] != 1 || got["spend_input"] != 1000 ||
		got["spend_workers"] != 2 {
		t.Fatalf("one segment recorded twice and delivered three times reads %v — "+
			"the insert of the turn's own row is what gates the addition", got)
	}
}

// A TURN IS REFUSED ON A TASK THE WRITER CANNOT SEE, AND CHARGED TO A REMOVED
// ONE.
//
// A purge destroys the rows a charge adds to and its marker is permanent; a
// task this node does not hold has nothing to add to. A tombstone destroys
// nothing — the work was done on the task whether or not somebody filed it
// away since — so a removed task is still charged.
func TestRecordTurnRefusesATaskItCannotSee(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	if _, err := r.writer.RecordTurn(t.Context(), "turn/nowhere", sampleTurn("never")); err == nil {
		t.Error("a turn on a task that never existed was accepted")
	}

	for _, id := range []string{"purged", "removed"} {
		if _, err := r.writer.CreateTask(t.Context(), "op-"+id, newTask(id), nil); err != nil {
			t.Fatalf("CreateTask %s: %v", id, err)
		}
		r.drain()
	}
	if _, err := r.writer.PurgeTask(t.Context(), "op-purge", "purged", "ENG", ""); err != nil {
		t.Fatalf("PurgeTask: %v", err)
	}
	if _, err := r.writer.RemoveTask(t.Context(), "op-remove", "removed", "ENG", false, nil); err != nil {
		t.Fatalf("RemoveTask: %v", err)
	}
	r.drain()

	_, err := r.writer.RecordTurn(t.Context(), "turn/purged", sampleTurn("purged"))
	if err == nil || !strings.Contains(err.Error(), "purged") {
		t.Errorf("a turn on a purged task answered %v, want a refusal naming the purge", err)
	}
	if _, err := r.writer.RecordTurn(t.Context(), "turn/removed", sampleTurn("removed")); err != nil {
		t.Fatalf("a turn on a tombstoned task was refused: %v — a removal hides "+
			"a task and destroys nothing", err)
	}
	r.drain()
	if got := r.taskSpend("removed"); got["spend_turns"] != 1 {
		t.Errorf("the tombstoned task's turns read %d, want the turn charged", got["spend_turns"])
	}
}

// A TURN ON A PURGED TASK IS READ PAST, NOT FATAL.
//
// A turn is additive: nothing at the broker orders it against a purge of the
// same task, so a turn decided while the task existed can land after the purge
// that removed it. Its spend update then finds no row — and every node applies
// the same log in the same order, so an applier that stopped there stopped the
// whole fleet. The deletion gate is what reads it past.
func TestATurnOnAPurgedTaskIsReadPastNotFatal(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if _, err := r.writer.CreateTask(t.Context(), "op-1", newTask("t-1"), nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r.drain()

	// THE RACE, exactly: the purge is on the log but not yet applied here,
	// so the turn's decide still sees the task — and its record lands on
	// the log AFTER the purge.
	if _, err := r.writer.PurgeTask(t.Context(), "op-purge", "t-1", "ENG", ""); err != nil {
		t.Fatalf("PurgeTask: %v", err)
	}
	turn, err := r.writer.RecordTurn(t.Context(), "turn/late", sampleTurn("t-1"))
	if err != nil {
		t.Fatalf("RecordTurn: %v", err)
	}
	r.drain() // Fails the test if the applier stops on the turn.

	reason, gated, err := tracker.NewGates(r.db).GatedAt(t.Context(),
		statelog.Subject{Kind: string(tracker.KindTurn), ID: "t-1"}, "node-a",
		"turn/late", turn.Position)
	if err != nil {
		t.Fatalf("GatedAt: %v", err)
	}
	if !gated || reason != statelog.ReasonDeleted {
		t.Errorf("a turn on a purged task is gated=%v as %q, want it read past "+
			"as deleted", gated, reason)
	}
	if got := r.count(`SELECT COUNT(*) FROM tracker_turns WHERE task_id = 't-1'`); got != 0 {
		t.Errorf("the purged task has %d turn rows — the gate is what keeps a late "+
			"charge off a destroyed task", got)
	}
}

// count reads one integer.
func (r *roundTrip) count(query string, args ...any) int {
	r.t.Helper()
	var n int
	if err := r.db.Replicated().Read(r.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(r.t.Context(), query, args...).Scan(&n)
	}); err != nil {
		r.t.Fatalf("%s: %v", query, err)
	}
	return n
}

// A REOPEN IS A LEAVE FROM A FINISHED GROUP, AND NOTHING ELSE.
//
// done → in progress is a reopen; todo → in progress never finished; done →
// closed stayed finished. A counter that moved on every status change would be
// a count of status changes under a name that promises something narrower.
func TestAReopenIsCountedOnlyWhenFinishedWorkIsUnfinished(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if _, err := r.writer.CreateTask(t.Context(), "op-1", newTask("t-1"), nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r.drain()

	for i, step := range []struct {
		to   tracker.Status
		want int64
	}{
		{tracker.StatusInProgress, 0}, // todo → in progress: never finished
		{tracker.StatusDone, 0},
		{tracker.StatusClosed, 0},     // done → closed: still finished
		{tracker.StatusInProgress, 1}, // closed → in progress: a reopen
		{tracker.StatusDone, 1},
		{tracker.StatusTodo, 2}, // done → todo: a reopen
		{tracker.StatusInReview, 2},
	} {
		to := step.to
		if _, err := r.writer.UpdateTask(t.Context(), "op-status-"+itoa(i), "t-1", "ENG",
			tracker.NoIfMatch, tracker.TaskPatch{Status: &to},
			tracker.ChangeStatus, nil); err != nil {
			t.Fatalf("move to %s: %v", to, err)
		}
		r.drain()
		if got := r.taskSpend("t-1")["reopens"]; got != step.want {
			t.Fatalf("after moving to %s the task reads %d reopens, want %d",
				to, got, step.want)
		}
	}
}

// THE BACKFILL EQUALS THE REPLAY.
//
// A build that adds a derived column meets rows written without it, and fills
// them by re-deriving from the history rows. The column is only fleet-identical
// if what the re-derivation computes is exactly what the incremental rule
// would have reached by applying the same records — so rows zeroed as a
// predecessor left them must come back to the value the replay wrote.
func TestTheReopenBackfillEqualsAReplay(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	moves := map[string][]tracker.Status{
		"t-1": {tracker.StatusDone, tracker.StatusInProgress, tracker.StatusDone,
			tracker.StatusTodo},
		"t-2": {tracker.StatusInProgress, tracker.StatusDone, tracker.StatusClosed},
		"t-3": {tracker.StatusCancelled, tracker.StatusInReview},
	}
	for id := range moves {
		if _, err := r.writer.CreateTask(t.Context(), "op-"+id, newTask(id), nil); err != nil {
			t.Fatalf("CreateTask %s: %v", id, err)
		}
		r.drain()
	}
	for id, statuses := range moves {
		for i, status := range statuses {
			to := status
			if _, err := r.writer.UpdateTask(t.Context(), "op-"+id+"-"+itoa(i), id, "ENG",
				tracker.NoIfMatch, tracker.TaskPatch{Status: &to},
				tracker.ChangeStatus, nil); err != nil {
				t.Fatalf("move %s to %s: %v", id, to, err)
			}
			r.drain()
		}
	}
	replayed := map[string]int64{}
	for id := range moves {
		replayed[id] = r.taskSpend(id)["reopens"]
	}
	if replayed["t-1"] != 2 || replayed["t-2"] != 0 || replayed["t-3"] != 1 {
		t.Fatalf("the replay counted %v, want t-1=2 t-2=0 t-3=1", replayed)
	}

	// THE PREDECESSOR'S ROWS: the column as a build without it left it.
	if err := r.db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(t.Context(), `UPDATE tracker_tasks SET reopens = 0`); err != nil {
			return err
		}
		_, err := r.applier.Rederive(t.Context(), tx, statelog.ApplyOptions{})
		return err
	}); err != nil {
		t.Fatalf("re-derive: %v", err)
	}
	for id, want := range replayed {
		if got := r.taskSpend(id)["reopens"]; got != want {
			t.Errorf("%s re-derives to %d reopens and the replay counted %d — a "+
				"node upgrading onto old rows would disagree with one that applied "+
				"them", id, got, want)
		}
	}
	if tracker.DerivationVersion < 1 {
		t.Error("the applier derives a column and claims no derivation rules")
	}
}

// MEDIAN AND P90 ARE A VALUE THE SET HOLDS, AND ABSENT OVER NOTHING.
//
// Nearest rank: the value at ⌈p·n⌉ of the sorted set. The median of an even
// set is its lower middle value, never an average no task has.
func TestMedianAndP90AreOrderStatisticsOfTheWholeSet(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for i, points := range []float64{8, 1, 5, 3, 2, 13, 21, 34, 55, 89, 144} {
		task := newTask("odd-" + itoa(i))
		task.Points = points
		if _, err := r.writer.CreateTask(t.Context(), "op-odd-"+itoa(i), task, nil); err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
		r.drain()
	}

	// 1 2 3 5 8 13 21 34 55 89 144: n=11, the median at 6 and p90 at 10.
	odd := r.ask(map[string]any{"container": "project:ENG",
		"totals": "points:median,points:p90"})
	for key, want := range map[string]float64{"points:median": 13, "points:p90": 89} {
		if got := totalOf(t, odd, key); got.Value == nil || *got.Value != want {
			t.Errorf("odd %s = %v, want %v", key, got.Value, want)
		}
	}

	even := r.ask(map[string]any{
		"container": "project:ENG", "key": "ENG-1,ENG-2,ENG-3,ENG-4,ENG-5,ENG-6",
		"totals": "points:median,points:p90",
	})
	// Keys are minted in create order: 8 1 5 3 2 13 → sorted 1 2 3 5 8 13.
	for key, want := range map[string]float64{"points:median": 3, "points:p90": 13} {
		got := totalOf(t, even, key)
		if got.Value == nil || *got.Value != want {
			t.Errorf("even %s = %v, want %v — nearest rank takes the value at "+
				"⌈p·n⌉, never an average of two", key, got.Value, want)
		}
	}

	empty := r.ask(map[string]any{
		"container": "project:ENG", "key": "ENG-999",
		"totals": "points:median,points:p90,created_at:median",
	})
	for _, key := range []string{"points:median", "points:p90", "created_at:median"} {
		if got := totalOf(t, empty, key); got.Value != nil || got.At != nil {
			t.Errorf("%s over an empty set answered %v/%v, want it absent — "+
				"nothing is not zero", key, got.Value, got.At)
		}
	}

	dates := r.ask(map[string]any{"container": "project:ENG", "totals": "created_at:median"})
	if got := totalOf(t, dates, "created_at:median"); got.At == nil || got.Value != nil {
		t.Errorf("a median over a date column answered %v/%v, want an instant", got.Value, got.At)
	}
	if err := r.askErr(map[string]any{"container": "project:ENG", "totals": "created_at:avg"}); err == nil {
		t.Error("an average of dates was answered — a number of microseconds nobody meant")
	}
}

package worklet_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
	"github.com/crewlet/crewlet/internal/worklet"
)

// applyHarness is one node's replicated estate with the tracker's applier over
// it.
type applyHarness struct {
	t       *testing.T
	db      store.ReplicatedHandle
	applier *worklet.Applier
	seq     uint64

	// maxVariables is what every collection's insert chunks to. It starts
	// at the estate's own probed limit, which is what the framework hands
	// a real apply; a case that wants a CHUNK BOUNDARY it can count sets
	// its own, because the probed limit and the 1 000-row cap put every
	// collection the caps permit inside one statement.
	maxVariables int
}

func newApplyHarness(t *testing.T) *applyHarness {
	t.Helper()
	dbNode, db := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 1)
	t.Cleanup(func() {
		if err := dbNode.Close(); err != nil {
			t.Errorf("close the store: %v", err)
		}
	})
	return &applyHarness{
		t: t, db: db, applier: worklet.NewApplier(nil),
		maxVariables: db.Caps().MaxVariables,
	}
}

// apply runs one record through the applier at the next position.
func (h *applyHarness) apply(rec worklet.MutationRecord, brokerAt time.Time) (int, error) {
	h.t.Helper()
	h.seq++
	return h.applyAt(rec, brokerAt, h.seq)
}

// applyAt runs one record at a POSITION THE CALLER CHOOSES.
//
// The cases that need it are the ones the ordinary path cannot reach: a record
// arriving BELOW rows already applied is what a reprocess after an upgrade and
// a redelivery after an adoption both look like, and both are exactly where a
// derived value that folded over arrival order would diverge.
func (h *applyHarness) applyAt(rec worklet.MutationRecord, brokerAt time.Time,
	seq uint64) (int, error) {
	h.t.Helper()
	body, err := json.Marshal(rec)
	if err != nil {
		h.t.Fatalf("encode the record: %v", err)
	}
	position := statelog.Position{
		Stream: "CREWLET_TRACKER_LOG", Generation: rec.Gen, Seq: seq,
	}
	var rows int
	err = h.db.Tx(h.t.Context(), func(tx *sql.Tx) error {
		record := statelog.Record{
			Envelope: statelog.Envelope{
				V: rec.V, Kind: string(rec.Subject.Kind),
				Subject: statelog.Subject{
					Kind: string(rec.Subject.Kind), ID: rec.Subject.ID,
				},
				Op: string(rec.Op), OpID: rec.OpID, Gen: rec.Gen,
				Writer: rec.Writer,
			},
			Position: position,
			Payload:  body,
			StoredAt: brokerAt,
		}
		reason, gated, err := h.applier.Gated(h.t.Context(), tx, record)
		if err != nil {
			return err
		}
		if gated {
			return &gateError{reason}
		}
		// THE REAL PROBED LIMIT, not a left-out zero. MaxVariables is
		// what every collection's insert chunks to, and a harness that
		// passed nothing would exercise only the one-row-per-statement
		// degradation — which is the shape the applier was CONVERTED
		// AWAY FROM, so the suite would certify the path production
		// does not take.
		n, err := h.applier.Apply(h.t.Context(), tx, record, statelog.ApplyOptions{
			Now: brokerAt, StoredAt: brokerAt,
			MaxVariables: h.maxVariables,
		})
		rows = n
		return err
	})
	return rows, err
}

type gateError struct{ reason statelog.Reason }

func (e *gateError) Error() string { return "gated: " + string(e.reason) }

// count reads one table's row count.
func (h *applyHarness) count(table string) int {
	h.t.Helper()
	var n int
	if err := h.db.Read(h.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(h.t.Context(),
			`SELECT COUNT(*) FROM `+table).Scan(&n)
	}); err != nil {
		h.t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// value reads one scalar.
func (h *applyHarness) value(query string, args ...any) int64 {
	h.t.Helper()
	var n sql.NullInt64
	if err := h.db.Read(h.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(h.t.Context(), query, args...).Scan(&n)
	}); err != nil {
		h.t.Fatalf("read %q: %v", query, err)
	}
	return n.Int64
}

// taskRecord hand-builds a task record in the shape the writer gives one: its
// change kind stated ([worklet.MutationRecord.Kind]), and agreeing with its
// wake's where it has one.
func taskRecord(id string, op worklet.OpKind, payload any, notify *worklet.Notify) worklet.MutationRecord {
	body, _ := json.Marshal(payload)
	kind := map[worklet.OpKind]worklet.ChangeKind{
		worklet.OpCreate: worklet.ChangeCreated, worklet.OpPatch: worklet.ChangeFields,
		worklet.OpTombstone: worklet.ChangeRemoved, worklet.OpRestore: worklet.ChangeRestored,
		worklet.OpPurge: worklet.ChangePurged,
	}[op]
	if notify != nil {
		kind = notify.Kind
	}
	return worklet.MutationRecord{
		Kind: kind,
		RecordEnvelope: worklet.RecordEnvelope{
			V: worklet.RecordVersion, OpID: id + "-" + string(op),
			Subject: worklet.TaskSubject(id), Op: op,
			CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
			Writer:    "node-a", Scope: worklet.ScopeSet{Subject: true, Container: "ENG"},
		},
		Mutation: body, Actor: "ana", ActorKind: worklet.AuthorHuman,
		Notify: notify,
	}
}

// filedTask files one plain task into ENG under its own id.
//
// THE SEEDER THE READ-SIDE SUITES SHARE, so a case about an activity feed, a
// blocker or a merge states only what it is about rather than repeating a
// create.
func filedTask(t *testing.T, r *roundTrip, id string) {
	t.Helper()
	task := newTask(id)
	task.Key = "ENG-" + id
	if _, err := r.writer.CreateTask(t.Context(), "op-"+id, task, nil); err != nil {
		t.Fatalf("CreateTask %s: %v", id, err)
	}
	r.drain()
}

// itoa is strconv.Itoa for a non-negative int, spelled out so a case can
// compose an id or read a cap back out of a refusal without importing strconv
// into every file that needs one.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var out []byte
	for n > 0 {
		out = append([]byte{byte('0' + n%10)}, out...)
		n /= 10
	}
	return string(out)
}

func newTask(id string) worklet.Task {
	at := time.Unix(1_700_000_000, 0).UTC()
	return worklet.Task{
		V: worklet.DocumentVersion, ID: id, Key: "ENG-1", Project: "ENG",
		Type: "task", Title: "a task", Status: worklet.StatusTodo,
		StatusGroup: worklet.GroupNotStarted, Priority: worklet.PriorityNormal,
		Rank: worklet.RankOrigin, CreatedAt: at, UpdatedAt: at,
	}
}

// A TURN'S SPEND IS GATED ON ITS OWN INSERT, so a redelivery cannot
// double-count.
//
// The total is a FUNCTION of the applied records rather than a separately
// transmitted number, which is what makes it impossible for it to disagree
// with the turns it summarises.
func TestATurnsSpendCannotBeCountedTwice(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	if _, err := h.apply(taskRecord("t-1", worklet.OpCreate, newTask("t-1"), nil),
		time.Unix(1_700_000_100, 0).UTC()); err != nil {
		t.Fatalf("create: %v", err)
	}

	turn := worklet.MutationRecord{
		RecordEnvelope: worklet.RecordEnvelope{
			V: worklet.RecordVersion, OpID: "turn-1",
			Subject: worklet.TurnSubject("t-1"), Op: worklet.OpTurn,
			CreatedAt: time.Unix(1_700_000_200, 0).UTC(),
			Writer:    "node-a", Scope: worklet.ScopeSet{Subject: true, Container: "ENG"},
		},
		Mutation: mustJSON(map[string]any{
			"task": "t-1",
			"spend": map[string]int{
				"turns": 1, "rounds": 4, "input": 1000, "output": 500,
			},
		}),
	}
	if _, err := h.apply(turn, time.Unix(1_700_000_200, 0).UTC()); err != nil {
		t.Fatalf("turn: %v", err)
	}
	if got := h.value(`SELECT spend_input FROM tracker_tasks WHERE id = 't-1'`); got != 1000 {
		t.Fatalf("after one turn the input spend is %d, want 1000", got)
	}
	// THE SAME TURN AGAIN, at a higher position — which is what a
	// redelivery the ledger no longer collapses, because its retention
	// sweep removed the row, looks like.
	if _, err := h.apply(turn, time.Unix(1_700_000_200, 0).UTC()); err != nil {
		t.Fatalf("redelivered turn: %v", err)
	}
	if got := h.value(`SELECT spend_input FROM tracker_tasks WHERE id = 't-1'`); got != 1000 {
		t.Fatalf("a redelivered turn moved the input spend to %d — the insert is "+
			"what gates the addition, so a second delivery must add nothing", got)
	}
	if got := h.count("tracker_turns"); got != 1 {
		t.Errorf("the turn table holds %d rows for one turn", got)
	}
}

// A TURN NAMING A TASK THIS NODE DOES NOT HAVE STOPS THE LOOP.
//
// Under a strict replay the create is below this position, so an absent task
// is a writer's bug rather than a race — and going on would leave a running
// total nobody can reconcile against the turns it was built from.
func TestATurnForAnAbsentTaskStopsTheLoop(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	turn := worklet.MutationRecord{
		RecordEnvelope: worklet.RecordEnvelope{
			V: worklet.RecordVersion, OpID: "turn-1",
			Subject: worklet.TurnSubject("missing"), Op: worklet.OpTurn,
			Writer: "node-a", Scope: worklet.ScopeSet{Subject: true, Container: "ENG"},
		},
		Mutation: mustJSON(map[string]any{
			"task": "missing", "spend": map[string]int{"turns": 1},
		}),
	}
	if _, err := h.apply(turn, time.Unix(1_700_000_200, 0).UTC()); err == nil {
		t.Fatal("a turn naming a task this node does not have was applied")
	}
}

// THE EVICTION GATE DROPS A RECORD BY ITS POSITION, NOT BY ITS AUTHOR.
//
// It depends on nothing but the log's own order, which is what makes it the
// fence that still holds when coordination cannot be reached at all — and why
// a record written BEFORE the eviction is applied normally.
func TestTheEvictionGateDropsOnlyWhatFollowsIt(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	early := taskRecord("t-1", worklet.OpCreate, newTask("t-1"), nil)
	early.Writer = "node-b"
	if _, err := h.apply(early, time.Unix(1_700_000_100, 0).UTC()); err != nil {
		t.Fatalf("a record from before the eviction was refused: %v", err)
	}

	eviction := worklet.MutationRecord{
		RecordEnvelope: worklet.RecordEnvelope{
			V: worklet.GateRecordVersion, OpID: "evict-1",
			Subject: worklet.EvictionSubject("node-b"), Op: worklet.OpEviction,
			Writer: "node-a", Scope: worklet.ScopeSet{Subject: true},
		},
		Mutation: mustJSON(worklet.Eviction{
			V: 1, NodeID: "node-b", EvictedBy: "ops",
			EvictedAt: time.Unix(1_700_000_150, 0).UTC(),
		}),
	}
	if _, err := h.apply(eviction, time.Unix(1_700_000_150, 0).UTC()); err != nil {
		t.Fatalf("eviction: %v", err)
	}

	// A RECORD FROM BELOW THE EVICTION, applied after it — which is what a
	// redelivery looks like once the gate is in place, and the case that
	// tells a gate keyed on the POSITION from one keyed on the author.
	redelivered := taskRecord("t-3", worklet.OpCreate, newTask("t-3"), nil)
	redelivered.Writer = "node-b"
	redelivered.OpID = "t-3-redelivered"
	if _, err := h.applyAt(redelivered, time.Unix(1_700_000_100, 0).UTC(), 1); err != nil {
		t.Fatalf("a record from before the eviction was gated after it: %v", err)
	}

	late := taskRecord("t-2", worklet.OpCreate, newTask("t-2"), nil)
	late.Writer = "node-b"
	_, err := h.apply(late, time.Unix(1_700_000_200, 0).UTC())
	var gate *gateError
	if !asGate(err, &gate) {
		t.Fatalf("a record written after the eviction was applied (%v)", err)
	}
	if gate.reason != statelog.ReasonEvicted {
		t.Fatalf("the gate reason is %q", gate.reason)
	}
	if got := h.count("tracker_tasks"); got != 2 {
		t.Errorf("the estate holds %d tasks; the two from below the eviction "+
			"apply and the one above it does not", got)
	}
}

func asGate(err error, target **gateError) bool {
	return errors.As(err, target)
}

// A PURGE'S MARKER IS PERMANENT, AND EVERY LATER RECORD ABOUT THE TASK IS
// DROPPED.
//
// Otherwise a redelivery months later would resurrect rows an operator
// deliberately removed — which is the one deletion in this design that is
// meant to be irreversible.
func TestAPurgedTaskStaysPurged(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	if _, err := h.apply(taskRecord("t-1", worklet.OpCreate, newTask("t-1"), nil),
		time.Unix(1_700_000_100, 0).UTC()); err != nil {
		t.Fatalf("create: %v", err)
	}
	purge := taskRecord("t-1", worklet.OpPurge,
		map[string]any{"reason": "a duplicate import"}, nil)
	if _, err := h.apply(purge, time.Unix(1_700_000_200, 0).UTC()); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if got := h.count("tracker_tasks"); got != 0 {
		t.Fatalf("the purged task's row survives (%d rows)", got)
	}
	if got := h.count("tracker_deletions"); got != 1 {
		t.Fatalf("the deletion marker was not written (%d rows)", got)
	}

	patch := taskRecord("t-1", worklet.OpPatch,
		worklet.TaskPatch{Title: ptr("resurrected")}, nil)
	_, err := h.apply(patch, time.Unix(1_700_000_300, 0).UTC())
	var gate *gateError
	if !asGate(err, &gate) || gate.reason != statelog.ReasonDeleted {
		t.Fatalf("a record about a purged task was applied (%v)", err)
	}
}

// A PURGE DESTROYS ONE TASK, NOT A SUBTREE — AND LEAVES NO DANGLING PARENT.
//
// The purge deleted the row and everything naming it and left each child's
// `parent_id` pointing at an id that resolves to nothing. No reader can tell
// that from a parent merely held on another node, and the next edit to such a
// child derives its depth and root from a chain that stops at nothing.
//
// Destroying the children instead would be worse: a purge has no inverse, and
// "it was under the thing you purged" is not a confirmation anybody gave. So
// each direct child moves onto the purged task's OWN parent.
func TestAPurgeReParentsItsChildrenRatherThanOrphaningThem(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	at := time.Unix(1_700_000_100, 0).UTC()
	for _, id := range []string{"grandparent", "parent", "child"} {
		if _, err := h.apply(taskRecord(id, worklet.OpCreate, newTask(id), nil), at); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	for _, move := range []struct{ id, parent string }{
		{"parent", "grandparent"}, {"child", "parent"},
	} {
		parent := move.parent
		if _, err := h.apply(taskRecord(move.id, worklet.OpPatch,
			worklet.TaskPatch{Parent: &parent}, nil), at); err != nil {
			t.Fatalf("re-parent %s: %v", move.id, err)
		}
	}
	if got := h.value(`SELECT depth FROM tracker_tasks WHERE id = 'child'`); got != 2 {
		t.Fatalf("the fixture's child is at depth %d, so this case is not the "+
			"shape it names", got)
	}

	purge := taskRecord("parent", worklet.OpPurge,
		map[string]any{"reason": "a duplicate import"}, nil)
	if _, err := h.apply(purge, time.Unix(1_700_000_200, 0).UTC()); err != nil {
		t.Fatalf("purge: %v", err)
	}

	// THE CHILD SURVIVES, which is the half a cascading delete would fail.
	if got := h.count("tracker_tasks"); got != 2 {
		t.Fatalf("%d task rows survive the purge of one task", got)
	}
	// AND ITS PARENT RESOLVES. A dangling pointer here is invisible to
	// every reader and is what the whole case is about.
	var parent sql.NullString
	if err := h.db.Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(),
			`SELECT p.id FROM tracker_tasks c
			 LEFT JOIN tracker_tasks p ON p.id = c.parent_id
			 WHERE c.id = 'child'`).Scan(&parent)
	}); err != nil {
		t.Fatalf("read the child's parent: %v", err)
	}
	if !parent.Valid || parent.String != "grandparent" {
		t.Errorf("the child's parent is %q (resolves=%v); it should have moved "+
			"onto the purged task's own parent", parent.String, parent.Valid)
	}
	// AND THE DERIVED COLUMNS FOLLOWED. Re-parenting the pointer and
	// leaving the closure behind is the same dangling reference one join
	// further away.
	if got := h.value(`SELECT depth FROM tracker_tasks WHERE id = 'child'`); got != 1 {
		t.Errorf("the moved child is at depth %d rather than 1", got)
	}
	if got := h.value(
		`SELECT distance FROM tracker_task_closure
		 WHERE ancestor_id = 'grandparent' AND descendant_id = 'child'`); got != 1 {
		t.Errorf("the closure puts the moved child %d from its new parent", got)
	}
	if got := h.count("tracker_task_closure"); got != 3 {
		t.Errorf("%d closure rows survive; two self-rows and one edge is the "+
			"whole tree after the purge", got)
	}
}

// THE EFFECTIVE INSTANT IS A MAX OVER A SET, so a late record RAISES its
// successors and never lowers them.
//
// Two nodes at one checkpoint have seen the same set of rows and a different
// order of them; a fold over arrival order gives them different answers,
// permanently, on a column nothing repairs.
func TestALateRecordRaisesItsSuccessorsAndNeverLowersThem(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	if _, err := h.apply(taskRecord("t-1", worklet.OpCreate, newTask("t-1"), nil),
		time.Unix(1_700_000_100, 0).UTC()); err != nil {
		t.Fatalf("create: %v", err)
	}
	// A record whose BROKER instant is far ahead of the one before it.
	ahead := taskRecord("t-1", worklet.OpPatch,
		worklet.TaskPatch{Title: ptr("second")}, nil)
	ahead.OpID = "patch-2"
	if _, err := h.apply(ahead, time.Unix(1_700_009_000, 0).UTC()); err != nil {
		t.Fatalf("patch: %v", err)
	}
	before := h.value(
		`SELECT effective_at FROM tracker_history WHERE id = 't-1-create'`)

	// And now one whose broker instant is EARLIER and whose POSITION is
	// BELOW the rows already applied — which is what a record reprocessed
	// after an upgrade looks like, and the one case an overwrite would
	// destroy: every row above it would take this record's older instant.
	behind := taskRecord("t-1", worklet.OpPatch,
		worklet.TaskPatch{Title: ptr("third")}, nil)
	behind.OpID = "patch-3"
	if _, err := h.applyAt(behind, time.Unix(1_700_000_150, 0).UTC(), 1); err != nil {
		t.Fatalf("patch: %v", err)
	}
	if got := h.value(
		`SELECT effective_at FROM tracker_history WHERE id = 'patch-2'`); got !=
		store.EncodeTime(time.Unix(1_700_009_000, 0).UTC()) {
		t.Fatalf("a record reprocessed below an applied row moved that row's "+
			"effective instant to %d — a max over a set can only rise, and two "+
			"nodes that saw the same set in a different order must agree", got)
	}
	after := h.value(
		`SELECT effective_at FROM tracker_history WHERE id = 't-1-create'`)
	if after != before {
		t.Fatalf("an earlier record moved an existing row's effective instant "+
			"from %d to %d — a max over a set can only rise", before, after)
	}
	// The new row's own instant is at least its predecessor's, which is
	// what makes a duration non-negative.
	newest := h.value(`SELECT effective_at FROM tracker_history WHERE id = 'patch-3'`)
	if newest < before {
		t.Fatalf("the newest row's effective instant is %d, below the %d of a "+
			"row at a lower position — a duration between them would be "+
			"negative", newest, before)
	}
}

// A QUIET COMMIT WRITES ITS HISTORY ROW AND NO INBOX ROW.
//
// "Quiet" means it wakes nobody, and nothing else — so the activity feed is a
// complete account of what happened rather than an account of what was
// announced.
func TestAQuietCommitIsRecordedAndAnnouncedToNobody(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	if _, err := h.apply(taskRecord("t-1", worklet.OpCreate, newTask("t-1"), nil),
		time.Unix(1_700_000_100, 0).UTC()); err != nil {
		t.Fatalf("create: %v", err)
	}
	if got := h.count("tracker_history"); got != 1 {
		t.Fatalf("a quiet commit wrote %d history rows, want one", got)
	}
	if got := h.value(`SELECT notified FROM tracker_history WHERE id = 't-1-create'`); got != 0 {
		t.Errorf("a quiet commit is marked notified")
	}
	if got := h.count("tracker_notifications"); got != 0 {
		t.Fatalf("a quiet commit wrote %d inbox rows", got)
	}

	loud := taskRecord("t-1", worklet.OpPatch,
		worklet.TaskPatch{Title: ptr("louder")},
		&worklet.Notify{
			Kind: worklet.ChangeStatus,
			Snapshot: worklet.Snapshot{
				Key: "ENG-1", Assignee: "bo", Watchers: []string{"cy"},
			},
		})
	if _, err := h.apply(loud, time.Unix(1_700_000_200, 0).UTC()); err != nil {
		t.Fatalf("patch: %v", err)
	}
	if got := h.count("tracker_notifications"); got != 2 {
		t.Fatalf("a loud commit wrote %d inbox rows for an assignee and a "+
			"watcher", got)
	}
}

// A CHANGE ANOTHER WRITER ALREADY MADE RECORDS NO MOVE.
//
// A wake is built from the snapshot the writer's tool read OUTSIDE the write's
// transaction, so two writers setting one status from one stale read both
// publish a wake claiming todo→done. Only the first apply moves anything; the
// second compares the row it holds, finds it already done, and its history row
// must say so — `{}` — rather than store the wake's claim of a move this apply
// never made, beside the row of the record that did make it.
func TestAChangeAnotherWriterAlreadyMadeRecordsNoMove(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	if _, err := h.apply(taskRecord("t-1", worklet.OpCreate, newTask("t-1"), nil),
		time.Unix(1_700_000_100, 0).UTC()); err != nil {
		t.Fatalf("create: %v", err)
	}
	done := worklet.StatusDone
	claim := func(op string) worklet.MutationRecord {
		rec := taskRecord("t-1", worklet.OpPatch, worklet.TaskPatch{Status: &done},
			&worklet.Notify{
				Kind:     worklet.ChangeStatus,
				Fields:   map[string]worklet.Delta{"status": {From: "todo", To: "done"}},
				Snapshot: worklet.Snapshot{Key: "ENG-1"},
			})
		rec.OpID = op
		return rec
	}
	if _, err := h.apply(claim("first"), time.Unix(1_700_000_200, 0).UTC()); err != nil {
		t.Fatalf("the first status change: %v", err)
	}
	if _, err := h.apply(claim("second"), time.Unix(1_700_000_300, 0).UTC()); err != nil {
		t.Fatalf("the second status change: %v", err)
	}
	var first, second string
	if err := h.db.Read(t.Context(), func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(t.Context(),
			`SELECT fields_json FROM tracker_history WHERE id = 'first'`).Scan(&first); err != nil {
			return err
		}
		return tx.QueryRowContext(t.Context(),
			`SELECT fields_json FROM tracker_history WHERE id = 'second'`).Scan(&second)
	}); err != nil {
		t.Fatalf("read the history rows: %v", err)
	}
	if !strings.Contains(first, `"status"`) {
		t.Fatalf("the change that moved the status recorded %s — the apply's own "+
			"comparison names the move", first)
	}
	if second != "{}" {
		t.Fatalf("a change another writer had already made recorded %s — its "+
			"apply moved nothing, and the wake's claim is a read taken before "+
			"the first writer's change landed", second)
	}
}

// A RECORD THE VERSION GUARD SKIPPED STORES ITS WRITER'S OWN DELTAS.
//
// Reprocessed below a successor this node already applied — a record deferred
// here and read once it could be — it has no document of its own to compare
// against: the one it would have moved is its successor's. The writer's
// statement is the only account of what it moved, so that is what its row
// says, where a comparison against the successor's document would name a move
// it never made and an empty one would lose it.
func TestARecordTheVersionGuardSkippedStoresItsWritersOwnDeltas(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	if _, err := h.applyAt(taskRecord("t-1", worklet.OpCreate, newTask("t-1"), nil),
		time.Unix(1_700_000_100, 0).UTC(), 1); err != nil {
		t.Fatalf("create: %v", err)
	}
	ahead := taskRecord("t-1", worklet.OpPatch, worklet.TaskPatch{Title: ptr("third")}, nil)
	ahead.OpID = "ahead"
	if _, err := h.applyAt(ahead, time.Unix(1_700_000_300, 0).UTC(), 3); err != nil {
		t.Fatalf("the successor: %v", err)
	}
	late := taskRecord("t-1", worklet.OpPatch, worklet.TaskPatch{Title: ptr("second")},
		&worklet.Notify{
			Kind:     worklet.ChangeFields,
			Fields:   map[string]worklet.Delta{"title": {From: "a task", To: "second"}},
			Snapshot: worklet.Snapshot{Key: "ENG-1"},
		})
	late.OpID = "late"
	if _, err := h.applyAt(late, time.Unix(1_700_000_200, 0).UTC(), 2); err != nil {
		t.Fatalf("the late record: %v", err)
	}
	var fields string
	if err := h.db.Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(),
			`SELECT fields_json FROM tracker_history WHERE id = 'late'`).Scan(&fields)
	}); err != nil {
		t.Fatalf("read the late record's history row: %v", err)
	}
	if !strings.Contains(fields, `"second"`) {
		t.Fatalf("a record the version guard skipped recorded %s — the writer's "+
			"own statement is the only account of what it moved", fields)
	}
}

// THE PROJECT COUNTS ARE MAINTAINED BY THE COMMIT THAT MOVES THEM.
//
// An aggregate over every task in every project on every poll is half a
// million index entries at year five, for three numbers a commit already knows
// how to move.
func TestTheProjectCountsAreMaintainedRatherThanScanned(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	project := worklet.MutationRecord{
		RecordEnvelope: worklet.RecordEnvelope{
			V: worklet.RecordVersion, OpID: "eng-1",
			Subject: worklet.ProjectSubject("ENG"), Op: worklet.OpCreate,
			Writer: "node-a", Scope: worklet.ScopeSet{Subject: true},
		},
		Kind: worklet.ChangeProjectCreated,
		Mutation: mustJSON(worklet.Project{
			V: worklet.DocumentVersion, Key: "ENG", Name: "Engineering",
		}),
	}
	if _, err := h.apply(project, time.Unix(1_700_000_050, 0).UTC()); err != nil {
		t.Fatalf("project: %v", err)
	}
	if _, err := h.apply(taskRecord("t-1", worklet.OpCreate, newTask("t-1"), nil),
		time.Unix(1_700_000_100, 0).UTC()); err != nil {
		t.Fatalf("create: %v", err)
	}
	if got := h.value(`SELECT open_count FROM tracker_projects WHERE key = 'ENG'`); got != 1 {
		t.Fatalf("the open count is %d after one open task", got)
	}

	done := worklet.StatusDone
	if _, err := h.apply(taskRecord("t-1", worklet.OpPatch,
		worklet.TaskPatch{Status: &done}, nil),
		time.Unix(1_700_000_200, 0).UTC()); err != nil {
		t.Fatalf("patch: %v", err)
	}
	if got := h.value(`SELECT open_count FROM tracker_projects WHERE key = 'ENG'`); got != 0 {
		t.Errorf("the open count is %d after the task finished", got)
	}
	if got := h.value(`SELECT done_count FROM tracker_projects WHERE key = 'ENG'`); got != 1 {
		t.Errorf("the done count is %d after the task finished", got)
	}
}

// A TASK FILED FINISHED IS FINISHED FROM ITS FIRST ROW.
//
// A create may name the status it starts in — a person filing into a board's
// Done lane files a done task, in one record rather than a create followed by
// a move that could fail after the task was filed in the wrong column. The
// finish instants and the project census are derived by the applier from the
// status GROUP on every commit, a create included, so a task created `done`
// carries `done_at` and counts as done from the start; one created `closed`
// carries both instants and counts as closed. A create path that skipped the
// derivation would leave a done task that "finished" at no time, which every
// "done this week" filter and burn-down quietly drops.
func TestATaskCreatedFinishedIsStampedAndCountedFromItsFirstRow(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	project := worklet.MutationRecord{
		RecordEnvelope: worklet.RecordEnvelope{
			V: worklet.RecordVersion, OpID: "eng-1",
			Subject: worklet.ProjectSubject("ENG"), Op: worklet.OpCreate,
			Writer: "node-a", Scope: worklet.ScopeSet{Subject: true},
		},
		Kind: worklet.ChangeProjectCreated,
		Mutation: mustJSON(worklet.Project{
			V: worklet.DocumentVersion, Key: "ENG", Name: "Engineering",
		}),
	}
	if _, err := h.apply(project, time.Unix(1_700_000_050, 0).UTC()); err != nil {
		t.Fatalf("project: %v", err)
	}
	for _, c := range []struct {
		id     string
		status worklet.Status
	}{
		{"t-done", worklet.StatusDone},
		{"t-closed", worklet.StatusClosed},
	} {
		task := newTask(c.id)
		task.Status, task.StatusGroup = c.status, c.status.Group()
		if _, err := h.apply(taskRecord(c.id, worklet.OpCreate, task, nil),
			time.Unix(1_700_000_100, 0).UTC()); err != nil {
			t.Fatalf("create %s: %v", c.id, err)
		}
	}
	for _, c := range []struct {
		id           string
		done, closed int64
	}{
		{"t-done", 1, 0},
		{"t-closed", 1, 1},
	} {
		done := h.value(`SELECT done_at IS NOT NULL FROM tracker_tasks WHERE id = ?`, c.id)
		closed := h.value(`SELECT closed_at IS NOT NULL FROM tracker_tasks WHERE id = ?`, c.id)
		if done != c.done || closed != c.closed {
			t.Errorf("%s was created with done_at set %v and closed_at set %v, "+
				"want %v and %v", c.id, done == 1, closed == 1, c.done == 1, c.closed == 1)
		}
	}
	for column, want := range map[string]int64{
		"open_count": 0, "done_count": 1, "closed_count": 1,
	} {
		if got := h.value(`SELECT ` + column + ` FROM tracker_projects WHERE key = 'ENG'`); got != want {
			t.Errorf("the project's %s is %d after one task created done and one "+
				"closed, want %d", column, got, want)
		}
	}
}

// A CYCLE APPLIES COMPLETELY AND RAISES A FLAG.
//
// Two concurrent re-parents on two nodes form a shape no single write could
// see, and both records are legitimately committed. A stalled log is every
// node stopping over one task's shape; a flag is a repair somebody can make.
func TestACycleIsFlaggedRatherThanStalling(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	for _, id := range []string{"a", "b"} {
		if _, err := h.apply(taskRecord(id, worklet.OpCreate, newTask(id), nil),
			time.Unix(1_700_000_100, 0).UTC()); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	for _, pair := range [][2]string{{"a", "b"}, {"b", "a"}} {
		parent := pair[1]
		if _, err := h.apply(taskRecord(pair[0], worklet.OpPatch,
			worklet.TaskPatch{Parent: &parent}, nil),
			time.Unix(1_700_000_200, 0).UTC()); err != nil {
			t.Fatalf("re-parent %s under %s: %v", pair[0], pair[1], err)
		}
	}
	if got := h.value(`SELECT COUNT(*) FROM tracker_tasks WHERE cycle = 1`); got == 0 {
		t.Fatal("a parent cycle was applied and flagged nowhere, so nothing " +
			"will ever repair it")
	}
}

func mustJSON(v any) []byte {
	body, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return body
}

func ptr[T any](v T) *T { return &v }

// ONE SPELLING OF "NOTHING MOVED" IN THE FIELDS COLUMN.
//
// # What this protects
//
// `fields_json` is written from one of two places — the applier's own deltas
// where it compared two documents, and the RECORD's notification where it did
// not. Both are [TaskDeltas], and [TaskDeltas] returns a NIL map when nothing
// moved. `jsonOf` is `json.Marshal`, and a nil map marshals to the literal
// `null`.
//
// So on `notify != nil` alone, every loud commit that moved no field stored
// `null` in a column whose every other row stores `{}`. Nothing broke:
// `json_extract` answers NULL against either and the dashboard coalesces. That
// is exactly why it survived, and exactly what makes it worth a test — two
// spellings of one fact in one column are indistinguishable until the first
// query compares them, and then they disagree silently.
//
// A LEAD RE-STATING A PRIORITY LIST IS THE CASE TO ASSERT ON, and the choice
// is load-bearing twice over. It is loud — a lead writing somebody else's
// queue is the one person write that announces itself, so the row reaches the
// notification branch at all — and the SECOND statement of one list moves
// nothing, because `personDeltas` deliberately records who re-ordered a queue
// and not when (`priorities_set_at` moves on every write and would make every
// re-statement look like a change).
//
// It used to be the FIRST such write, on the argument that a document apply
// "passes no deltas at all". That is no longer true and was the bug
// `deltas.go` exists to fix: every document apply now compares the stored
// object with the record's, so a first priorities write moves `priorities` and
// `priorities_set_by` and has a real delta to store. A purge looks like the
// obvious alternative and is not — `Gates.purgeNotify` returns nil unless the
// project has a lead, so on a harness without one the row takes neither branch
// and the assertion passes for the wrong reason.
func TestAHistoryRowWithNoDeltasStoresAnEmptyObject(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if _, err := r.writer.CreateTask(t.Context(), "op-1", newTask("t-1"), nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r.drain()
	// A LEAD WRITING SOMEBODY ELSE'S LIST, because that is what carries a
	// notification: a person reordering their OWN queue announces nothing,
	// so a self-write leaves `notified = 0` and never reaches the branch
	// this case is about.
	lead := r.writer.As("bob", worklet.AuthorHuman, worklet.Provenance{})
	for i, op := range []string{"op-prio", "op-prio-again"} {
		// THE CLOCK MOVES BETWEEN THEM, which is what makes the second
		// write a real test of the one field `personDeltas` leaves out:
		// `priorities_set_at` is re-stamped on every lead write, so on a
		// frozen clock this case would pass whether or not it were
		// recorded.
		r.at = wednesday.Add(time.Duration(i) * time.Hour)
		if _, err := lead.WritePriorities(t.Context(), op, "ana",
			[]string{"t-1"}, nil, worklet.PersonAuthority{Authorized: true}); err != nil {
			t.Fatalf("WritePriorities %s: %v", op, err)
		}
		r.drain()
	}

	// THE NEWEST ROW, which is the re-statement. The first write is the
	// one that moved the list onto an empty record and has a delta of its
	// own — see TestAPriorityListRecordsTheOrderItMoved.
	got := r.strings(`SELECT fields_json FROM tracker_history
		WHERE subject_kind = ? AND kind = ?
		ORDER BY log_seq DESC LIMIT 1`,
		string(worklet.KindPerson), string(worklet.ChangePrioritised))
	if len(got) == 0 {
		t.Fatal("the priorities write left no history row, so this case " +
			"asserts nothing about the column it is here for")
	}
	// AND THE ROW CARRIED A NOTIFICATION, or the branch under test was
	// never taken and `{}` below is the untouched initial value rather
	// than an answer. This is the assertion the first draft of this case
	// was missing, and it passed either way without it.
	if notified := r.strings(`SELECT notified FROM tracker_history
		WHERE subject_kind = ? AND kind = ?
		ORDER BY log_seq DESC LIMIT 1`,
		string(worklet.KindPerson), string(worklet.ChangePrioritised)); len(notified) == 0 ||
		notified[0] != "1" {

		t.Fatalf("the priorities row carries notified = %v, so the write "+
			"announced nothing and this case never reaches the branch it "+
			"exists for", notified)
	}
	if got[0] != "{}" {
		t.Errorf("a commit that moved no field stored fields_json = %q, want "+
			"`{}` — every other row in this column spells an empty delta set "+
			"that way, and a second spelling is one a reader comparing them "+
			"cannot tell from a real difference", got[0])
	}
}

// A KIND NOBODY DECLARED FAULTS THE APPLY, rather than applying as nothing.
//
// A record of a kind this build has no case for, at a version it can read, is a
// writer that published a kind it never declared: the dispatch has nothing to
// hand it to, and the applier's unconditional return after the switch is what
// makes that mistake visible. It is not a gate — a gate drops a record
// knowingly — and nothing is written.
func TestAKindNobodyDeclaredFaults(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	rec := taskRecord("t-1", worklet.OpCreate, newTask("t-1"), nil)
	rec.Subject.Kind = "nonesuch"
	rec.OpID = "nonesuch-1"

	_, err := h.apply(rec, time.Unix(1_700_000_100, 0).UTC())
	var gate *gateError
	if asGate(err, &gate) {
		t.Fatalf("a kind nobody declared was GATED as %q, which drops it knowingly; "+
			"a writer's undeclared kind must fault", gate.reason)
	}
	if err == nil || !strings.Contains(err.Error(), "nonesuch") {
		t.Fatalf("applying a kind nobody declared = %v, want a fault naming the kind", err)
	}
	if got := h.count("tracker_tasks"); got != 0 {
		t.Errorf("the estate holds %d task(s) after a record that faulted", got)
	}
}

// A REMARK'S RECORDS WRITE ONLY WHAT EACH OWNS: a post creates the row, an edit
// rewrites its words, and neither reaches the column an answer put there.
//
// The apply was one upsert of everything the record carried, and a comment
// record carries no `answered_by` — so it wrote NULL there every time. An
// author correcting the wording of a question somebody had already answered
// reopened it on every board, in `asked_of=` and in the answerer's own queue;
// a post re-sent under an id that already held a row rewrote that row whole,
// edit and answer included.
func TestAnEditOfAQuestionKeepsTheAnswerThatClosedIt(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	at := time.Unix(1_700_000_100, 0).UTC()
	if _, err := h.apply(taskRecord("t-1", worklet.OpCreate, newTask("t-1"), nil), at); err != nil {
		t.Fatalf("create: %v", err)
	}
	remark := func(op string, kind worklet.ChangeKind, comment worklet.Comment) {
		t.Helper()
		rec := taskRecord("t-1", worklet.OpPatch, worklet.TaskPatch{Comment: &comment}, nil)
		rec.Kind, rec.OpID = kind, op
		if _, err := h.apply(rec, at); err != nil {
			t.Fatalf("%s: %v", op, err)
		}
	}
	question := worklet.Comment{
		ID: "cm-1", Task: "t-1", Author: "ana", AuthorKind: worklet.AuthorHuman,
		Body: "who owns the rollback?", Ask: "bob", CreatedAt: at,
	}
	remark("op-ask", worklet.ChangeComment, question)
	answers := "cm-1"
	remark("op-answer", worklet.ChangeComment, worklet.Comment{
		ID: "cm-2", Task: "t-1", Author: "bob", AuthorKind: worklet.AuthorHuman,
		Body: "platform does", Answers: &answers, CreatedAt: at,
	})

	open := func() int64 {
		t.Helper()
		return h.value(`SELECT COUNT(*) FROM tracker_comments
			WHERE ask <> '' AND resolved = 0 AND answered_by IS NULL AND removed = 0`)
	}
	answeredBy := func() string {
		t.Helper()
		var by sql.NullString
		if err := h.db.Read(t.Context(), func(tx *sql.Tx) error {
			return tx.QueryRowContext(t.Context(),
				`SELECT answered_by FROM tracker_comments WHERE id = 'cm-1'`).Scan(&by)
		}); err != nil {
			t.Fatalf("read answered_by: %v", err)
		}
		return by.String
	}
	stored := func() worklet.Comment {
		t.Helper()
		var document []byte
		if err := h.db.Read(t.Context(), func(tx *sql.Tx) error {
			return tx.QueryRowContext(t.Context(),
				`SELECT document FROM tracker_comments WHERE id = 'cm-1'`).Scan(&document)
		}); err != nil {
			t.Fatalf("read the question: %v", err)
		}
		var c worklet.Comment
		if err := json.Unmarshal(document, &c); err != nil {
			t.Fatalf("decode the question: %v", err)
		}
		return c
	}
	if got := answeredBy(); got != "cm-2" || open() != 0 {
		t.Fatalf("after the answer the question is answered by %q with %d open, "+
			"want cm-2 and none", got, open())
	}

	// THE AUTHOR CORRECTS THE QUESTION'S WORDS — the record carrying the
	// whole stored remark with its body replaced, as the writer forms it.
	edited := question
	edited.Body, edited.UpdatedAt = "who owns the rollback now?", at.Add(time.Minute)
	remark("op-edit", worklet.ChangeCommentEdited, edited)
	if got := answeredBy(); got != "cm-2" {
		t.Errorf("after an edit of its words the question is answered by %q, "+
			"want cm-2 — an edit reopened a question somebody had answered", got)
	}
	if n := open(); n != 0 {
		t.Errorf("after an edit %d question(s) read as open", n)
	}
	if got := stored(); got.Body != edited.Body || !got.UpdatedAt.Equal(edited.UpdatedAt) {
		t.Errorf("the edited question reads %q at %v, want %q at %v",
			got.Body, got.UpdatedAt, edited.Body, edited.UpdatedAt)
	}

	// AN EDIT IS THE WORDS AND NOTHING ELSE, whatever its record carries
	// beside them: an edit naming another person asked leaves the question
	// asking the person it asked.
	widened := edited
	widened.Body, widened.Ask = "who owns the rollback, really?", "cy"
	remark("op-edit-widened", worklet.ChangeCommentEdited, widened)
	if got := h.value(`SELECT COUNT(*) FROM tracker_comments
		WHERE id = 'cm-1' AND ask = 'bob'`); got != 1 {
		t.Errorf("an edit record naming another person asked moved the ask column")
	}
	if got := stored(); got.Ask != "bob" || got.Body != widened.Body {
		t.Errorf("the edited document asks %q and reads %q, want bob and %q — "+
			"an edit moved what the remark asked", got.Ask, got.Body, widened.Body)
	}

	// A POST OF A ROW ALREADY THERE CHANGES NOTHING: not its words, not
	// its answer, not when it was written.
	before := stored()
	reposted := question
	reposted.Body, reposted.CreatedAt = "a different remark entirely", at.Add(time.Hour)
	remark("op-ask-again", worklet.ChangeComment, reposted)
	if got := stored(); got.Body != before.Body || !got.CreatedAt.Equal(before.CreatedAt) {
		t.Errorf("a post of an existing remark rewrote it to %q at %v, want %q at %v",
			got.Body, got.CreatedAt, before.Body, before.CreatedAt)
	}
	if got := answeredBy(); got != "cm-2" || open() != 0 {
		t.Errorf("a post of an existing remark left it answered by %q with %d "+
			"open, want cm-2 and none", got, open())
	}
	if got := h.count("tracker_comments"); got != 2 {
		t.Errorf("the thread holds %d rows, want the question and its answer", got)
	}
}

// A RESOLVE WRITES THE RESOLUTION AND NOTHING ELSE: that a remark is resolved,
// who resolved it and when, in its columns and in the document carrying them,
// beside the words its edit wrote and the answer that closed it.
//
// The apply wrote only what a post and an edit own, so a resolve record went
// the way of a post, found its row already there and wrote nothing: a
// question resolved without an answer stayed open on every board and in its
// person's queue, and was missing from what they decided lately.
func TestAResolveWritesTheResolutionAndNothingElse(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	at := time.Unix(1_700_000_100, 0).UTC()
	if _, err := h.apply(taskRecord("t-1", worklet.OpCreate, newTask("t-1"), nil), at); err != nil {
		t.Fatalf("create: %v", err)
	}
	remark := func(op string, kind worklet.ChangeKind, comment worklet.Comment) {
		t.Helper()
		rec := taskRecord("t-1", worklet.OpPatch, worklet.TaskPatch{Comment: &comment}, nil)
		rec.Kind, rec.OpID = kind, op
		if _, err := h.apply(rec, at); err != nil {
			t.Fatalf("%s: %v", op, err)
		}
	}
	question := worklet.Comment{
		ID: "cm-1", Task: "t-1", Author: "ana", AuthorKind: worklet.AuthorHuman,
		Body: "who owns the rollback?", Ask: "bob", CreatedAt: at,
	}
	remark("op-ask", worklet.ChangeComment, question)
	answers := "cm-1"
	remark("op-answer", worklet.ChangeComment, worklet.Comment{
		ID: "cm-2", Task: "t-1", Author: "bob", AuthorKind: worklet.AuthorHuman,
		Body: "platform does", Answers: &answers, CreatedAt: at,
	})
	edited := question
	edited.Body, edited.UpdatedAt = "who owns the rollback now?", at.Add(time.Minute)
	remark("op-edit", worklet.ChangeCommentEdited, edited)

	type stored struct {
		resolved               int64
		resolvedBy, answeredBy sql.NullString
		resolvedAt, updatedAt  sql.NullInt64
		ask, body, record      string
		document               worklet.Comment
	}
	read := func() stored {
		t.Helper()
		var row stored
		var document []byte
		if err := h.db.Read(t.Context(), func(tx *sql.Tx) error {
			return tx.QueryRowContext(t.Context(), `
				SELECT resolved, resolved_by, resolved_at, answered_by, updated_at,
				       ask, body, record_id, document
				FROM tracker_comments WHERE id = 'cm-1'`).Scan(&row.resolved,
				&row.resolvedBy, &row.resolvedAt, &row.answeredBy, &row.updatedAt,
				&row.ask, &row.body, &row.record, &document)
		}); err != nil {
			t.Fatalf("read the question: %v", err)
		}
		if err := json.Unmarshal(document, &row.document); err != nil {
			t.Fatalf("decode the question: %v", err)
		}
		return row
	}
	resolvedAt := func(row stored) time.Time {
		t.Helper()
		if !row.resolvedAt.Valid {
			return time.Time{}
		}
		return store.DecodeTime(row.resolvedAt.Int64)
	}

	// THE RESOLVE, carrying beside its resolution everything a record could
	// carry wrong: other words, another person asked, a later edit instant.
	first := at.Add(2 * time.Minute)
	resolve := edited
	resolve.Resolved, resolve.ResolvedBy, resolve.ResolvedAt = true, "ana", &first
	resolve.Body, resolve.Ask, resolve.UpdatedAt = "never mind", "cy", at.Add(time.Hour)
	remark("op-resolve", worklet.ChangeCommentResolved, resolve)

	got := read()
	if got.resolved != 1 || got.resolvedBy.String != "ana" || !resolvedAt(got).Equal(first) {
		t.Errorf("the question reads resolved=%d by %q at %v, want resolved by ana at %v",
			got.resolved, got.resolvedBy.String, resolvedAt(got), first)
	}
	if doc := got.document; !doc.Resolved || doc.ResolvedBy != "ana" ||
		doc.ResolvedAt == nil || !doc.ResolvedAt.Equal(first) {
		t.Errorf("the question's document reads resolved=%v by %q at %v, want what "+
			"its columns say: the document and the row are one account of it",
			doc.Resolved, doc.ResolvedBy, doc.ResolvedAt)
	}
	if got.answeredBy.String != "cm-2" {
		t.Errorf("after its resolve the question is answered by %q, want cm-2", got.answeredBy.String)
	}
	if got.body != edited.Body || got.document.Body != edited.Body {
		t.Errorf("after its resolve the question reads %q (its document %q), want "+
			"its edit's words %q", got.body, got.document.Body, edited.Body)
	}
	if edit := store.DecodeTime(got.updatedAt.Int64); !edit.Equal(edited.UpdatedAt) ||
		!got.document.UpdatedAt.Equal(edited.UpdatedAt) {
		t.Errorf("after its resolve the question was last edited at %v (its document "+
			"%v), want its edit's %v", edit, got.document.UpdatedAt, edited.UpdatedAt)
	}
	if got.ask != "bob" || got.document.Ask != "bob" || got.record != "op-ask" {
		t.Errorf("after its resolve the question asks %q (document %q) under record %q, "+
			"want bob under the record that posted it", got.ask, got.document.Ask, got.record)
	}

	// STATED AGAIN, WRITTEN AGAIN, as an edit that changes no word is: the
	// row holds the newest resolution.
	again := at.Add(3 * time.Minute)
	resolve.ResolvedAt = &again
	remark("op-resolve-again", worklet.ChangeCommentResolved, resolve)
	if got := read(); !resolvedAt(got).Equal(again) || got.document.ResolvedAt == nil ||
		!got.document.ResolvedAt.Equal(again) {
		t.Errorf("a resolve stated again left the question resolved at %v (its "+
			"document %v), want %v", resolvedAt(got), got.document.ResolvedAt, again)
	}

	// A RESOLVE OF A REMARK THIS NODE DOES NOT HOLD writes nothing: a row
	// made up from it would be a remark nobody posted.
	ghost := resolve
	ghost.ID = "cm-9"
	remark("op-resolve-ghost", worklet.ChangeCommentResolved, ghost)
	if got := h.count("tracker_comments"); got != 2 {
		t.Errorf("the thread holds %d rows after a resolve of a remark it does not "+
			"hold, want the question and its answer", got)
	}
}

// A REMOVAL RECORD WRITES NOTHING. No gesture takes a tracker comment down, so
// no writer forms the removal kind, and a record of it that reaches the apply
// anyway must not take the post's arm, which every kind but an edit and a
// resolve takes. There, on a node that did not hold the remark, it made one up
// from the removal, closing the ask that remark claimed to answer. A removal
// of a remark the node holds leaves it as posted: nothing takes one down.
//
// Mutation: drop the removal kind's arm and the removal of a remark the node
// does not hold writes a row and closes cm-2.
func TestARemovalRecordWritesNothing(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	at := time.Unix(1_700_000_100, 0).UTC()
	if _, err := h.apply(taskRecord("t-1", worklet.OpCreate, newTask("t-1"), nil), at); err != nil {
		t.Fatalf("create: %v", err)
	}
	remark := func(op string, kind worklet.ChangeKind, comment worklet.Comment) {
		t.Helper()
		rec := taskRecord("t-1", worklet.OpPatch, worklet.TaskPatch{Comment: &comment}, nil)
		rec.Kind, rec.OpID = kind, op
		if _, err := h.apply(rec, at); err != nil {
			t.Fatalf("%s: %v", op, err)
		}
	}
	remark("op-remark", worklet.ChangeComment, worklet.Comment{
		ID: "cm-1", Task: "t-1", Author: "ana", AuthorKind: worklet.AuthorHuman,
		Body: "the rollback is platform's", CreatedAt: at,
	})
	remark("op-ask", worklet.ChangeComment, worklet.Comment{
		ID: "cm-2", Task: "t-1", Author: "bob", AuthorKind: worklet.AuthorHuman,
		Body: "who owns the pager?", Ask: "ana", CreatedAt: at,
	})

	answers := "cm-2"
	for op, gone := range map[string]worklet.Comment{
		"op-remove": {ID: "cm-1", Task: "t-1", Author: "ana",
			AuthorKind: worklet.AuthorHuman, Removed: true, CreatedAt: at},
		"op-remove-unheld": {ID: "cm-9", Task: "t-1", Author: "ana",
			AuthorKind: worklet.AuthorHuman, Body: "me", Answers: &answers,
			Removed: true, CreatedAt: at},
	} {
		remark(op, worklet.ChangeCommentRemoved, gone)
	}

	if got := h.count("tracker_comments"); got != 2 {
		t.Errorf("the thread holds %d rows after two removals, want the remark "+
			"and the question it held before them", got)
	}
	var removed int64
	var body string
	var answeredBy sql.NullString
	if err := h.db.Read(t.Context(), func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(t.Context(), `
			SELECT removed, body FROM tracker_comments WHERE id = 'cm-1'`).
			Scan(&removed, &body); err != nil {
			return err
		}
		return tx.QueryRowContext(t.Context(), `
			SELECT answered_by FROM tracker_comments WHERE id = 'cm-2'`).Scan(&answeredBy)
	}); err != nil {
		t.Fatalf("read the thread: %v", err)
	}
	if removed != 0 || body != "the rollback is platform's" {
		t.Errorf("after a removal record cm-1 reads removed=%d %q, want it as posted",
			removed, body)
	}
	if answeredBy.Valid {
		t.Errorf("a removal record closed cm-2 as answered by %q", answeredBy.String)
	}
}

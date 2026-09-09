package tracker_test

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tracker"
)

// applyHarness is one node's replicated estate with the tracker's applier over
// it.
type applyHarness struct {
	t       *testing.T
	db      *store.DB
	applier *tracker.Applier
	seq     uint64
}

func newApplyHarness(t *testing.T) *applyHarness {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "node.db"),
		store.Options{PinnedWriters: 1})
	if err != nil {
		t.Fatalf("open a store: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close the store: %v", err)
		}
	})
	return &applyHarness{t: t, db: db, applier: tracker.NewApplier("node-a")}
}

// apply runs one record through the applier at the next position.
func (h *applyHarness) apply(rec tracker.MutationRecord, brokerAt time.Time) (int, error) {
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
func (h *applyHarness) applyAt(rec tracker.MutationRecord, brokerAt time.Time,
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
	err = h.db.Replicated().Tx(h.t.Context(), func(tx *sql.Tx) error {
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
		n, err := h.applier.Apply(h.t.Context(), tx, record, statelog.ApplyOptions{
			Now: brokerAt, StoredAt: brokerAt,
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
	if err := h.db.Replicated().Read(h.t.Context(), func(tx *sql.Tx) error {
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
	if err := h.db.Replicated().Read(h.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(h.t.Context(), query, args...).Scan(&n)
	}); err != nil {
		h.t.Fatalf("read %q: %v", query, err)
	}
	return n.Int64
}

func taskRecord(id string, op tracker.OpKind, payload any, notify *tracker.Notify) tracker.MutationRecord {
	body, _ := json.Marshal(payload)
	return tracker.MutationRecord{
		RecordEnvelope: tracker.RecordEnvelope{
			V: tracker.RecordVersion, OpID: id + "-" + string(op),
			Subject: tracker.TaskSubject(id), Op: op,
			CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
			Writer:    "node-a", Scope: tracker.ScopeSet{Subject: true},
		},
		Mutation: body, Actor: "ana", ActorKind: tracker.AuthorHuman,
		Notify: notify,
	}
}

func newTask(id string) tracker.Task {
	at := time.Unix(1_700_000_000, 0).UTC()
	return tracker.Task{
		V: tracker.DocumentVersion, ID: id, Key: "ENG-1", Project: "ENG",
		Type: "task", Title: "a task", Status: tracker.StatusTodo,
		StatusGroup: tracker.GroupNotStarted, Priority: tracker.PriorityNormal,
		Rank: tracker.RankOrigin, CreatedAt: at, UpdatedAt: at,
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
	if _, err := h.apply(taskRecord("t-1", tracker.OpCreate, newTask("t-1"), nil),
		time.Unix(1_700_000_100, 0).UTC()); err != nil {
		t.Fatalf("create: %v", err)
	}

	turn := tracker.MutationRecord{
		RecordEnvelope: tracker.RecordEnvelope{
			V: tracker.RecordVersion, OpID: "turn-1",
			Subject: tracker.TurnSubject("t-1"), Op: tracker.OpTurn,
			CreatedAt: time.Unix(1_700_000_200, 0).UTC(),
			Writer:    "node-a", Scope: tracker.ScopeSet{Subject: true},
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
	// redelivery after an adoption scrubbed the operation ledger looks
	// like.
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
	turn := tracker.MutationRecord{
		RecordEnvelope: tracker.RecordEnvelope{
			V: tracker.RecordVersion, OpID: "turn-1",
			Subject: tracker.TurnSubject("missing"), Op: tracker.OpTurn,
			Writer: "node-a", Scope: tracker.ScopeSet{Subject: true},
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
	early := taskRecord("t-1", tracker.OpCreate, newTask("t-1"), nil)
	early.Writer = "node-b"
	if _, err := h.apply(early, time.Unix(1_700_000_100, 0).UTC()); err != nil {
		t.Fatalf("a record from before the eviction was refused: %v", err)
	}

	eviction := tracker.MutationRecord{
		RecordEnvelope: tracker.RecordEnvelope{
			V: tracker.RecordVersion, OpID: "evict-1",
			Subject: tracker.EvictionSubject("node-b"), Op: tracker.OpEviction,
			Writer: "node-a", Scope: tracker.ScopeSet{Subject: true},
		},
		Mutation: mustJSON(tracker.Eviction{
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
	redelivered := taskRecord("t-3", tracker.OpCreate, newTask("t-3"), nil)
	redelivered.Writer = "node-b"
	redelivered.OpID = "t-3-redelivered"
	if _, err := h.applyAt(redelivered, time.Unix(1_700_000_100, 0).UTC(), 1); err != nil {
		t.Fatalf("a record from before the eviction was gated after it: %v", err)
	}

	late := taskRecord("t-2", tracker.OpCreate, newTask("t-2"), nil)
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
	e, ok := err.(*gateError)
	if ok {
		*target = e
	}
	return ok
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
	if _, err := h.apply(taskRecord("t-1", tracker.OpCreate, newTask("t-1"), nil),
		time.Unix(1_700_000_100, 0).UTC()); err != nil {
		t.Fatalf("create: %v", err)
	}
	purge := taskRecord("t-1", tracker.OpPurge,
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

	patch := taskRecord("t-1", tracker.OpPatch,
		tracker.TaskPatch{Title: ptr("resurrected")}, nil)
	_, err := h.apply(patch, time.Unix(1_700_000_300, 0).UTC())
	var gate *gateError
	if !asGate(err, &gate) || gate.reason != statelog.ReasonDeleted {
		t.Fatalf("a record about a purged task was applied (%v)", err)
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
	if _, err := h.apply(taskRecord("t-1", tracker.OpCreate, newTask("t-1"), nil),
		time.Unix(1_700_000_100, 0).UTC()); err != nil {
		t.Fatalf("create: %v", err)
	}
	// A record whose BROKER instant is far ahead of the one before it.
	ahead := taskRecord("t-1", tracker.OpPatch,
		tracker.TaskPatch{Title: ptr("second")}, nil)
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
	behind := taskRecord("t-1", tracker.OpPatch,
		tracker.TaskPatch{Title: ptr("third")}, nil)
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
	if _, err := h.apply(taskRecord("t-1", tracker.OpCreate, newTask("t-1"), nil),
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

	loud := taskRecord("t-1", tracker.OpPatch,
		tracker.TaskPatch{Title: ptr("louder")},
		&tracker.Notify{
			Kind: tracker.ChangeStatus,
			Snapshot: tracker.Snapshot{
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

// THE PROJECT COUNTS ARE MAINTAINED BY THE COMMIT THAT MOVES THEM.
//
// An aggregate over every task in every project on every poll is half a
// million index entries at year five, for three numbers a commit already knows
// how to move.
func TestTheProjectCountsAreMaintainedRatherThanScanned(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	project := tracker.MutationRecord{
		RecordEnvelope: tracker.RecordEnvelope{
			V: tracker.RecordVersion, OpID: "eng-1",
			Subject: tracker.ProjectSubject("ENG"), Op: tracker.OpCreate,
			Writer: "node-a", Scope: tracker.ScopeSet{Subject: true},
		},
		Mutation: mustJSON(tracker.Project{
			V: tracker.DocumentVersion, Key: "ENG", Name: "Engineering",
		}),
	}
	if _, err := h.apply(project, time.Unix(1_700_000_050, 0).UTC()); err != nil {
		t.Fatalf("project: %v", err)
	}
	if _, err := h.apply(taskRecord("t-1", tracker.OpCreate, newTask("t-1"), nil),
		time.Unix(1_700_000_100, 0).UTC()); err != nil {
		t.Fatalf("create: %v", err)
	}
	if got := h.value(`SELECT open_count FROM tracker_projects WHERE key = 'ENG'`); got != 1 {
		t.Fatalf("the open count is %d after one open task", got)
	}

	done := tracker.StatusDone
	if _, err := h.apply(taskRecord("t-1", tracker.OpPatch,
		tracker.TaskPatch{Status: &done}, nil),
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

// A CYCLE APPLIES COMPLETELY AND RAISES A FLAG.
//
// Two concurrent re-parents on two nodes form a shape no single write could
// see, and both records are legitimately committed. A stalled log is every
// node stopping over one task's shape; a flag is a repair somebody can make.
func TestACycleIsFlaggedRatherThanStalling(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	for _, id := range []string{"a", "b"} {
		if _, err := h.apply(taskRecord(id, tracker.OpCreate, newTask(id), nil),
			time.Unix(1_700_000_100, 0).UTC()); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	for _, pair := range [][2]string{{"a", "b"}, {"b", "a"}} {
		parent := pair[1]
		if _, err := h.apply(taskRecord(pair[0], tracker.OpPatch,
			tracker.TaskPatch{Parent: &parent}, nil),
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

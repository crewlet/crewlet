package tracker_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
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

	// maxVariables is what every collection's insert chunks to. It starts
	// at the estate's own probed limit, which is what the framework hands
	// a real apply; a case that wants a CHUNK BOUNDARY it can count sets
	// its own, because the probed 2 000 puts every collection the caps
	// permit inside one statement.
	maxVariables int
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
	return &applyHarness{
		t: t, db: db, applier: tracker.NewApplier("node-a"),
		maxVariables: db.Caps().MaxVariables,
	}
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

// text reads one string column.
func (h *applyHarness) text(query string, args ...any) string {
	h.t.Helper()
	var out sql.NullString
	if err := h.db.Replicated().Read(h.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(h.t.Context(), query, args...).Scan(&out)
	}); err != nil {
		h.t.Fatalf("read %q: %v", query, err)
	}
	return out.String
}

// seedChildren fills a root's subtree straight into the tables, for the one
// case that needs a subtree LARGER than a cap rather than a subtree of a
// particular shape.
//
// WRITTEN AS THE APPLIER WOULD LEAVE IT — the task row with its parent, root
// and depth, plus the two closure rows (self at distance 0, root at distance
// 1) — because what the case exercises is the applier REBUILDING this, and a
// fixture that skipped the closure would be asking it to build rather than to
// redo. The ids the assertions read are applied through the real path; this is
// only the bulk in between.
func (h *applyHarness) seedChildren(root string, n int) {
	h.t.Helper()
	if err := h.db.Replicated().Tx(h.t.Context(), func(tx *sql.Tx) error {
		for i := range n {
			id := fmt.Sprintf("child-%04d", i)
			if _, err := tx.ExecContext(h.t.Context(), `
				INSERT INTO tracker_tasks
					(id, key, project_key, parent_id, root_id, depth,
					 type, title, status, status_group, rank, document,
					 created_at, updated_at, version)
				VALUES (?,?,'ENG',?,?,1,'task','a task','todo',
					'not_started','nn','{}',0,0,1)
				ON CONFLICT (id) DO NOTHING`,
				id, "ENG-"+id, root, root); err != nil {
				return err
			}
			for _, row := range [][2]any{{id, 0}, {root, 1}} {
				if _, err := tx.ExecContext(h.t.Context(), `
					INSERT INTO tracker_task_closure
						(ancestor_id, descendant_id, distance)
					VALUES (?,?,?)
					ON CONFLICT (ancestor_id, descendant_id) DO NOTHING`,
					row[0], id, row[1]); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		h.t.Fatalf("seed %d children of %s: %v", n, root, err)
	}
}

func taskRecord(id string, op tracker.OpKind, payload any, notify *tracker.Notify) tracker.MutationRecord {
	body, _ := json.Marshal(payload)
	return tracker.MutationRecord{
		RecordEnvelope: tracker.RecordEnvelope{
			V: tracker.RecordVersion, OpID: id + "-" + string(op),
			Subject: tracker.TaskSubject(id), Op: op,
			CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
			Writer:    "node-a", Scope: tracker.ScopeSet{Subject: true, Container: "ENG"},
		},
		Mutation: body, Actor: "ana", ActorKind: tracker.AuthorHuman,
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
			Writer:    "node-a", Scope: tracker.ScopeSet{Subject: true, Container: "ENG"},
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
			Writer: "node-a", Scope: tracker.ScopeSet{Subject: true, Container: "ENG"},
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

// A PURGE TAKES A DEPENDENCY'S MIRROR WITH THE EDGE IT MIRRORS.
//
// The purge deleted the derived edge in both directions and left the mirror
// rows — the blocker's own list of who waits on it — naming a task that no
// longer exists.
func TestAPurgeTakesTheMirrorOfADependencyWithIt(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "dep")
	filedTask(t, r, "blk")
	filedTask(t, r, "later")
	for _, edge := range []struct{ dependent, blocker string }{
		{"dep", "blk"}, {"blk", "later"},
	} {
		if _, err := r.writer.Depend(t.Context(), "op-"+edge.dependent,
			tracker.DependencyChange{
				Task: edge.dependent, Project: "ENG",
				WaitingOnAdd: []string{edge.blocker},
			}, nil); err != nil {
			t.Fatalf("Depend %s on %s: %v", edge.dependent, edge.blocker, err)
		}
		r.drain()
	}
	naming := func() []string {
		return r.strings(`SELECT task_id || '<-' || dependent_id
			FROM tracker_task_dependents
			WHERE task_id = 'blk' OR dependent_id = 'blk'`)
	}
	if got := naming(); len(got) != 2 {
		t.Fatalf("the fixture's mirrors naming blk are %v, want one each way, "+
			"so this case is not the shape it names", got)
	}

	if _, err := r.writer.PurgeTask(t.Context(), "op-purge", "blk", "ENG",
		"a duplicate import"); err != nil {
		t.Fatalf("purge: %v", err)
	}
	r.drain()
	if got := naming(); len(got) != 0 {
		t.Errorf("the purged task is still named by the dependency mirrors %v", got)
	}
}

// A RANK ORDER IS APPLIED AS THE VERSION IT WAS WRITTEN AT SAYS.
//
// A placement that moved only the `rank` column was undone by the task's next
// commit, which rewrites the column from the document; the fix writes the key
// into the document too. But a record is applied by every node on whatever
// build it runs, and again by any node that replays the log — so the fix cannot
// change what an existing record does. A record at the first version keeps the
// column-only placement it was first applied with, and the document placement
// belongs to the version writers now stamp, which an older build retains
// rather than applies the old way.
func TestARankOrderIsAppliedAsTheVersionItWasWrittenAtSays(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	at := time.Unix(1_700_000_100, 0).UTC()
	if _, err := h.apply(taskRecord("t-1", tracker.OpCreate, newTask("t-1"), nil), at); err != nil {
		t.Fatalf("create: %v", err)
	}
	created := h.text(`SELECT json_extract(CAST(document AS TEXT), '$.rank')
		FROM tracker_tasks WHERE id = 't-1'`)
	order := func(v int, op string, rank tracker.Rank) tracker.MutationRecord {
		return tracker.MutationRecord{
			RecordEnvelope: tracker.RecordEnvelope{
				V: v, OpID: op, Subject: tracker.RankOrderSubject("ENG"),
				Op: tracker.OpPatch, Writer: "node-a",
				Scope: tracker.ScopeSet{Terms: []tracker.ScopeTerm{
					{Kind: tracker.TermContainer, ID: "ENG"},
				}},
			},
			Mutation: mustJSON(tracker.RankOrder{
				V: tracker.DocumentVersion, Project: "ENG",
				Placements: []tracker.Placement{{Task: "t-1", Rank: rank}},
			}),
		}
	}
	placed := func() (string, string) {
		return h.text(`SELECT rank FROM tracker_tasks WHERE id = 't-1'`),
			h.text(`SELECT json_extract(CAST(document AS TEXT), '$.rank')
				FROM tracker_tasks WHERE id = 't-1'`)
	}

	if _, err := h.apply(order(tracker.RecordVersion, "op-v1", "a5"), at); err != nil {
		t.Fatalf("apply the first version's placement: %v", err)
	}
	if column, document := placed(); column != "a5" || document != created {
		t.Fatalf("a first-version rank order left the column at %q and the "+
			"document at %q, want %q and the untouched %q — the placement it was "+
			"first applied with", column, document, "a5", created)
	}
	if _, err := h.apply(order(tracker.RankOrderRecordVersion, "op-v2", "a7"), at); err != nil {
		t.Fatalf("apply the current version's placement: %v", err)
	}
	if column, document := placed(); column != "a7" || document != "a7" {
		t.Fatalf("a current rank order left the column at %q and the document "+
			"at %q, want both at %q", column, document, "a7")
	}
}

// EACH RECORD IS WRITTEN AT THE VERSION ITS APPLY MEANS, AND NO HIGHER.
//
// The version is what tells a node which apply a record was written for, so the
// writer has to stamp it — a rank order, a purge and a patch carrying a merge
// target each at their own, and every other record at the first: a record of
// any other kind stamped above what an older build reads would be retained by
// every such node for no change in what it does, together with every later
// record its scope covers.
func TestEachRecordIsWrittenAtTheVersionItsApplyMeans(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.applyWhileWriting()
	filedTask(t, r, "t-1")
	filedTask(t, r, "t-2")
	filedTask(t, r, "t-3")
	filedTask(t, r, "t-4")
	if _, err := r.writer.MoveTask(t.Context(), "op-drop", "ENG", "t-2", "",
		"t-1"); err != nil {
		t.Fatalf("MoveTask: %v", err)
	}
	r.drain()
	if _, err := r.writer.PurgeTask(t.Context(), "op-purge", "t-3", "ENG",
		"filed twice"); err != nil {
		t.Fatalf("PurgeTask: %v", err)
	}
	r.drain()
	if _, err := r.writer.MergeDuplicates(t.Context(), "op-merge", "t-4", "t-1",
		false, nil); err != nil {
		t.Fatalf("MergeDuplicates: %v", err)
	}
	r.drain()

	// EVERY RECORD ON THE LOG, each against the version its apply means:
	// a rank order, a purge and a merge target at their own, everything
	// else at the first.
	want := func(env tracker.RecordEnvelope, payload []byte) int {
		switch {
		case env.Subject.Kind == tracker.KindRankOrder:
			return tracker.RankOrderRecordVersion
		case env.Op == tracker.OpPurge:
			return tracker.PurgeRecordVersion
		}
		record, err := tracker.Decode(payload)
		if err != nil {
			t.Fatalf("decode the %s record on %s: %v", env.Op, env.Subject, err)
		}
		var patch map[string]json.RawMessage
		if env.Op == tracker.OpPatch && json.Unmarshal(record.Mutation, &patch) == nil {
			if _, carries := patch["merge_into"]; carries {
				return tracker.MergeRecordVersion
			}
		}
		return tracker.RecordVersion
	}
	seen := map[int]bool{}
	for seq := uint64(1); seq <= r.consumed; seq++ {
		_, payload, _, ok, err := r.log.At(t.Context(), seq)
		if err != nil || !ok {
			t.Fatalf("read record %d: ok=%v %v", seq, ok, err)
		}
		env, err := tracker.DecodeEnvelope(payload)
		if err != nil {
			t.Fatalf("decode record %d: %v", seq, err)
		}
		if expected := want(env, payload); env.V != expected {
			t.Errorf("the %s %s record at %d was written at version %d, want %d",
				env.Subject.Kind, env.Op, seq, env.V, expected)
		}
		seen[env.V] = true
		if env.Op == tracker.OpPurge {
			// THE PAYLOAD STATES THE SAME NUMBER, because a purge's
			// payload is what a marker at the first version keeps.
			record, err := tracker.Decode(payload)
			if err != nil {
				t.Fatalf("decode the purge: %v", err)
			}
			var body struct {
				V int `json:"v"`
			}
			if err := json.Unmarshal(record.Mutation, &body); err != nil ||
				body.V != tracker.PurgeRecordVersion {
				t.Errorf("the purge's payload states version %d (%v), want %d",
					body.V, err, tracker.PurgeRecordVersion)
			}
		}
	}
	for _, version := range []int{tracker.RecordVersion,
		tracker.RankOrderRecordVersion, tracker.PurgeRecordVersion,
		tracker.MergeRecordVersion} {
		if !seen[version] {
			t.Fatalf("no record on the log was written at version %d, so this "+
				"case is not the shape it names", version)
		}
	}
	// AND AN OLDER BUILD RETAINS A RANK ORDER BUT STOPS AT A PURGE: a rank
	// order installs no gate, so the framework files it for a build that
	// can read it, and a purge does — so a build that cannot read its
	// version halts rather than applying it by an older rule.
	if (tracker.Domain{}).InstallsGate(statelog.Envelope{
		Kind: string(tracker.KindRankOrder), Op: string(tracker.OpPatch),
	}) {
		t.Error("a rank order reads as a gate, so a build that cannot read its " +
			"version would stop its applier rather than retain the record")
	}
	if !(tracker.Domain{}).InstallsGate(statelog.Envelope{
		Kind: string(tracker.KindTask), Op: string(tracker.OpPurge),
	}) {
		t.Error("a purge does not read as a gate, so a build that cannot read " +
			"its version would retain it and go on applying every later record " +
			"about a task its peers destroyed")
	}
	got := (tracker.Domain{}).RecordVersion()
	for _, version := range []int{tracker.RankOrderRecordVersion,
		tracker.PurgeRecordVersion, tracker.MergeRecordVersion} {
		if got < version {
			t.Errorf("the domain declares it reads version %d, below the %d it "+
				"writes", got, version)
		}
	}
}

// appendRecord puts one record on the log exactly as given, which is how a
// case puts there a record this build's own writer no longer writes — a purge
// at the first record version, as every purge was written before
// [tracker.PurgeRecordVersion].
func (r *roundTrip) appendRecord(rec tracker.MutationRecord) {
	r.t.Helper()
	body, err := rec.Encode()
	if err != nil {
		r.t.Fatalf("encode the record: %v", err)
	}
	subject := tracker.Domain{}.Stream().SubjectPrefix + "." + rec.Subject.String()
	if _, _, err := r.log.Append(r.t.Context(), subject, rec.OpID, nil, body); err != nil {
		r.t.Fatalf("append the record: %v", err)
	}
}

// firstVersionPurge is a purge of one task as a writer before
// [tracker.PurgeRecordVersion] wrote it: the record at the first version, its
// payload stating the same.
func firstVersionPurge(id, reason string) tracker.MutationRecord {
	rec := taskRecord(id, tracker.OpPurge, map[string]any{
		"v": tracker.RecordVersion, "reason": reason,
	}, nil)
	rec.Kind = tracker.ChangePurged
	return rec
}

// A PURGE IS APPLIED BY THE RULE OF THE VERSION IT WAS WRITTEN AT.
//
// Every node's copy is derived from the log, so a node that replays it — a
// fresh one, or one restored from a snapshot — has to reach exactly the rows
// its peers hold. A purge at [tracker.PurgeRecordVersion] empties the task's
// history and inbox content, takes the mirror of its dependencies, writes its
// own line, and moves its children's documents with their rows, and no later
// commit writes an edge to it back. A purge at the first version does none of
// those — that is its version's rule — so this build replaying one has to leave
// exactly what that rule leaves: the content, the mirror, and a child's next
// commit writing the purged parent back. Anything else is two copies of one log
// holding different rows for the same record. One fixture, both versions; only
// the purge differs.
func TestAPurgeIsAppliedByTheRuleOfTheVersionItWasWrittenAt(t *testing.T) {
	t.Parallel()
	const (
		title  = "the merger with Contoso"
		remark = "legal says wait for the filing"
		reason = "asked for by legal"
	)
	for _, tc := range []struct {
		name    string
		current bool
	}{{"first version", false}, {"purge version", true}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRoundTrip(t)
			task := newTask("t-1")
			task.Title, task.Assignee = title, "bob"
			if _, err := r.writer.CreateTask(t.Context(), "op-t-1", task, nil); err != nil {
				t.Fatalf("create: %v", err)
			}
			r.drain()
			key := r.strings(`SELECT key FROM tracker_tasks WHERE id = 't-1'`)[0]
			if _, err := r.writer.UpdateTask(t.Context(), "op-comment", "t-1", "ENG",
				tracker.NoIfMatch, tracker.TaskPatch{Comment: &tracker.Comment{
					ID: "cm-1", Task: "t-1", Author: "ana",
					AuthorKind: tracker.AuthorHuman, Body: remark, CreatedAt: wednesday,
				}}, tracker.ChangeComment, &tracker.Notify{
					Kind: tracker.ChangeComment, Excerpt: remark, CommentID: "cm-1",
					Snapshot: tracker.Snapshot{
						Key: key, Project: "ENG", Assignee: "bob",
						CommentAuthorKind: tracker.AuthorHuman,
					},
				}); err != nil {
				t.Fatalf("comment: %v", err)
			}
			r.drain()
			filedTask(t, r, "blk")
			if _, err := r.writer.Depend(t.Context(), "op-dep", tracker.DependencyChange{
				Task: "t-1", Project: "ENG", WaitingOnAdd: []string{"blk"},
			}, nil); err != nil {
				t.Fatalf("Depend: %v", err)
			}
			r.drain()
			parent := "t-1"
			kid := newTask("kid")
			kid.Parent, kid.Depth = &parent, 1
			if _, err := r.writer.CreateTask(t.Context(), "op-kid", kid, nil); err != nil {
				t.Fatalf("create kid: %v", err)
			}
			r.drain()
			mirror := func() int {
				return len(r.strings(`SELECT dependent_id FROM tracker_task_dependents
					WHERE task_id = 'blk' AND dependent_id = 't-1'`))
			}
			if contentRows(r, title) == 0 || contentRows(r, remark) == 0 || mirror() != 1 {
				t.Fatal("the fixture carries no content or no mirror before the " +
					"purge, so this case is not the shape it names")
			}

			if tc.current {
				if _, err := r.writer.PurgeTask(t.Context(), "op-purge", "t-1",
					"ENG", reason); err != nil {
					t.Fatalf("purge: %v", err)
				}
			} else {
				r.appendRecord(firstVersionPurge("t-1", reason))
			}
			r.drain()
			// THE CHILD'S OWN DOCUMENT, which the detail read answers from,
			// before any commit of the child's could rewrite it.
			kidDocument := parentOf(r.task(t, "kid"))
			// A COMMIT ON EACH NEIGHBOUR, neither of which touches the edge
			// or the parent: what each writes is the version's to decide.
			for _, id := range []string{"kid", "blk"} {
				if _, err := r.writer.UpdateTask(t.Context(), "op-touch-"+id, id,
					"ENG", tracker.NoIfMatch,
					tracker.TaskPatch{Title: ptr("touched " + id)},
					tracker.ChangeFields, nil); err != nil {
					t.Fatalf("touch %s: %v", id, err)
				}
				r.drain()
			}

			content := contentRows(r, title) + contentRows(r, remark)
			kidParent := r.strings(`SELECT COALESCE(parent_id, '') FROM tracker_tasks
				WHERE id = 'kid'`)[0]
			var purgeLine string
			marked := 0
			for _, record := range r.activity(tracker.ActivityQuery{Task: key}).Records {
				if record.Kind == tracker.ChangePurged {
					purgeLine = record.Excerpt
				} else if record.ContentPurged {
					marked++
				}
			}
			inbox, err := r.reader.Inbox(t.Context(), tracker.InboxQuery{
				Handle: "bob", IncludeSnoozed: true, Level: statelog.ReadStale,
			}, wednesday)
			if err != nil {
				t.Fatalf("read bob's inbox: %v", err)
			}
			var notice *tracker.InboxNotice
			for i := range inbox.Notices {
				if inbox.Notices[i].SubjectID == "t-1" &&
					inbox.Notices[i].Kind == tracker.ChangeComment {
					notice = &inbox.Notices[i]
				}
			}
			if notice == nil {
				t.Fatal("bob's notice of the comment is gone — a purge of " +
					"either version keeps who was told")
			}

			if tc.current {
				if content != 0 {
					t.Errorf("%d rows still carry the task's content", content)
				}
				if mirror() != 0 {
					t.Error("the blocker's mirror still names the purged task")
				}
				if kidDocument != "" || kidParent != "" {
					t.Errorf("the child's document said parent %q after the purge "+
						"and its next commit wrote %q, want the root the purge "+
						"moved it to in both", kidDocument, kidParent)
				}
				if !strings.Contains(purgeLine, reason) {
					t.Errorf("the purge's own row reads %q, want its line", purgeLine)
				}
				if marked == 0 || !notice.ContentPurged || notice.Excerpt != "" {
					t.Errorf("%d feed rows and bob's notice (%q, content_purged=%v) "+
						"do not say their content was emptied", marked,
						notice.Excerpt, notice.ContentPurged)
				}
				return
			}
			if content == 0 {
				t.Error("a first-version purge emptied the content its version " +
					"left, so this node now differs from every node that applied it")
			}
			if mirror() != 1 {
				t.Error("a first-version purge took the mirror its version left")
			}
			if kidDocument != "t-1" || kidParent != "t-1" {
				t.Errorf("the child's document said parent %q after the purge and "+
					"its next commit wrote %q, want the purged parent in both, as "+
					"the first version's rule leaves them", kidDocument, kidParent)
			}
			if purgeLine != "" {
				t.Errorf("a first-version purge's row reads %q, and that version "+
					"wrote no line on it", purgeLine)
			}
			if marked != 0 || notice.ContentPurged || notice.Excerpt != remark {
				t.Errorf("%d feed rows and bob's notice (%q, content_purged=%v) "+
					"claim content a first-version purge never emptied", marked,
					notice.Excerpt, notice.ContentPurged)
			}
		})
	}
}

// A PURGED TASK IS NOT WRITTEN BACK BY THE NEXT COMMIT ON EITHER END OF AN EDGE.
//
// The purge deletes every row naming the task, but the tasks on the other end
// still carry the edge in their own documents — the dependent its `waiting_on`,
// the blocker its `Dependents` — and a commit rewrites a task's rows from its
// document. So the dependent's next comment put the edge back and it waited, for
// good, on a task whose status can never move.
func TestAPurgedTaskIsNotWrittenBackByTheNextCommitOnEitherEnd(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "dep")
	filedTask(t, r, "blk")
	filedTask(t, r, "later")
	for _, edge := range []struct{ dependent, blocker string }{
		{"dep", "blk"}, {"blk", "later"},
	} {
		if _, err := r.writer.Depend(t.Context(), "op-"+edge.dependent,
			tracker.DependencyChange{
				Task: edge.dependent, Project: "ENG",
				WaitingOnAdd: []string{edge.blocker},
			}, nil); err != nil {
			t.Fatalf("Depend %s on %s: %v", edge.dependent, edge.blocker, err)
		}
		r.drain()
	}
	if _, err := r.writer.PurgeTask(t.Context(), "op-purge", "blk", "ENG",
		"a duplicate import"); err != nil {
		t.Fatalf("purge: %v", err)
	}
	r.drain()

	// A COMMIT ON EACH END, neither of which touches the edge.
	for _, id := range []string{"dep", "later"} {
		if _, err := r.writer.UpdateTask(t.Context(), "op-touch-"+id, id, "ENG",
			tracker.NoIfMatch, tracker.TaskPatch{Title: ptr("touched " + id)},
			tracker.ChangeFields, nil); err != nil {
			t.Fatalf("touch %s: %v", id, err)
		}
		r.drain()
	}
	if got := r.strings(`
		SELECT 'relation ' || task_id || '->' || other_id FROM tracker_relations
		WHERE task_id = 'blk' OR other_id = 'blk'
		UNION ALL
		SELECT 'dependency ' || task_id || '->' || blocker_id FROM tracker_task_deps
		WHERE task_id = 'blk' OR blocker_id = 'blk'
		UNION ALL
		SELECT 'mirror ' || task_id || '<-' || dependent_id FROM tracker_task_dependents
		WHERE task_id = 'blk' OR dependent_id = 'blk'`); len(got) != 0 {
		t.Fatalf("the purged task is named again after a commit on each end: %v", got)
	}
	for _, row := range r.ask(map[string]any{"container": "project:ENG"}).Rows {
		if row.ID == "dep" && row.Blocked {
			t.Fatal("dep reads as blocked by a task that was purged")
		}
	}
}

// A BLOCKER'S CAP COUNTS THE DEPENDENTS THAT ARE STILL THERE.
//
// A purged dependent stays listed in its blocker's own document — the purge
// rewrites no other task's — and a new dependent was refused against that list,
// so a blocker full to the cap with one of its dependents purged could take no
// replacement: "64 tasks already wait on it", of which one no longer exists.
// The gesture resolves from the live edges, and the record it publishes
// carries the list without the purged entry.
func TestABlockersCapCountsTheDependentsThatAreStillThere(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.applyWhileWriting()
	filedTask(t, r, "blk")
	dependents := make([]string, 0, tracker.MaxDependents)
	for i := range tracker.MaxDependents {
		id := fmt.Sprintf("d-%02d", i)
		filedTask(t, r, id)
		dependents = append(dependents, id)
	}
	if _, err := r.writer.Depend(t.Context(), "op-fill", tracker.DependencyChange{
		Task: "blk", Project: "ENG", BlockingAdd: dependents,
	}, nil); err != nil {
		t.Fatalf("fill blk to the cap: %v", err)
	}
	r.drain()
	filedTask(t, r, "new")
	if _, err := r.writer.Depend(t.Context(), "op-over", tracker.DependencyChange{
		Task: "new", Project: "ENG", WaitingOnAdd: []string{"blk"},
	}, nil); err == nil {
		t.Fatal("a blocker at the cap took one more dependent, so this case " +
			"is not the shape it names")
	}

	if _, err := r.writer.PurgeTask(t.Context(), "op-purge", "d-00", "ENG",
		"filed in error"); err != nil {
		t.Fatalf("purge a dependent: %v", err)
	}
	r.drain()
	if _, err := r.writer.Depend(t.Context(), "op-replace", tracker.DependencyChange{
		Task: "new", Project: "ENG", WaitingOnAdd: []string{"blk"},
	}, nil); err != nil {
		t.Fatalf("a blocker whose purged dependent freed a place refused its "+
			"replacement: %v", err)
	}
	r.drain()
	listed := oneTask(t, r, "blk").Dependents
	if len(listed) != tracker.MaxDependents || slices.Contains(listed, "d-00") ||
		!slices.Contains(listed, "new") {
		t.Fatalf("blk's own list holds %d dependents, d-00 present=%v, new "+
			"present=%v — want the cap exactly, the purged one gone and the "+
			"replacement in", len(listed), slices.Contains(listed, "d-00"),
			slices.Contains(listed, "new"))
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
		if _, err := h.apply(taskRecord(id, tracker.OpCreate, newTask(id), nil), at); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	for _, move := range []struct{ id, parent string }{
		{"parent", "grandparent"}, {"child", "parent"},
	} {
		parent := move.parent
		if _, err := h.apply(taskRecord(move.id, tracker.OpPatch,
			tracker.TaskPatch{Parent: &parent}, nil), at); err != nil {
			t.Fatalf("re-parent %s: %v", move.id, err)
		}
	}
	if got := h.value(`SELECT depth FROM tracker_tasks WHERE id = 'child'`); got != 2 {
		t.Fatalf("the fixture's child is at depth %d, so this case is not the "+
			"shape it names", got)
	}

	purge := taskRecord("parent", tracker.OpPurge,
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
	if err := h.db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
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

// A CHILD THAT CROSSED A PURGE LANDS WHERE THE PURGE PUT ITS CHILDREN.
//
// A child is written on its own subject and a purge on its parent's, so the
// broker arbitrates neither against the other: a create under a task, or a move
// onto it, decided on a node that had not yet applied the task's purge lands
// after the purge on the log — and written as it says, it hangs from a row no
// node holds. From [tracker.PurgeRecordVersion] it lands where that purge moved
// the children it found, the purged task's own parent, following the markers
// when that parent was purged too — so either order of the two ends in one
// tree. After a purge at the first version it is written by that version's
// rule: under the parent the record names.
func TestAChildThatCrossedAPurgeLandsWhereThePurgeMovedItsChildren(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		version int
		// alsoGrandparent purges the parent's own parent after it, so
		// the late child has two markers to follow.
		alsoGrandparent bool
		want            string
	}{
		{"first version leaves it under the purged task", tracker.RecordVersion, false, "x"},
		{"purge version moves it onto the purged task's parent", tracker.PurgeRecordVersion, false, "gp"},
		{"purge version follows a purged parent's parent", tracker.PurgeRecordVersion, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newApplyHarness(t)
			at := time.Unix(1_700_000_100, 0).UTC()
			for _, id := range []string{"gp", "x", "mover"} {
				if _, err := h.apply(taskRecord(id, tracker.OpCreate, newTask(id), nil), at); err != nil {
					t.Fatalf("create %s: %v", id, err)
				}
			}
			gp := "gp"
			if _, err := h.apply(taskRecord("x", tracker.OpPatch,
				tracker.TaskPatch{Parent: &gp}, nil), at); err != nil {
				t.Fatalf("file x under gp: %v", err)
			}
			purge := func(id string) {
				t.Helper()
				rec := taskRecord(id, tracker.OpPurge, map[string]any{
					"v": tc.version, "reason": "filed twice",
				}, nil)
				rec.V = tc.version
				if _, err := h.apply(rec, at); err != nil {
					t.Fatalf("purge %s: %v", id, err)
				}
			}
			purge("x")
			if tc.alsoGrandparent {
				purge("gp")
			}

			// THE TWO WRITES THAT CROSS IT: a create under x, and a move
			// onto x, each applied after x's purge.
			x := "x"
			late := newTask("late")
			late.Parent, late.Depth = &x, 1
			if _, err := h.apply(taskRecord("late", tracker.OpCreate, late, nil), at); err != nil {
				t.Fatalf("create late under x: %v", err)
			}
			if _, err := h.apply(taskRecord("mover", tracker.OpPatch,
				tracker.TaskPatch{Parent: &x}, nil), at); err != nil {
				t.Fatalf("move mover onto x: %v", err)
			}
			for _, id := range []string{"late", "mover"} {
				row := h.text(`SELECT COALESCE(parent_id, '') FROM tracker_tasks
					WHERE id = ?`, id)
				document := h.text(`SELECT COALESCE(json_extract(CAST(document AS TEXT),
					'$.parent'), '') FROM tracker_tasks WHERE id = ?`, id)
				if row != tc.want || document != tc.want {
					t.Errorf("%s's parent is %q in its row and %q in its document, "+
						"want %q in both", id, row, document, tc.want)
				}
			}
		})
	}
}

// A CHILD'S RECORD HELD BACK BELOW A PURGE OF ITS PARENT STILL APPLIES.
//
// A node that cannot read a record on a child retains it — and every later
// record on that child — until a build that can arrives. The purge of the
// child's parent is a record on ANOTHER subject, which that retained record's
// scope does not cover, so the node applies the purge first and the child's
// record after it, below the purge's position. The two orders have to end in
// one set of rows: the purge moves the child it finds, and the child's own
// records, whenever they apply, merge onto whatever document the child then
// has. A child row stamped with the purge's position read every child record
// below that position as already applied, so the reprocess wrote a history row
// and nothing else, and that node lacked the change for good.
//
// Mutation: stamp `scoped_through` with the purge's position in
// moveChildDocument and both cases diverge.
func TestAChildRecordHeldBackBelowAPurgeOfItsParentStillApplies(t *testing.T) {
	t.Parallel()
	moved := "y"
	renamed := "renamed while its parent was being purged"
	for _, tc := range []struct {
		name  string
		patch tracker.TaskPatch
	}{
		{"it renames the child, which the purge then moves", tracker.TaskPatch{
			Title: &renamed,
		}},
		{"it moves the child out from under the purged task", tracker.TaskPatch{
			Title: &renamed, Parent: &moved,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			at := time.Unix(1_700_000_100, 0).UTC()
			gp, p := "gp", "p"
			fixture := []tracker.MutationRecord{
				taskRecord("gp", tracker.OpCreate, newTask("gp"), nil),
				taskRecord("p", tracker.OpCreate, newTask("p"), nil),
				taskRecord("y", tracker.OpCreate, newTask("y"), nil),
				taskRecord("c", tracker.OpCreate, newTask("c"), nil),
				taskRecord("p", tracker.OpPatch, tracker.TaskPatch{Parent: &gp}, nil),
				taskRecord("c", tracker.OpPatch, tracker.TaskPatch{Parent: &p}, nil),
			}
			fixture[5].OpID = "c-into-p"
			held := taskRecord("c", tracker.OpPatch, tc.patch, nil)
			held.OpID = "c-held"
			purge := taskRecord("p", tracker.OpPurge, map[string]any{
				"v": tracker.PurgeRecordVersion, "reason": "filed twice",
			}, nil)
			purge.V = tracker.PurgeRecordVersion
			heldAt := uint64(len(fixture) + 1)

			inOrder, deferred := newApplyHarness(t), newApplyHarness(t)
			for _, h := range []*applyHarness{inOrder, deferred} {
				for _, rec := range fixture {
					if _, err := h.apply(rec, at); err != nil {
						t.Fatalf("fixture %s: %v", rec.OpID, err)
					}
				}
			}
			// IN ORDER: the child's record, then the purge.
			if _, err := inOrder.applyAt(held, at, heldAt); err != nil {
				t.Fatalf("apply the child's record: %v", err)
			}
			if _, err := inOrder.applyAt(purge, at, heldAt+1); err != nil {
				t.Fatalf("apply the purge: %v", err)
			}
			// HELD BACK: the purge at its position, then the child's
			// record reprocessed at its own, below it.
			if _, err := deferred.applyAt(purge, at, heldAt+1); err != nil {
				t.Fatalf("apply the purge: %v", err)
			}
			if _, err := deferred.applyAt(held, at, heldAt); err != nil {
				t.Fatalf("reprocess the child's record: %v", err)
			}

			if got := deferred.text(`SELECT title FROM tracker_tasks WHERE id = 'c'`); got != renamed {
				t.Errorf("the reprocessed record did not apply: the child is "+
					"titled %q", got)
			}
			want, got := inOrder.estate(), deferred.estate()
			if !slices.Equal(want, got) {
				t.Errorf("a node that held the child's record back below the "+
					"purge holds different rows from one that applied the two "+
					"in order:\n in order: %v\n held:     %v", want, got)
			}
		})
	}
}

// A FIELD THIS BUILD DOES NOT KNOW SURVIVES EVERY APPLY THAT REWRITES THE
// DOCUMENT: the commit that merges a patch onto it, and the purge that moves
// it to a new parent. Each decodes the stored document and encodes it again,
// and a key with no field would be gone from the node that did it while every
// peer that knows the key keeps it.
//
// Mutation: drop [tracker.Task]'s UnmarshalJSON and the key is gone at the
// first commit.
func TestAFieldThisBuildDoesNotKnowSurvivesACommitAndAPurgeMove(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	at := time.Unix(1_700_000_100, 0).UTC()
	gp, p := "gp", "p"
	for _, rec := range []tracker.MutationRecord{
		taskRecord("gp", tracker.OpCreate, newTask("gp"), nil),
		taskRecord("p", tracker.OpCreate, newTask("p"), nil),
		taskRecord("p", tracker.OpPatch, tracker.TaskPatch{Parent: &gp}, nil),
	} {
		if _, err := h.apply(rec, at); err != nil {
			t.Fatalf("fixture %s: %v", rec.OpID, err)
		}
	}
	child := newTask("c")
	child.Parent = &p
	body, err := json.Marshal(child)
	if err != nil {
		t.Fatalf("encode the child: %v", err)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatalf("decode the child: %v", err)
	}
	document["lane"] = json.RawMessage(`"urgent"`)
	withLane, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("encode the child with its lane: %v", err)
	}
	lane := func(when string) {
		t.Helper()
		if got := h.text(`SELECT json_extract(CAST(document AS TEXT), '$.lane')
			FROM tracker_tasks WHERE id = 'c'`); got != "urgent" {
			t.Errorf("after %s the child's document holds lane %q, want the "+
				"newer build's value kept", when, got)
		}
	}
	if _, err := h.apply(taskRecord("c", tracker.OpCreate,
		json.RawMessage(withLane), nil), at); err != nil {
		t.Fatalf("create the child: %v", err)
	}
	lane("its create")
	renamed := "renamed"
	patch := taskRecord("c", tracker.OpPatch, tracker.TaskPatch{Title: &renamed}, nil)
	patch.OpID = "c-rename"
	if _, err := h.apply(patch, at); err != nil {
		t.Fatalf("rename the child: %v", err)
	}
	lane("a commit on it")
	purge := taskRecord("p", tracker.OpPurge, map[string]any{
		"v": tracker.PurgeRecordVersion, "reason": "filed twice",
	}, nil)
	purge.V = tracker.PurgeRecordVersion
	if _, err := h.apply(purge, at); err != nil {
		t.Fatalf("purge the parent: %v", err)
	}
	if got := h.text(`SELECT parent_id FROM tracker_tasks WHERE id = 'c'`); got != "gp" {
		t.Fatalf("the purge left the child under %q, so it did not move it and "+
			"this case is not the shape it names", got)
	}
	lane("the purge of its parent moved it")
}

// estate is every row a task commit derives, rendered for comparison between
// two nodes: the task rows whole, documents included, and the ancestry.
func (h *applyHarness) estate() []string {
	h.t.Helper()
	var out []string
	for _, query := range []string{
		`SELECT id || '|' || COALESCE(parent_id, '') || '|' || root_id || '|' ||
		        depth || '|' || title || '|' || version || '|' || scoped_through ||
		        '|' || CAST(document AS TEXT)
		 FROM tracker_tasks ORDER BY id`,
		`SELECT ancestor_id || '>' || descendant_id || '@' || distance
		 FROM tracker_task_closure ORDER BY ancestor_id, descendant_id`,
	} {
		if err := h.db.Replicated().Read(h.t.Context(), func(tx *sql.Tx) error {
			rows, err := tx.QueryContext(h.t.Context(), query)
			if err != nil {
				return err
			}
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var row string
				if err := rows.Scan(&row); err != nil {
					return err
				}
				out = append(out, row)
			}
			return rows.Err()
		}); err != nil {
			h.t.Fatalf("read the estate: %v", err)
		}
	}
	return out
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

// A RE-PARENT REBUILDS THE WHOLE SUBTREE, not the first thousand of it.
//
// `descendantsOf` carried a `LIMIT MaxDescendants` on the claim that the cap
// bounded the subtree, and nothing enforces that cap where a subtree GROWS:
// only a move and a removal check it, and a create under a parent never does.
// So a subtree grown one task at a time past the cap hit that limit INSIDE THE
// APPLIER, and the tail it cut kept its old root_id, depth and too_deep — for
// ever, since nothing revisits a task whose own record did not change.
//
// Every node computed the same wrong answer identically, so nothing could
// notice it: a board's `too_deep` flag was derived from a depth that was never
// updated, and a re-parent left half a subtree filed under the root it came
// from. A short read in an applier is not a short answer, it is durable wrong
// state replicated to the fleet.
func TestAReParentRebuildsEveryDescendantAndNotJustTheFirstThousand(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	at := time.Unix(1_700_000_100, 0).UTC()

	// One root with MaxDescendants+2 direct children, which is the cheapest
	// shape past the bound: a deep chain would hit MaxDepth's own flag and
	// confuse what this case is about.
	const root, newHome = "root", "elsewhere"
	over := tracker.MaxDescendants + 2
	for _, id := range []string{newHome, root} {
		if _, err := h.apply(taskRecord(id, tracker.OpCreate, newTask(id), nil), at); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	// SEEDED THROUGH THE APPLIER for the first and last child so the shape
	// is one a real log produces, and directly for the bulk in between so
	// the case costs a few statements rather than a thousand records. The
	// two that matter are the ones the assertions read.
	first, last := "child-0000", fmt.Sprintf("child-%04d", over-1)
	for _, id := range []string{first, last} {
		task := newTask(id)
		parent := root
		task.Parent = &parent
		if _, err := h.apply(taskRecord(id, tracker.OpCreate, task, nil), at); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	h.seedChildren(root, over)

	if got := h.count("tracker_task_closure"); got <= tracker.MaxDescendants {
		t.Fatalf("the fixture holds %d closure rows, which is inside the bound "+
			"this case is about", got)
	}

	// THE ROOT MOVES. Every descendant's ancestry moves with it.
	into := newHome
	if _, err := h.apply(taskRecord(root, tracker.OpPatch,
		tracker.TaskPatch{Parent: &into}, nil), at); err != nil {
		t.Fatalf("re-parent %s: %v", root, err)
	}

	for _, id := range []string{first, last} {
		if got := h.text(
			`SELECT root_id FROM tracker_tasks WHERE id = ?`, id); got != newHome {
			t.Errorf("%s is still filed under %q after its root moved to %q — "+
				"the applier rebuilt only part of the subtree", id, got, newHome)
		}
		if got := h.value(
			`SELECT depth FROM tracker_tasks WHERE id = ?`, id); got != 2 {
			t.Errorf("%s is at depth %d rather than 2", id, got)
		}
		if got := h.value(
			`SELECT distance FROM tracker_task_closure
			 WHERE ancestor_id = ? AND descendant_id = ?`, newHome, id); got != 2 {
			t.Errorf("the closure puts %s %d from the new root", id, got)
		}
	}
}

package tracker_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A RANK ORDER AND A PURGE ARE WRITTEN AT THE VERSION THAT CHANGED THEIR APPLY.
//
// Both kept their shape at version 4 and changed what their apply does to the
// OTHER tasks they touch: a rank order writes each moved task's document, and
// a purge takes itself out of every task that names it. Written at version 1,
// a build from before the change would decode either and apply it the old way,
// and the rows every node is supposed to hold identically would differ between
// the two builds for ever.
func TestARankOrderAndAPurgeAreWrittenAtTheVersionThatChangedTheirApply(t *testing.T) {
	t.Parallel()
	if got := (tracker.Domain{}).RecordVersion(); got < 4 {
		t.Fatalf("this build reads record version %d, want at least the 4 that "+
			"changed the rank order's and the purge's apply", got)
	}
	r := newRoundTrip(t)
	for _, id := range []string{"t-1", "t-2"} {
		if _, err := r.writer.CreateTask(t.Context(), "op-"+id, newTask(id), nil); err != nil {
			t.Fatalf("CreateTask %s: %v", id, err)
		}
		r.drain()
	}
	moved, err := tracker.KeyBetween(oneTask(t, r, "t-1").Rank, oneTask(t, r, "t-2").Rank)
	if err != nil {
		t.Fatalf("a key between the two cards: %v", err)
	}
	start := r.logEnd(t)
	if _, err := r.writer.MoveTasks(t.Context(), "op-move", "ENG",
		[]tracker.Placement{{Task: "t-2", Rank: moved}}); err != nil {
		t.Fatalf("MoveTasks: %v", err)
	}
	r.drain()
	operator := r.writer.As("ops-1", tracker.AuthorOperator, tracker.Provenance{})
	if _, err := operator.PurgeTask(t.Context(), "op-purge", "t-1", "ENG",
		"a duplicate import"); err != nil {
		t.Fatalf("PurgeTask: %v", err)
	}
	r.drain()

	seen := map[tracker.ObjectKind]int{}
	for seq := start + 1; seq <= r.logEnd(t); seq++ {
		rec := r.recordAt(t, seq)
		switch {
		case rec.Subject.Kind == tracker.KindRankOrder, rec.Op == tracker.OpPurge:
			seen[rec.Subject.Kind]++
			if rec.V != 4 {
				t.Errorf("the %s record on %s carries version %d, want 4 — a build "+
					"from before version 4 would apply it by the old rule", rec.Op,
					rec.Subject, rec.V)
			}
		}
	}
	if seen[tracker.KindRankOrder] != 1 || seen[tracker.KindTask] != 1 {
		t.Errorf("found %v, want one rank order and one purge — this case is not the "+
			"shape it names", seen)
	}
}

// A VERSION-1 RANK ORDER IS APPLIED BY VERSION 1'S RULE, AND A VERSION-4 ONE BY
// VERSION 4'S.
//
// Every build before version 4 wrote a placement to the rank COLUMN alone and
// wherever the task was filed; version 4 writes the moved task's document too,
// and only for a task in the order's project. A node that replays a version-1
// order from the log — after adopting a snapshot, say — must write what every
// node that applied it at the time wrote, or it holds rows nobody else does.
func TestARankOrderIsAppliedByItsOwnVersionsRule(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		version int
		// what the ENG task's document and the OPS task's column hold after
		ownDocumentMoves, elsewhereMoves bool
	}{
		{version: 1, ownDocumentMoves: false, elsewhereMoves: true},
		{version: 4, ownDocumentMoves: true, elsewhereMoves: false},
	} {
		t.Run("version "+itoa(tc.version), func(t *testing.T) {
			t.Parallel()
			h := newApplyHarness(t)
			at := time.Unix(1_700_000_100, 0).UTC()
			own, elsewhere := newTask("t-eng"), newTask("t-ops")
			elsewhere.Project, elsewhere.Key = "OPS", "OPS-1"
			for _, task := range []tracker.Task{own, elsewhere} {
				if _, err := h.apply(taskRecord(task.ID, tracker.OpCreate, task, nil), at); err != nil {
					t.Fatalf("create %s: %v", task.ID, err)
				}
			}
			placed, err := tracker.KeyBetween(tracker.RankOrigin, "")
			if err != nil || placed == tracker.RankOrigin {
				t.Fatalf("a key other than the origin: %q, %v", placed, err)
			}
			order := taskRecord("ENG", tracker.OpPatch, tracker.RankOrder{
				V: tracker.DocumentVersion, Project: "ENG",
				Placements: []tracker.Placement{
					{Task: own.ID, Rank: placed}, {Task: elsewhere.ID, Rank: placed},
				},
			}, nil)
			order.V, order.Subject = tc.version, tracker.RankOrderSubject("ENG")
			order.Scope = tracker.ScopeSet{Terms: []tracker.ScopeTerm{
				{Kind: tracker.TermContainer, ID: "ENG"},
			}}
			if _, err := h.apply(order, at); err != nil {
				t.Fatalf("apply the rank order: %v", err)
			}

			if got := rankColumn(t, h, own.ID); got != string(placed) {
				t.Errorf("the ENG task's rank column is %q, want %q — every version "+
					"moves the column", got, placed)
			}
			if got := documentOf(t, h, own.ID).Rank; (got == placed) != tc.ownDocumentMoves {
				t.Errorf("the ENG task's document says rank %q; want it moved = %v at "+
					"version %d", got, tc.ownDocumentMoves, tc.version)
			}
			if got := rankColumn(t, h, elsewhere.ID); (got == string(placed)) != tc.elsewhereMoves {
				t.Errorf("the OPS task's rank column is %q; want it moved = %v at "+
					"version %d", got, tc.elsewhereMoves, tc.version)
			}
		})
	}
}

// A VERSION-1 PURGE IS APPLIED BY VERSION 1'S RULE, AND A VERSION-4 ONE BY
// VERSION 4'S.
//
// Every build before version 4 fixed the rows naming the purged task and left
// the other tasks' documents alone; version 4 takes the purged task out of each
// document too. Both are exactly what their version's nodes hold, so a replay
// of either must write the same.
func TestAPurgeIsAppliedByItsOwnVersionsRule(t *testing.T) {
	t.Parallel()
	for _, version := range []int{1, 4} {
		t.Run("version "+itoa(version), func(t *testing.T) {
			t.Parallel()
			h := newApplyHarness(t)
			at := time.Unix(1_700_000_100, 0).UTC()
			for _, id := range []string{"purged", "child", "dependent"} {
				if _, err := h.apply(taskRecord(id, tracker.OpCreate, newTask(id), nil), at); err != nil {
					t.Fatalf("create %s: %v", id, err)
				}
			}
			parent := "purged"
			if _, err := h.apply(taskRecord("child", tracker.OpPatch,
				tracker.TaskPatch{Parent: &parent}, nil), at); err != nil {
				t.Fatalf("re-parent the child: %v", err)
			}
			if _, err := h.apply(taskRecord("dependent", tracker.OpPatch, tracker.TaskPatch{
				Relations: &[]tracker.Relation{{Kind: tracker.RelationWaitingOn, Other: "purged"}},
			}, nil), at); err != nil {
				t.Fatalf("make the dependent wait: %v", err)
			}

			purge := taskRecord("purged", tracker.OpPurge,
				map[string]any{"v": tracker.GateRecordVersion, "reason": "a duplicate import"}, nil)
			purge.V = version
			if _, err := h.apply(purge, at); err != nil {
				t.Fatalf("purge: %v", err)
			}

			// THE ROWS MOVE AT EVERY VERSION.
			if got := h.value(`SELECT COUNT(*) FROM tracker_tasks
				WHERE id = 'child' AND parent_id IS NULL`); got != 1 {
				t.Errorf("the child's pointer did not move onto the purged task's own "+
					"parent (none) at version %d", version)
			}
			if got := h.value(`SELECT COUNT(*) FROM tracker_task_deps
				WHERE blocker_id = 'purged'`); got != 0 {
				t.Errorf("%d dependency rows still name the purged task at version %d",
					got, version)
			}
			// AND THE DOCUMENTS ONLY FROM VERSION 4.
			rewritten := version >= 4
			if child := documentOf(t, h, "child"); (child.Parent == nil) != rewritten {
				t.Errorf("the child's document names parent %v at version %d; want it "+
					"rewritten = %v", child.Parent, version, rewritten)
			}
			waits := slices.ContainsFunc(documentOf(t, h, "dependent").Relations,
				func(r tracker.Relation) bool { return r.Other == "purged" })
			if waits == rewritten {
				t.Errorf("the dependent's document still waits on the purged task = %v "+
					"at version %d; want it rewritten = %v", waits, version, rewritten)
			}
		})
	}
}

// A BUILD FROM BEFORE VERSION 4 RETAINS A VERSION-4 RANK ORDER AND HALTS AT A
// VERSION-4 PURGE — through the real framework loop, over the real log.
//
// Neither may be applied by that build's rule, which is the old one. A rank
// order is an ordinary record, so it is retained until the node upgrades; a
// purge installs a gate, and a gate a build cannot apply is a stop rather than
// a deferral, because a deferred gate licenses every record above it.
func TestABuildBeforeVersionFourRetainsARankOrderAndHaltsAtAPurge(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for _, id := range []string{"t-1", "t-2"} {
		if _, err := r.writer.CreateTask(t.Context(), "op-"+id, newTask(id), nil); err != nil {
			t.Fatalf("CreateTask %s: %v", id, err)
		}
		r.drain()
	}
	created := oneTask(t, r, "t-2").Rank
	moved, err := tracker.KeyBetween(oneTask(t, r, "t-1").Rank, created)
	if err != nil {
		t.Fatalf("a key between the two cards: %v", err)
	}
	if _, err := r.writer.MoveTasks(t.Context(), "op-move", "ENG",
		[]tracker.Placement{{Task: "t-2", Rank: moved}}); err != nil {
		t.Fatalf("MoveTasks: %v", err)
	}
	r.drain()
	ordered := r.logEnd(t)

	olderNode, older := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "older.db"), store.Options{}, 1)
	t.Cleanup(func() { _ = olderNode.Close() })
	runner, err := statelog.NewRunner(statelog.RunnerDeps{
		Domain:  olderTracker{reads: 3},
		Spec:    statelog.EstateStream(olderTracker{reads: 3}),
		Applier: tracker.NewApplier("node-older"),
		Fetch:   &trackerLogFetch{log: r.log, next: 1},
		Log:     r.log,
		Node:    olderNode,
		DB:      older,
	})
	if err != nil {
		t.Fatalf("build the older node's applier: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	deadline := time.Now().Add(20 * time.Second)
	for runner.Committed().Seq < ordered {
		if time.Now().After(deadline) {
			t.Fatalf("the older node reached %d of %d", runner.Committed().Seq, ordered)
		}
		time.Sleep(20 * time.Millisecond)
	}
	value := func(query string) string {
		t.Helper()
		var v string
		if err := older.Read(t.Context(), func(tx *sql.Tx) error {
			return tx.QueryRowContext(t.Context(), query).Scan(&v)
		}); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return v
	}
	if got := value(`SELECT COUNT(*) FROM tracker_log_deferred`); got != "1" {
		t.Errorf("the older node retained %s record(s), want the rank order", got)
	}
	if got := value(`SELECT rank FROM tracker_tasks WHERE id = 't-2'`); got != string(created) {
		t.Errorf("the older node moved t-2 to %q by its own rule, want it left at %q "+
			"until it can read the order", got, created)
	}

	operator := r.writer.As("ops-1", tracker.AuthorOperator, tracker.Provenance{})
	if _, err := operator.PurgeTask(t.Context(), "op-purge", "t-1", "ENG",
		"a duplicate import"); err != nil {
		t.Fatalf("PurgeTask: %v", err)
	}
	r.drain()
	select {
	case err := <-done:
		if !errors.Is(err, statelog.ErrStopped) {
			t.Fatalf("the older node's loop ended with %v, want it stopped at the purge", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the older node went on past a purge it cannot apply")
	}
	if got := value(`SELECT COUNT(*) FROM tracker_tasks WHERE id = 't-1'`); got != "1" {
		t.Errorf("the older node applied the purge it halted at (%s rows of t-1)", got)
	}
}

// olderTracker is this domain as a build that reads only up to one record
// version sees it.
type olderTracker struct {
	tracker.Domain
	reads int
}

func (o olderTracker) RecordVersion() int { return o.reads }

// rankColumn reads one task's rank column.
func rankColumn(t *testing.T, h *applyHarness, id string) string {
	t.Helper()
	var rank string
	if err := h.db.Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(),
			`SELECT rank FROM tracker_tasks WHERE id = ?`, id).Scan(&rank)
	}); err != nil {
		t.Fatalf("read %s's rank: %v", id, err)
	}
	return rank
}

// documentOf decodes one task's stored document.
func documentOf(t *testing.T, h *applyHarness, id string) tracker.Task {
	t.Helper()
	var document []byte
	if err := h.db.Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(),
			`SELECT document FROM tracker_tasks WHERE id = ?`, id).Scan(&document)
	}); err != nil {
		t.Fatalf("read %s's document: %v", id, err)
	}
	var task tracker.Task
	if err := json.Unmarshal(document, &task); err != nil {
		t.Fatalf("decode %s's document: %v", id, err)
	}
	return task
}

// A REWRITE OF ANOTHER TASK MOVES ONLY THE COLUMNS ITS CHANGE IS ABOUT.
//
// A task can hold a column its document disagrees with, because the version-1
// rules wrote columns alone: a rank order moved the rank column, a purge moved
// a child's pointer. A later purge that merely unlinks such a task rewrites its
// document — and re-deriving every column from that document put the purged
// parent back under the task and undid the drag, for a change about neither.
func TestARewriteMovesOnlyTheColumnsItsChangeIsAbout(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	at := time.Unix(1_700_000_100, 0).UTC()
	for _, id := range []string{"old-parent", "task", "unlinked"} {
		if _, err := h.apply(taskRecord(id, tracker.OpCreate, newTask(id), nil), at); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	parent := "old-parent"
	if _, err := h.apply(taskRecord("task", tracker.OpPatch, tracker.TaskPatch{
		Parent:    &parent,
		Relations: &[]tracker.Relation{{Kind: tracker.RelationLinked, Other: "unlinked"}},
	}, nil), at); err != nil {
		t.Fatalf("file the task under its parent and link it: %v", err)
	}
	// THE VERSION-1 RULES, which leave the task's columns ahead of its
	// document: its pointer moved off the purged parent, its rank dragged.
	purge := taskRecord("old-parent", tracker.OpPurge,
		map[string]any{"v": tracker.GateRecordVersion, "reason": "gone"}, nil)
	purge.V = 1
	if _, err := h.apply(purge, at); err != nil {
		t.Fatalf("the version-1 purge: %v", err)
	}
	dragged, err := tracker.KeyBetween(tracker.RankOrigin, "")
	if err != nil {
		t.Fatalf("a key: %v", err)
	}
	order := taskRecord("ENG", tracker.OpPatch, tracker.RankOrder{
		V: tracker.DocumentVersion, Project: "ENG",
		Placements: []tracker.Placement{{Task: "task", Rank: dragged}},
	}, nil)
	order.V, order.Subject = 1, tracker.RankOrderSubject("ENG")
	order.Scope = tracker.ScopeSet{Terms: []tracker.ScopeTerm{{Kind: tracker.TermContainer, ID: "ENG"}}}
	if _, err := h.apply(order, at); err != nil {
		t.Fatalf("the version-1 rank order: %v", err)
	}
	if doc := documentOf(t, h, "task"); doc.Parent == nil || doc.Rank == dragged {
		t.Fatalf("the premise: the task's document should still name its old parent and "+
			"rank, and holds %v and %q", doc.Parent, doc.Rank)
	}

	// A VERSION-4 PURGE THAT ONLY UNLINKS IT.
	unlink := taskRecord("unlinked", tracker.OpPurge,
		map[string]any{"v": tracker.GateRecordVersion, "reason": "gone"}, nil)
	if _, err := h.apply(unlink, at); err != nil {
		t.Fatalf("the version-4 purge: %v", err)
	}
	if doc := documentOf(t, h, "task"); len(doc.Relations) != 0 {
		t.Fatalf("the premise: the purge rewrote the task's document, and it still "+
			"holds %+v", doc.Relations)
	}
	if got := h.value(`SELECT COUNT(*) FROM tracker_tasks
		WHERE id = 'task' AND parent_id IS NULL`); got != 1 {
		t.Error("a purge that only unlinked the task put its purged parent back " +
			"under it — a pointer to a task that no longer exists")
	}
	if got := rankColumn(t, h, "task"); got != string(dragged) {
		t.Errorf("a purge that only unlinked the task moved its rank to %q, want the "+
			"dragged %q", got, dragged)
	}
}

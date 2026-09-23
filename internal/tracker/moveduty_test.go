package tracker_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A RETRY THAT READ THE ROOT BEFORE THE FIRST RUN'S MOVE OF IT HAD APPLIED
// finishes the walk, and answers with the key the first run gave the root.
//
// The retry finds the root still in its old project, so it takes the path of a
// move that never started: it mints a range of its own and asks for the root's
// step — which the ledger answers with the first run's record once this node
// has applied it. The root's key is that record's number, read off its row;
// this call's range is the gap, and the descendant the first run never reached
// takes a number from it. The path had no test at all.
func TestARetryThatReadTheRootBeforeItsMoveAppliedFinishesTheWalk(t *testing.T) {
	t.Parallel()
	r := moveFixture(t, "m-kid-a", "m-kid-b")
	// NOTHING THE FIRST RUN PUBLISHES APPLIES DURING IT.
	r.applyOnlyOnDrain()
	lossy, log := r.lossyWriter(t)
	op := statelog.NewOpID(time.Now(), "move")
	log.refuse("m-kid-b")
	if _, err := lossy.MoveTaskToProject(t.Context(), op, "m-root", "OPS", nil); err == nil {
		t.Fatal("a move refused on a descendant reported success")
	}
	log.refuse("")
	if got := oneTask(t, r, "m-root"); got.Project != "ENG" {
		t.Fatalf("the premise: the root reads %q on this node before the retry, "+
			"want ENG — its move is on the log and not applied", got.Project)
	}
	end := r.logEnd(t)

	r.applyWhileWriting()
	retry, err := lossy.MoveTaskToProject(t.Context(), op, "m-root", "OPS", nil)
	if err != nil {
		t.Fatalf("the retry: %v", err)
	}
	r.drain()
	root := oneTask(t, r, "m-root")
	if retry.Outcome != statelog.OutcomeApplied || !retry.Collapsed ||
		retry.Key != root.Key {
		t.Errorf("the retry = %+v key %q, want applied as the root's own key %s",
			retry.Result, retry.Key, root.Key)
	}
	// THE FIRST RUN'S RANGE WAS OPS-1..3 AND THE RETRY'S OPS-4..6: the root
	// and the child the first run carried keep their numbers, and the child
	// it never reached takes the retry's third.
	for id, want := range map[string]string{
		"m-root": "OPS-1", "m-kid-a": "OPS-2", "m-kid-b": "OPS-6",
	} {
		if got := oneTask(t, r, id); got.Project != "OPS" || got.Key != want {
			t.Errorf("%s is %s in %q, want %s in OPS", id, got.Key, got.Project, want)
		}
	}
	if root.Moving {
		t.Error("the root is still marked mid-move after its walk finished")
	}
	if got := r.logEnd(t); got != end+3 {
		t.Errorf("the retry put %d record(s) on the log, want 3 — its own "+
			"range, m-kid-b's move and the mark coming down", got-end)
	}
}

// A MOVE THAT STOPPED AND WAS NEVER RE-RUN IS FINISHED BY THE DUTY.
//
// Nothing re-runs a gesture its caller did not retry — a turn that crashed, a
// process that was killed, a descendant's append a full stream refused — and a
// re-run was the only repair there was, so the subtree stayed split across two
// projects for good. The root's own move now marks it, and the duty finishes
// what the mark says is unfinished.
func TestTheDutyFinishesAMoveNobodyReran(t *testing.T) {
	t.Parallel()
	r := moveFixture(t, "m-kid-a", "m-kid-b")
	op := stopMoveAt(t, r, "m-kid-b")
	root, carried := oneTask(t, r, "m-root"), oneTask(t, r, "m-kid-a")
	if !root.Moving {
		t.Fatal("the stopped walk left its root unmarked, so nothing can know " +
			"the subtree is split")
	}

	swept, err := trackerWorker(t, r).Tick(t.Context())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := swept["tracker_abandoned_moves"]; got != 1 {
		t.Fatalf("the duty finished %d move(s), want 1: %v", got, swept)
	}
	r.drain()
	left := oneTask(t, r, "m-kid-b")
	if left.Project != "OPS" || !strings.HasPrefix(left.Key, "OPS-") {
		t.Errorf("m-kid-b is %s in %q after the duty, want an OPS key in OPS",
			left.Key, left.Project)
	}
	if got := oneTask(t, r, "m-kid-a"); got.Key != carried.Key {
		t.Errorf("m-kid-a was re-keyed from %s to %s", carried.Key, got.Key)
	}
	after := oneTask(t, r, "m-root")
	if after.Moving || after.Key != root.Key {
		t.Errorf("the root is %s, marked %v, want %s with its mark down",
			after.Key, after.Moving, root.Key)
	}
	if trackerGate(t, r, "tracker_abandoned_moves") {
		t.Error("the gate still reports a move to finish, so this job runs on " +
			"every tick for ever")
	}

	// AND THE CALLER'S OWN RE-RUN, arriving after the duty finished its
	// walk, is answered with nothing to do — the mark it would take down is
	// already down, which is an empty decision rather than a history row
	// saying nothing moved.
	end := r.logEnd(t)
	retry, err := r.writer.MoveTaskToProject(t.Context(), op, "m-root", "OPS", nil)
	if err != nil || retry.Outcome != statelog.OutcomeApplied {
		t.Fatalf("the caller's re-run after the duty = (%+v, %v), want applied",
			retry.Result, err)
	}
	if got := r.logEnd(t); got != end {
		t.Errorf("the re-run put %d record(s) on the log for a walk the duty "+
			"had finished", got-end)
	}
}

// THE DUTY FINISHES ONLY A MOVE NOBODY IS WALKING, for the reason the merge's
// own case gives: a root is marked for the whole of a LIVE move, and only the
// walk's claim says it was abandoned.
//
// And a move left alone does not hold up the next one.
func TestTheDutyLeavesAMoveWhoseWalkHoldsItsClaim(t *testing.T) {
	t.Parallel()
	r := moveFixture(t, "m-kid-a", "m-kid-b")
	stopMoveAt(t, r, "m-kid-b")
	// A SECOND ABANDONED MOVE, later in id order than the held one.
	if _, err := r.writer.CreateTask(t.Context(), "op-n-root", newTask("n-root"), nil); err != nil {
		t.Fatalf("file n-root: %v", err)
	}
	r.drain()
	parent := "n-root"
	for _, id := range []string{"n-kid-a", "n-kid-b"} {
		kid := newTask(id)
		kid.Parent, kid.Depth = &parent, 1
		if _, err := r.writer.CreateTask(t.Context(), "op-"+id, kid, nil); err != nil {
			t.Fatalf("file %s: %v", id, err)
		}
		r.drain()
	}
	stopMoveOf(t, r, "n-root", "n-kid-b")
	lease, err := r.claims.TryAcquire(t.Context(), tracker.MoveClaim("m-root"),
		coord.AcquireOptions{Owner: "node-b", TTL: tracker.ClaimTTL})
	if err != nil || lease == nil {
		t.Fatalf("hold the running walk's claim: (%v, %v)", lease, err)
	}

	swept, err := trackerWorker(t, r).Tick(t.Context())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := swept["tracker_abandoned_moves"]; got != 1 {
		t.Errorf("the duty finished %d move(s), want n-root's alone", got)
	}
	r.drain()
	if !oneTask(t, r, "m-root").Moving || oneTask(t, r, "m-kid-b").Project != "ENG" {
		t.Error("the duty finished a move whose walk is running")
	}
	if oneTask(t, r, "n-kid-b").Project != "OPS" || oneTask(t, r, "n-root").Moving {
		t.Error("the move nobody holds was not finished — one held walk stopped " +
			"the duty finishing the rest")
	}

	if _, err := r.claims.Release(t.Context(), tracker.MoveClaim("m-root"),
		"node-b", lease.Epoch); err != nil {
		t.Fatalf("release the claim: %v", err)
	}
	if swept, err = trackerWorker(t, r).Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := swept["tracker_abandoned_moves"]; got != 1 {
		t.Errorf("the duty finished %d move(s) once the claim lapsed, want 1", got)
	}
	r.drain()
	if oneTask(t, r, "m-kid-b").Project != "OPS" || oneTask(t, r, "m-root").Moving {
		t.Error("the abandoned move is still split after its claim lapsed")
	}
}

// A MOVE REFUSES A SUBTREE WITH A TASK IN THE TRASH, before its first append.
//
// A tombstoned task refuses every write, so its step stopped the walk — and
// the re-run, and the duty behind both — on the same task for good, with the
// subtree split around it.
func TestAMoveRefusesASubtreeWithATaskInTheTrash(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, removed, want string }{
		{"a removed descendant", "m-kid", "trash"},
		{"a removed root", "m-root", "restore it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := moveFixture(t, "m-kid")
			if _, err := r.writer.RemoveTask(t.Context(), "op-remove", tc.removed,
				"ENG", false, nil); err != nil {
				t.Fatalf("RemoveTask: %v", err)
			}
			r.drain()
			end := r.logEnd(t)
			_, err := r.writer.MoveTaskToProject(t.Context(),
				statelog.NewOpID(time.Now(), "move"), "m-root", "OPS", nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("the move = %v, want a refusal saying %q", err, tc.want)
			}
			if got := r.logEnd(t); got != end {
				t.Errorf("the refused move put %d record(s) on the log", got-end)
			}
		})
	}
}

// A DESCENDANT REMOVED WHILE THE WALK RAN IS WAITED FOR, never walked around.
//
// It is frozen, so nothing can carry it; taking the mark down around it would
// leave it in the old project for good, under a root in the new one. The mark
// stays up, the rest moves, and its restore is what lets the next pass finish.
func TestAMoveWaitsForADescendantRemovedWhileItRan(t *testing.T) {
	t.Parallel()
	r := moveFixture(t, "m-kid-a", "m-kid-b", "m-kid-c")
	stopMoveAt(t, r, "m-kid-b")
	if _, err := r.writer.RemoveTask(t.Context(), "op-remove", "m-kid-b", "ENG",
		false, nil); err != nil {
		t.Fatalf("RemoveTask: %v", err)
	}
	r.drain()

	swept, err := trackerWorker(t, r).Tick(t.Context())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := swept["tracker_abandoned_moves"]; got != 0 {
		t.Errorf("the duty reports %d move(s) finished with a descendant still "+
			"in the trash in the old project", got)
	}
	r.drain()
	if !oneTask(t, r, "m-root").Moving {
		t.Error("the mark came down around a descendant the move never carried")
	}
	if got := oneTask(t, r, "m-kid-c"); got.Project != "OPS" {
		t.Errorf("m-kid-c is in %q — the walk stopped at the removed task rather "+
			"than carrying everything it could", got.Project)
	}

	if _, err := r.writer.RestoreTask(t.Context(), "op-restore", "m-kid-b", "ENG",
		nil); err != nil {
		t.Fatalf("RestoreTask: %v", err)
	}
	r.drain()
	if swept, err = trackerWorker(t, r).Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := swept["tracker_abandoned_moves"]; got != 1 {
		t.Errorf("the duty finished %d move(s) after the restore, want 1", got)
	}
	r.drain()
	if got := oneTask(t, r, "m-kid-b"); got.Project != "OPS" || oneTask(t, r, "m-root").Moving {
		t.Errorf("after the restore m-kid-b is in %q and the root marked %v",
			got.Project, oneTask(t, r, "m-root").Moving)
	}
}

// A ROOT REMOVED MID-MOVE WAITS FOR ITS RESTORE, and costs no tick until then:
// it is frozen, so the mark could not come down anyway.
func TestARemovedRootMidMoveWaitsForItsRestore(t *testing.T) {
	t.Parallel()
	r := moveFixture(t, "m-kid-a", "m-kid-b")
	stopMoveAt(t, r, "m-kid-b")
	if _, err := r.writer.RemoveTask(t.Context(), "op-remove", "m-root", "OPS",
		false, nil); err != nil {
		t.Fatalf("RemoveTask: %v", err)
	}
	r.drain()
	if trackerGate(t, r, "tracker_abandoned_moves") {
		t.Fatal("the gate reports a move to finish when its root is in the trash " +
			"— the job runs on every tick until somebody restores it")
	}

	if _, err := r.writer.RestoreTask(t.Context(), "op-restore", "m-root", "OPS",
		nil); err != nil {
		t.Fatalf("RestoreTask: %v", err)
	}
	r.drain()
	swept, err := trackerWorker(t, r).Tick(t.Context())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := swept["tracker_abandoned_moves"]; got != 1 {
		t.Errorf("the duty finished %d move(s) after the restore, want 1", got)
	}
	r.drain()
	if oneTask(t, r, "m-kid-b").Project != "OPS" || oneTask(t, r, "m-root").Moving {
		t.Error("the restored root's move is still split")
	}
}

// A MOVE ON A NODE WHOSE APPLIER LAGS ITS OWN APPENDS STILL TAKES ITS MARK DOWN.
//
// The mark comes down on the root's own subject, which the root's move already
// moved, so the last append has to see that move — decided against the root in
// its old project, it is refused, and the walk that carried everything reports
// a failure and leaves the mark up. On such a node every append before it
// reports `pending`, so nothing else in the walk waits for the root's; the
// last append has to wait for it itself.
func TestAMoveOnALaggingNodeTakesItsMarkDown(t *testing.T) {
	t.Parallel()
	r := moveFixture(t, "m-kid")
	r.lagBehindOwnWrites()
	if _, err := r.writer.MoveTaskToProject(t.Context(),
		statelog.NewOpID(time.Now(), "move"), "m-root", "OPS", nil); err != nil {
		t.Fatalf("a move on a node behind its own appends: %v", err)
	}
	r.drain()
	if root := oneTask(t, r, "m-root"); root.Project != "OPS" || root.Moving {
		t.Errorf("the root is in %q and marked %v, want OPS with its mark down",
			root.Project, root.Moving)
	}
}

// A SUBTASK LEFT IN ANOTHER PROJECT THAN ITS ROOT IS IN THE ATTENTION QUEUE,
// and leaves it when the walk carries it.
//
// The flag shipped with the schema — a column, a filter value and a share of
// the attention index — and nothing ever set it, so `flag=inconsistent_project`
// answered nothing on every company, including one holding the split subtree
// a stopped move leaves behind.
func TestASubtaskOutsideItsRootsProjectIsFlagged(t *testing.T) {
	t.Parallel()
	r := moveFixture(t, "m-kid-a", "m-kid-b")
	flagged := func() []string {
		t.Helper()
		// EVERY ROW ON ITS OWN, as list_work_items asks: the flag is a fact
		// about a SUBTASK's row, and the collapsed mode filters roots.
		answer, err := r.reader.Tasks(t.Context(), tracker.Query{
			Scope: tracker.Scope{Workspace: true}, Flags: []string{"inconsistent_project"},
			Subtasks: tracker.SubtasksSeparate, Level: statelog.ReadStale,
		}, wednesday)
		if err != nil {
			t.Fatalf("the attention query: %v", err)
		}
		var ids []string
		for _, row := range answer.Rows {
			ids = append(ids, row.ID)
		}
		return ids
	}
	if got := flagged(); len(got) != 0 {
		t.Fatalf("a subtree in one project is flagged: %v", got)
	}

	stopMoveAt(t, r, "m-kid-b")
	if got := flagged(); len(got) != 1 || got[0] != "m-kid-b" {
		t.Errorf("the attention queue holds %v, want m-kid-b — left in ENG under a "+
			"root the move carried into OPS", got)
	}

	if _, err := trackerWorker(t, r).Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	r.drain()
	if got := flagged(); len(got) != 0 {
		t.Errorf("the attention queue still holds %v after the walk carried the "+
			"subtree whole", got)
	}
}

// A LEAF'S MOVE CARRIES NO MARK: it is one append, finished the moment it
// lands, and a mark would be one more to take it down.
func TestALeafsMoveIsOneRecord(t *testing.T) {
	t.Parallel()
	r := moveFixture(t)
	end := r.logEnd(t)
	if _, err := r.writer.MoveTaskToProject(t.Context(),
		statelog.NewOpID(time.Now(), "move"), "m-root", "OPS", nil); err != nil {
		t.Fatalf("the move: %v", err)
	}
	r.drain()
	// THE ALIAS, THE COUNTER AND THE ROOT.
	if got := r.logEnd(t); got != end+3 {
		t.Errorf("a leaf's move put %d record(s) on the log, want 3", got-end)
	}
	if oneTask(t, r, "m-root").Moving {
		t.Error("a leaf's move left it marked mid-move")
	}
}

// THE MARK IS WRITTEN AT THE VERSION THAT ADDED IT, AND NOTHING ELSE IS RAISED.
//
// A build that reads only version 1 decodes a patch by dropping the field it
// does not know and applies the rest — a root re-homed with no mark on that
// node's row where every newer node holds one. At version 2 that build
// retains the record instead. Every other record stays at 1, because a
// retained record holds back every later record nested under its scope.
func TestTheMoveMarkIsWrittenAtTheVersionThatAddedIt(t *testing.T) {
	t.Parallel()
	if got := (tracker.Domain{}).RecordVersion(); got != 2 {
		t.Fatalf("this build reads record version %d, want 2", got)
	}
	r := moveFixture(t, "m-kid")
	start := r.logEnd(t)
	if _, err := r.writer.MoveTaskToProject(t.Context(),
		statelog.NewOpID(time.Now(), "move"), "m-root", "OPS", nil); err != nil {
		t.Fatalf("the move: %v", err)
	}
	end := r.logEnd(t)
	marks := 0
	for seq := start + 1; seq <= end; seq++ {
		rec := r.recordAt(t, seq)
		var patch tracker.TaskPatch
		carriesMark := rec.Subject.Kind == tracker.KindTask &&
			json.Unmarshal(rec.Mutation, &patch) == nil && patch.Moving != nil
		want := 1
		if carriesMark {
			want, marks = 2, marks+1
		}
		if rec.V != want {
			t.Errorf("the %s record on %s carries version %d, want %d",
				rec.Op, rec.Subject, rec.V, want)
		}
	}
	if marks != 2 {
		t.Errorf("the move wrote %d record(s) carrying the mark, want 2 — the "+
			"root's move and the mark coming down", marks)
	}

	generation, _, err := tracker.GenerationRecord{}.GenerationRecord(statelog.GenerationFacts{
		Generation: 2, By: "ops-1", Writer: "node-a", At: wednesday,
	})
	if err != nil {
		t.Fatalf("encode a generation: %v", err)
	}
	if env, err := tracker.DecodeEnvelope(generation.Payload); err != nil || env.V != 1 {
		t.Errorf("a generation record carries version %d (%v), want 1 — an older "+
			"node retains it and never makes the transition", env.V, err)
	}
}

// A BUILD THAT READS ONLY VERSION 1 RETAINS THE MARKED RECORDS RATHER THAN
// APPLYING HALF OF THEM — through the real framework loop, over the real log —
// and goes on applying the records it can read.
func TestAnOlderBuildRetainsAMoveMark(t *testing.T) {
	t.Parallel()
	r := moveFixture(t, "m-kid")
	if _, err := r.writer.MoveTaskToProject(t.Context(),
		statelog.NewOpID(time.Now(), "move"), "m-root", "OPS", nil); err != nil {
		t.Fatalf("the move: %v", err)
	}
	// AND A BARRIER, which every build must apply: one is appended for
	// every linearizable read, on every node, through the whole upgrade.
	barrier, err := tracker.EncodeBarrier(statelog.Envelope{Kind: statelog.BarrierKind})
	if err != nil {
		t.Fatalf("encode a barrier: %v", err)
	}
	if _, _, err := r.log.Append(t.Context(), tracker.Domain{}.Stream().SubjectPrefix+
		"."+tracker.BarrierSubject().String(), "", nil, barrier); err != nil {
		t.Fatalf("append a barrier: %v", err)
	}
	end := r.logEnd(t)

	older, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "older.db"),
		store.Options{PinnedWriters: 1})
	if err != nil {
		t.Fatalf("open the older node's store: %v", err)
	}
	t.Cleanup(func() { _ = older.Close() })
	runner, err := statelog.NewRunner(statelog.RunnerDeps{
		Domain:  versionOneTracker{},
		Applier: tracker.NewApplier("node-older"),
		Fetch:   &trackerLogFetch{log: r.log, next: 1},
		Log:     r.log,
		DB:      older.Replicated(),
	})
	if err != nil {
		t.Fatalf("build the older node's applier: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	deadline := time.Now().Add(20 * time.Second)
	for runner.Committed().Seq < end {
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("the older node reached %d of %d", runner.Committed().Seq, end)
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("the older node's loop: %v", err)
	}

	value := func(query string) string {
		t.Helper()
		var v string
		if err := older.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
			return tx.QueryRowContext(t.Context(), query).Scan(&v)
		}); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return v
	}
	if got := value(`SELECT project_key FROM tracker_tasks WHERE id = 'm-root'`); got != "ENG" {
		t.Errorf("the older node applied the marked move of the root — it is in "+
			"%s there, with no mark on its row", got)
	}
	if got := value(`SELECT COUNT(*) FROM tracker_log_deferred`); got != "2" {
		t.Errorf("the older node retained %s record(s), want the two carrying the "+
			"mark — and never the barrier", got)
	}
	if got := value(`SELECT project_key FROM tracker_tasks WHERE id = 'm-kid'`); got != "OPS" {
		t.Errorf("the older node holds m-kid in %s, want OPS — the records it can "+
			"read are not held back by the root's", got)
	}
}

// stopMoveAt runs a move of m-root into OPS that the broker refuses at one
// descendant, and applies what landed: a walk that stopped there. It returns
// the move's operation id.
func stopMoveAt(t *testing.T, r *roundTrip, descendant string) string {
	t.Helper()
	return stopMoveOf(t, r, "m-root", descendant)
}

// stopMoveOf is [stopMoveAt] for any root.
func stopMoveOf(t *testing.T, r *roundTrip, root, descendant string) string {
	t.Helper()
	lossy, log := r.lossyWriter(t)
	log.refuse(descendant)
	op := statelog.NewOpID(time.Now(), "move")
	if _, err := lossy.MoveTaskToProject(t.Context(), op, root, "OPS", nil); err == nil {
		t.Fatalf("a move refused on %s reported success", descendant)
	}
	log.refuse("")
	r.drain()
	if got := oneTask(t, r, descendant); got.Project != "ENG" {
		t.Fatalf("the premise: %s is in %q, want ENG — the walk stopped there",
			descendant, got.Project)
	}
	return op
}

// recordAt decodes one record on the harness's log.
func (r *roundTrip) recordAt(t *testing.T, seq uint64) tracker.MutationRecord {
	t.Helper()
	_, payload, _, ok, err := r.log.At(t.Context(), seq)
	if err != nil || !ok {
		t.Fatalf("read record %d: (%v, %v)", seq, ok, err)
	}
	rec, err := tracker.Decode(payload)
	if err != nil {
		t.Fatalf("decode record %d: %v", seq, err)
	}
	return rec
}

// versionOneTracker is this domain as a build that reads only record version
// 1 sees it.
type versionOneTracker struct{ tracker.Domain }

func (versionOneTracker) RecordVersion() int { return 1 }

// trackerLogFetch hands a framework loop every record on a harness's log, in
// order.
type trackerLogFetch struct {
	log  *js.DomainLog
	mu   sync.Mutex
	next uint64
}

func (f *trackerLogFetch) Fetch(ctx context.Context, maxMessages, _ int,
	wait time.Duration) ([]statelog.Message, error) {

	end, err := f.log.End(ctx)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	var out []statelog.Message
	for f.next <= end && len(out) < maxMessages {
		_, payload, storedAt, ok, err := f.log.At(ctx, f.next)
		if err != nil {
			f.mu.Unlock()
			return nil, err
		}
		if ok {
			out = append(out, statelog.Message{
				Seq: f.next, StoredAt: storedAt, Payload: payload,
				Ack: func() error { return nil },
			})
		}
		f.next++
	}
	f.mu.Unlock()
	if len(out) == 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(min(wait, 50*time.Millisecond)):
		}
	}
	return out, nil
}

func (f *trackerLogFetch) Pending(ctx context.Context) (uint64, error) {
	end, err := f.log.End(ctx)
	if err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.next > end {
		return 0, nil
	}
	return end - f.next + 1, nil
}

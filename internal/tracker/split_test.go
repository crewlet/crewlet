package tracker_test

import (
	"database/sql"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/tracker"
)

// seedOps declares the project a move carries a subtree into.
func seedOps(t *testing.T, r *roundTrip) {
	t.Helper()
	if _, err := r.writer.WriteDocument(t.Context(), "op-ops",
		tracker.ProjectSubject("OPS"), "", tracker.Project{
			V: 1, Key: "OPS", Name: "Operations",
			CreatedAt: wednesday, UpdatedAt: wednesday,
		}, tracker.ChangeProjectCreated, nil); err != nil {
		t.Fatalf("seed the target project: %v", err)
	}
	r.drain()
}

// fileUnder files one task into ENG, under parent when it names one.
func fileUnder(t *testing.T, r *roundTrip, id, parent string) {
	t.Helper()
	task := newTask(id)
	task.Key = ""
	if parent != "" {
		task.Parent = &parent
	}
	if _, err := r.writer.CreateTask(t.Context(), "op-"+id, task, nil); err != nil {
		t.Fatalf("CreateTask %s: %v", id, err)
	}
	r.drain()
}

// projectFlag is a task's own `inconsistent_project` column on this node.
func projectFlag(t *testing.T, r *roundTrip, id string) int {
	t.Helper()
	var flag int
	if err := r.db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(),
			`SELECT inconsistent_project FROM tracker_tasks WHERE id = ?`, id).
			Scan(&flag)
	}); err != nil {
		t.Fatalf("read %s's project flag: %v", id, err)
	}
	return flag
}

// rearm is [hookedAppender.arm] for a case whose second hook fires after its
// first already has: the hook runs once per arming.
func (a *hookedAppender) rearm(match func(subject, opID string) bool, hook func()) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.match, a.hook, a.fired = match, hook, false
}

// splitJob is the tracker duty's job over the flag, as the worker holds it.
func splitJob(t *testing.T, r *roundTrip) func() bool {
	t.Helper()
	jobs, err := tracker.Jobs(tracker.DutyDeps{
		DB: r.db, Writer: r.writer, NodeID: "node-a", Reader: linearReader(t, r),
	})
	if err != nil {
		t.Fatalf("build the tracker's jobs: %v", err)
	}
	for _, job := range jobs {
		if job.Name == "tracker_inconsistent_project" {
			return func() bool {
				t.Helper()
				open, err := job.Gate(t.Context())
				if err != nil {
					t.Fatalf("read the gate: %v", err)
				}
				return open
			}
		}
	}
	t.Fatal("the tracker registers no tracker_inconsistent_project job")
	return nil
}

// stoppedMove is a cross-project move whose walk lost its claim after its
// first subtask: m-root and m-kid-1 are in OPS, and m-kid-2 and m-kid-1's own
// subtask m-gk are still in ENG. The walk reaches m-kid-2 before m-gk, which
// is the (depth, id) order it moves in.
func stoppedMove(t *testing.T) (*roundTrip, *hookedAppender) {
	t.Helper()
	r, hooked := newHookedRoundTrip(t)
	r.applyWhileWriting()
	seedOps(t, r)
	fileUnder(t, r, "m-root", "")
	fileUnder(t, r, "m-kid-1", "m-root")
	fileUnder(t, r, "m-kid-2", "m-root")
	fileUnder(t, r, "m-gk", "m-kid-1")

	claims, clock := memory.New(), newCaseClock()
	holder := writerOverClaims(t, r, claims, clock)
	hooked.arm(func(_, opID string) bool { return opID == "op-move.d/m-kid-1" },
		func() { lapse(claims, clock) })
	_, err := holder.MoveTaskToProject(t.Context(), "op-move", "m-root", "OPS", nil)
	if !hooked.didFire() || !errors.Is(err, tracker.ErrClaimLost) {
		t.Fatalf("the walk answered %v and its lease lapsed=%v, so this fixture "+
			"is not the stopped walk it names", err, hooked.didFire())
	}
	r.drain()
	return r, hooked
}

// A MOVE THAT STOPS MID-SUBTREE FLAGS EXACTLY WHAT IT LEFT BEHIND.
//
// A cross-project move is one append per task, so a walk that stops leaves
// part of its subtree in a project its root is not in — which no read of any
// one row says. The root's own apply derives it for the whole subtree beneath
// it, and each descendant's own move clears it for itself and what is under
// it: so the flag stands on precisely the tasks still to move, grandchildren
// of a moved child included, and on nothing that moved.
//
// Mutation: derive only for the task a record names (not its subtree) and the
// root's move flags nothing; derive at every version and the first-version
// case below goes red instead.
func TestAMoveThatStopsMidSubtreeFlagsWhatItLeftBehind(t *testing.T) {
	t.Parallel()
	r, _ := stoppedMove(t)
	for id, want := range map[string]struct {
		project string
		flag    int
	}{
		"m-root":  {"OPS", 0},
		"m-kid-1": {"OPS", 0},
		"m-kid-2": {"ENG", 1},
		"m-gk":    {"ENG", 1},
	} {
		if got := oneTask(t, r, id).Project; got != want.project {
			t.Errorf("%s is in %s, want %s — the walk did not stop where this "+
				"case needs it to", id, got, want.project)
		}
		if got := projectFlag(t, r, id); got != want.flag {
			t.Errorf("%s's inconsistent_project is %d, want %d", id, got, want.flag)
		}
	}
}

// THE DUTY FINISHES A MOVE WHOSE WALK STOPPED MID-SUBTREE.
//
// Re-issuing the move is refused once its root has moved, so without a repair
// the subtree stays split across two projects for ever — a board drawn in
// either shows half of it. The duty takes the move's claim, reads the root at
// the linearizable level, and moves every live descendant still outside the
// root's project by the walk's own steps, on keys minted in that project: so
// the rest arrive with keys nobody else holds, their old keys kept as former
// keys, and the flags clear on every node from the moves' own applies.
//
// Mutation: drop the job from tracker.Jobs, or select the tasks left behind in
// the root's project rather than outside it, and m-kid-2 stays in ENG.
func TestTheDutyFinishesAMoveWhoseWalkStoppedMidSubtree(t *testing.T) {
	t.Parallel()
	r, _ := stoppedMove(t)
	before := map[string]string{}
	for _, id := range []string{"m-kid-2", "m-gk"} {
		before[id] = oneTask(t, r, id).Key
	}

	swept, err := trackerWorker(t, r).Tick(t.Context())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := swept["tracker_inconsistent_project"]; got != 2 {
		t.Fatalf("the sweep moved %d task(s), want the two the walk left "+
			"behind: %v", got, swept)
	}
	r.drain()

	keys := map[string]string{}
	for _, id := range []string{"m-root", "m-kid-1", "m-kid-2", "m-gk"} {
		task := oneTask(t, r, id)
		if task.Project != "OPS" || !strings.HasPrefix(task.Key, "OPS-") {
			t.Errorf("%s is %s in %s after the sweep, want it keyed in OPS",
				id, task.Key, task.Project)
		}
		if other, taken := keys[task.Key]; taken {
			t.Errorf("%s and %s are both keyed %s", id, other, task.Key)
		}
		keys[task.Key] = id
		if got := projectFlag(t, r, id); got != 0 {
			t.Errorf("%s still carries inconsistent_project after the sweep", id)
		}
		old, moved := before[id]
		if !moved {
			continue
		}
		if len(task.FormerKeys) == 0 || task.FormerKeys[len(task.FormerKeys)-1] != old {
			t.Errorf("%s's former keys are %v and do not end in %s — the task "+
				"no longer says which key it had", id, task.FormerKeys, old)
		}
		if oneTask(t, r, old).ID != id {
			t.Errorf("%s's key before the sweep, %s, no longer finds it — the "+
				"key somebody pasted before the move stops resolving", id, old)
		}
	}
	// AND NOTHING IS LEFT FOR THE NEXT SWEEP, whose gate reads the flags
	// the moves cleared.
	if swept, err := trackerWorker(t, r).Tick(t.Context()); err != nil || len(swept) != 0 {
		t.Errorf("a second sweep did %v (%v) over a subtree already whole", swept, err)
	}
}

// THE SWEEP PASSES OVER A MOVE THAT IS STILL WALKING.
//
// The flags stand for the whole of a live walk too — the root's apply raises
// them before the walk has reached a single descendant — so a flag alone
// cannot tell a walk that died from one still running. The claim can: the
// walk holds it for its whole length, so the sweep takes it or does nothing.
// Run beside the walk, the sweep would mint a second number for a descendant
// the walk was about to move, and the walk's own step for it would then be
// refused, stopping the walk half-way.
//
// Mutation: carry on in finishSplit when the claim is held elsewhere and the
// sweep moves the subtasks the walk then fails to.
func TestTheSweepPassesOverAMoveThatIsStillWalking(t *testing.T) {
	t.Parallel()
	r, hooked := newHookedRoundTrip(t)
	r.applyWhileWriting()
	seedOps(t, r)
	fileUnder(t, r, "m-root", "")
	fileUnder(t, r, "m-kid-1", "m-root")
	fileUnder(t, r, "m-kid-2", "m-root")

	var swept map[string]int64
	var tickErr error
	hooked.arm(func(_, opID string) bool { return opID == "op-move.root" },
		func() {
			// THE ROOT APPLIED FIRST, so the sweep's gate and its read see
			// both subtasks flagged exactly as a peer's would.
			r.drain()
			swept, tickErr = trackerWorker(t, r).Tick(t.Context())
		})
	if _, err := r.writer.MoveTaskToProject(t.Context(), "op-move", "m-root",
		"OPS", nil); err != nil {
		t.Fatalf("the walk the sweep ran inside of failed: %v", err)
	}
	if !hooked.didFire() {
		t.Fatal("the sweep never ran inside the walk, so this case is not the " +
			"shape it names")
	}
	if tickErr != nil {
		t.Fatalf("Tick: %v", tickErr)
	}
	if n := swept["tracker_inconsistent_project"]; n != 0 {
		t.Errorf("the sweep moved %d task(s) while their walk was still running", n)
	}
	r.drain()
	for _, id := range []string{"m-kid-1", "m-kid-2"} {
		moves := r.strings(`SELECT id FROM tracker_history
			WHERE subject_id = ? AND kind = 'moved'`, id)
		if len(moves) != 1 || oneTask(t, r, id).Project != "OPS" {
			t.Errorf("%s was moved %d time(s) and is in %s, want once into OPS",
				id, len(moves), oneTask(t, r, id).Project)
		}
	}
}

// A SWEEP WHOSE LEASE LAPSES MID-WALK MOVES NOTHING AFTER.
//
// The repair is a walk like the gesture it finishes, one append per task under
// the move's claim, and the same fence holds for it: a sweep that can no longer
// vouch for its lease stops before its next append rather than re-key a
// subtree beside whoever holds the claim now.
//
// Mutation: make finishSplit append through d.deps.Writer rather than the
// writer fenced on its claim, and the second subtask moves.
func TestASplitSweepWhoseLeaseLapsesMidWalkMovesNothingAfter(t *testing.T) {
	t.Parallel()
	r, hooked := newHookedRoundTrip(t)
	r.applyWhileWriting()
	seedOps(t, r)
	fileUnder(t, r, "m-root", "")
	fileUnder(t, r, "m-kid-1", "m-root")
	fileUnder(t, r, "m-kid-2", "m-root")

	// THE WALK STOPS STRAIGHT AFTER ITS ROOT, leaving both subtasks.
	claims, clock := memory.New(), newCaseClock()
	holder := writerOverClaims(t, r, claims, clock)
	hooked.arm(func(_, opID string) bool { return opID == "op-move.root" },
		func() { lapse(claims, clock) })
	if _, err := holder.MoveTaskToProject(t.Context(), "op-move", "m-root",
		"OPS", nil); !errors.Is(err, tracker.ErrClaimLost) {
		t.Fatalf("the walk answered %v, want it stopped by its lapsed claim", err)
	}
	r.drain()

	// AND THE SWEEP'S OWN LEASE LAPSES after its first subtask.
	sweeper := writerOverClaims(t, r, claims, clock)
	hooked.rearm(func(_, opID string) bool {
		return strings.HasPrefix(opID, "duty.") && strings.HasSuffix(opID, ".d/m-kid-1")
	}, func() { lapse(claims, clock) })
	swept, err := trackerWorkerWriting(t, r, slog.New(slog.DiscardHandler),
		sweeper).Tick(t.Context())
	if !hooked.didFire() {
		t.Fatal("the lease never lapsed inside the sweep's walk, so this case " +
			"is not the shape it names")
	}
	if !errors.Is(err, tracker.ErrClaimLost) {
		t.Fatalf("a sweep whose lease lapsed mid-walk answered %v, want the "+
			"tick to report the lost claim", err)
	}
	if n := swept["tracker_inconsistent_project"]; n != 1 {
		t.Errorf("the sweep counted %d task(s) moved, want the one before the "+
			"lapse", n)
	}
	r.drain()
	for id, want := range map[string]string{"m-kid-1": "OPS", "m-kid-2": "ENG"} {
		if got := oneTask(t, r, id).Project; got != want {
			t.Errorf("%s is in %s after the sweep's lease lapsed, want %s", id, got, want)
		}
	}
}

// A RECORD BELOW THE FLAG'S VERSION LEAVES THE FLAG AS IT FOUND IT.
//
// Every node applies a record by the rule of the version it carries, so a
// build that predates the flag and one that has it hold the same rows for the
// same log only if a record written before the flag existed derives none. That
// cuts both ways, and both are here: a first-version move of a root flags
// nothing beneath it, and a first-version move that heals a split leaves the
// flag it found standing — which the duty's gate then reads past, because it
// compares the projects beside the flag rather than trusting it.
//
// Mutation: derive at every version and the first-version move flags m-kid;
// drop the projects from the duty's selection and the gate opens over a task
// already in its root's project.
func TestARecordBelowTheFlagsVersionLeavesTheFlagAsItFoundIt(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedOps(t, r)
	fileUnder(t, r, "m-root", "")
	fileUnder(t, r, "m-kid", "m-root")
	gate := splitJob(t, r)
	move := func(id, opID string, version int) {
		t.Helper()
		ops := "OPS"
		rec := taskRecord(id, tracker.OpPatch, tracker.TaskPatch{Project: &ops}, nil)
		rec.V, rec.OpID, rec.Kind = version, opID, tracker.ChangeMoved
		r.appendRecord(rec)
		r.drain()
	}

	move("m-root", "op-v1-root", tracker.RecordVersion)
	if got := projectFlag(t, r, "m-kid"); got != 0 {
		t.Fatalf("a first-version move of the root flagged its subtask — the "+
			"record a build without the flag applies deriving it anyway: %d", got)
	}
	move("m-root", "op-v5-root", tracker.ProjectFlagRecordVersion)
	if got := projectFlag(t, r, "m-kid"); got != 1 {
		t.Fatalf("a move at the flag's version left the subtask in ENG unflagged: %d", got)
	}
	if !gate() {
		t.Fatal("the gate is shut over a subtask in ENG under a root in OPS")
	}

	move("m-kid", "op-v1-kid", tracker.RecordVersion)
	if got := projectFlag(t, r, "m-kid"); got != 1 {
		t.Errorf("a first-version move of the subtask cleared its flag: %d", got)
	}
	if gate() {
		t.Error("the gate opens over a subtask already in its root's project — " +
			"a flag a first-version record left standing, trusted alone, runs " +
			"the job on every tick for a walk with nothing to move")
	}
}

// A MOVE PASSES OVER A DESCENDANT IN THE TRASH, AND THE DUTY TAKES IT ACROSS
// ONCE IT IS RESTORED.
//
// A removed task is frozen — every patch on one is refused — so a walk that
// tried to move one would stop there and leave every descendant after it
// behind. It is passed over instead: it stays flagged, the sweep leaves it
// while it is in the trash, and moves it once it is out.
//
// Mutation: move removed descendants in MoveTaskToProject and the walk stops
// at m-kid-1; drop the trash from the duty's selection and its gate opens over
// a task no patch can move.
func TestAMoveLeavesADescendantInTheTrashForTheDuty(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedOps(t, r)
	fileUnder(t, r, "m-root", "")
	fileUnder(t, r, "m-kid-1", "m-root")
	fileUnder(t, r, "m-kid-2", "m-root")
	removeNow(t, r, "m-kid-1")
	gate := splitJob(t, r)

	if _, err := r.writer.MoveTaskToProject(t.Context(), "op-move", "m-root",
		"OPS", nil); err != nil {
		t.Fatalf("a move over a subtree with an item in the trash failed: %v", err)
	}
	r.drain()
	if got := oneTask(t, r, "m-kid-2").Project; got != "OPS" {
		t.Errorf("the live subtask is in %s, want OPS", got)
	}
	if got := projectFlag(t, r, "m-kid-1"); got != 1 {
		t.Errorf("the subtask in the trash is not flagged (%d), so nothing "+
			"brings it across once it is restored", got)
	}
	if gate() {
		t.Error("the gate opens over a task in the trash, which no patch can move")
	}

	if _, err := r.writer.RestoreTask(t.Context(), "op-restore", "m-kid-1",
		"ENG", nil); err != nil {
		t.Fatalf("RestoreTask: %v", err)
	}
	r.drain()
	swept, err := trackerWorker(t, r).Tick(t.Context())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if swept["tracker_inconsistent_project"] != 1 {
		t.Fatalf("the sweep after the restore did %v, want the restored subtask moved",
			swept)
	}
	r.drain()
	if got := oneTask(t, r, "m-kid-1"); got.Project != "OPS" ||
		!strings.HasPrefix(got.Key, "OPS-") {
		t.Errorf("the restored subtask is %s in %s, want it keyed in OPS",
			got.Key, got.Project)
	}
}

// A SUBTASK IS FILED IN ITS PARENT'S PROJECT.
//
// A subtree lives in its root's project, and the duty moves whatever is not —
// so a create that filed a subtask anywhere else would answer with a key the
// task stopped having a sweep later. Refused inside the create's own decide,
// the caller is told which project to file it in, and no number is taken.
//
// Mutation: drop refuseForeignParent from refuseCreate and the subtask lands
// in OPS under a parent in ENG.
func TestASubtaskIsFiledInItsParentsProject(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedOps(t, r)
	fileUnder(t, r, "t-parent", "")

	parent := "t-parent"
	kid := newTask("t-kid")
	kid.Project, kid.Parent = "OPS", &parent
	_, err := r.writer.CreateTask(t.Context(), "op-kid", kid, nil)
	if !errors.Is(err, tracker.ErrForeignParent) || !strings.Contains(err.Error(), "ENG") {
		t.Fatalf("a subtask filed in OPS under a parent in ENG answered %v, "+
			"want it refused naming ENG", err)
	}
	r.drain()
	if got := r.strings(`SELECT id FROM tracker_tasks WHERE id = 't-kid'`); len(got) != 0 {
		t.Error("the refused subtask was written")
	}
	if got := counterOf(t, r, "OPS"); got != 0 {
		t.Errorf("the refused create took %d number(s) from OPS", got)
	}

	kid.Project = "ENG"
	if _, err := r.writer.CreateTask(t.Context(), "op-kid-eng", kid, nil); err != nil {
		t.Fatalf("the same subtask in its parent's project was refused: %v", err)
	}
}

// A TASK IN A CYCLE IS NOT FLAGGED AS OUTSIDE ITS ROOT'S PROJECT.
//
// A cycle has no root: the closure walk stops where it meets a task it has
// already seen, and whichever task that is only says where the walk began.
// Compared with it, a cycle spanning two projects would flag its tasks as
// outside a root they do not have — and the duty would move them into a
// project the walk's starting point chose. `cycle` is the flag they carry,
// and it is the one somebody resolves first.
//
// Mutation: drop the cycle from the derivation and both tasks are flagged.
func TestATaskInACycleIsNotFlaggedOutsideItsRootsProject(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedOps(t, r)
	fileUnder(t, r, "t-eng", "")
	ops := newTask("t-ops")
	ops.Key, ops.Project = "", "OPS"
	if _, err := r.writer.CreateTask(t.Context(), "op-t-ops", ops, nil); err != nil {
		t.Fatalf("CreateTask t-ops: %v", err)
	}
	r.drain()
	for _, step := range []struct{ id, project, parent string }{
		{"t-eng", "ENG", "t-ops"},
		{"t-ops", "OPS", "t-eng"},
	} {
		parent := step.parent
		if _, err := r.writer.UpdateTask(t.Context(), "op-under-"+step.id,
			step.id, step.project, tracker.NoIfMatch,
			tracker.TaskPatch{Parent: &parent}, tracker.ChangeReparented,
			nil); err != nil {
			t.Fatalf("re-parent %s: %v", step.id, err)
		}
		r.drain()
	}
	for _, id := range []string{"t-eng", "t-ops"} {
		var cycle, flag int
		if err := r.db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
			return tx.QueryRowContext(t.Context(), `SELECT cycle,
				inconsistent_project FROM tracker_tasks WHERE id = ?`, id).
				Scan(&cycle, &flag)
		}); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if cycle != 1 {
			t.Fatalf("%s is not in a cycle, so this case is not the shape it names", id)
		}
		if flag != 0 {
			t.Errorf("%s is flagged outside its root's project, and a task in "+
				"a cycle has no root", id)
		}
	}
}

// A MERGE ACROSS PROJECTS CARRIES THE SUBTASKS INTO THE TARGET'S PROJECT.
//
// A merge that moves the duplicate's subtasks re-parents them onto the item it
// folds into, and a re-parent does not change a project — so a duplicate in
// ENG merged into an item in OPS leaves its subtasks in ENG under a root in
// OPS, which no board draws. The re-parent is at the flag's version, so every
// node marks them, and the sweep carries them into OPS.
func TestAMergeAcrossProjectsCarriesTheSubtasksIntoTheTargetsProject(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.applyWhileWriting()
	seedOps(t, r)
	fileUnder(t, r, "dup", "")
	fileUnder(t, r, "kid", "dup")
	keep := newTask("keep")
	keep.Key, keep.Project = "", "OPS"
	if _, err := r.writer.CreateTask(t.Context(), "op-keep", keep, nil); err != nil {
		t.Fatalf("CreateTask keep: %v", err)
	}
	r.drain()

	if _, err := r.writer.MergeDuplicates(t.Context(), "op-merge", "dup", "keep",
		true, nil); err != nil {
		t.Fatalf("MergeDuplicates: %v", err)
	}
	r.drain()
	if got := projectFlag(t, r, "kid"); got != 1 {
		t.Fatalf("the subtask the merge moved under an item in OPS is not "+
			"flagged (%d), so nothing carries it into OPS", got)
	}
	swept, err := trackerWorker(t, r).Tick(t.Context())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if swept["tracker_inconsistent_project"] != 1 {
		t.Fatalf("the sweep did %v, want the merged subtask moved", swept)
	}
	r.drain()
	got := r.task(t, "kid")
	if got.Task.Project != "OPS" || !strings.HasPrefix(got.Task.Key, "OPS-") ||
		parentOf(got) != "keep" {
		t.Errorf("the merged subtask is %s in %s under %q, want it keyed in "+
			"OPS under keep", got.Task.Key, got.Task.Project, parentOf(got))
	}
}

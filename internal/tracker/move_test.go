package tracker_test

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// moveFixture is a harness holding a second project, OPS, and a root task in
// ENG with the children named, each a direct child of the root.
func moveFixture(t *testing.T, kids ...string) *roundTrip {
	t.Helper()
	r := newRoundTrip(t)
	r.applyWhileWriting()
	if _, err := r.writer.WriteDocument(t.Context(), "op-ops",
		tracker.ProjectSubject("OPS"), "", tracker.Project{
			V: 1, Key: "OPS", Name: "Operations",
			CreatedAt: wednesday, UpdatedAt: wednesday,
		}, tracker.ChangeProjectCreated, nil); err != nil {
		t.Fatalf("seed the target project: %v", err)
	}
	r.drain()
	if _, err := r.writer.CreateTask(t.Context(), "op-root", newTask("m-root"), nil); err != nil {
		t.Fatalf("file the root: %v", err)
	}
	r.drain()
	parent := "m-root"
	for _, id := range kids {
		kid := newTask(id)
		kid.Parent, kid.Depth = &parent, 1
		if _, err := r.writer.CreateTask(t.Context(), "op-"+id, kid, nil); err != nil {
			t.Fatalf("file %s: %v", id, err)
		}
		r.drain()
	}
	return r
}

// A RETRY OF A MOVE THAT FINISHED IS ANSWERED WITH IT: applied, under the
// root's key in the target, with nothing appended.
//
// The root is already in the target, and the move used to refuse exactly that
// — "already in OPS", about the project the move itself had put it in.
func TestARetryOfAFinishedMoveIsAnsweredWithIt(t *testing.T) {
	t.Parallel()
	r := moveFixture(t, "m-kid")
	op := statelog.NewOpID(time.Now(), "move")
	first, err := r.writer.MoveTaskToProject(t.Context(), op, "m-root", "OPS", nil)
	if err != nil {
		t.Fatalf("the move: %v", err)
	}
	r.drain()
	end := r.logEnd(t)

	retry, err := r.writer.MoveTaskToProject(t.Context(), op, "m-root", "OPS", nil)
	if err != nil {
		t.Fatalf("the retry of a move that finished: %v", err)
	}
	if retry.Outcome != statelog.OutcomeApplied || !retry.Collapsed ||
		retry.Position != first.Position || retry.Key != first.Key {
		t.Fatalf("the retry = %+v key %q, want applied at the root's move %s as %s",
			retry.Result, retry.Key, first.Position, first.Key)
	}
	if got := r.logEnd(t); got != end {
		t.Fatalf("the retry put %d record(s) on the log", got-end)
	}
}

// A RETRY OF A MOVE THAT STOPPED PARTWAY MOVES WHAT IS LEFT, ONCE.
//
// The first run carried the root and one child and was refused on the second.
// The retry's walk is shorter than the first run's, so a step named by its
// place in the walk answered the child that was LEFT with the record of the
// child that had already moved — and reported the move finished with that
// child still in the old project.
func TestARetryOfAStoppedMoveMovesWhatIsLeftOnce(t *testing.T) {
	t.Parallel()
	r := moveFixture(t, "m-kid-a", "m-kid-b")
	lossy, log := r.lossyWriter(t)
	op := statelog.NewOpID(time.Now(), "move")

	log.refuse("m-kid-b")
	if _, err := lossy.MoveTaskToProject(t.Context(), op, "m-root", "OPS", nil); err == nil {
		t.Fatal("a move refused on a descendant reported success")
	}
	log.refuse("")
	r.drain()
	moved := oneTask(t, r, "m-kid-a")
	if oneTask(t, r, "m-kid-b").Project != "ENG" || moved.Project != "OPS" {
		t.Fatal("the premise: the first run should have moved m-kid-a and not m-kid-b")
	}
	end := r.logEnd(t)

	retry, err := lossy.MoveTaskToProject(t.Context(), op, "m-root", "OPS", nil)
	if err != nil {
		t.Fatalf("the retry of a move that stopped partway: %v", err)
	}
	r.drain()
	if retry.Outcome != statelog.OutcomeApplied {
		t.Errorf("the retry answered %q, want applied", retry.Outcome)
	}
	left := oneTask(t, r, "m-kid-b")
	if left.Project != "OPS" || !strings.HasPrefix(left.Key, "OPS-") {
		t.Errorf("m-kid-b is %s in %q after the retry, want an OPS key in OPS",
			left.Key, left.Project)
	}
	if again := oneTask(t, r, "m-kid-a"); again.Key != moved.Key {
		t.Errorf("m-kid-a was re-keyed from %s to %s by the retry", moved.Key, again.Key)
	}
	// ONE RANGE FOR WHAT WAS LEFT AND ONE MOVE: no second root record and
	// no second move of the child that had already gone.
	if got := r.logEnd(t); got != end+2 {
		t.Errorf("the retry put %d record(s) on the log, want 2 — a counter "+
			"for what was left and m-kid-b's move", got-end)
	}
}

// A ROOT SOMEBODY ELSE MOVED IS STILL REFUSED — the retry path is for THIS
// operation's move, and finishing another one's walk under a new name would
// be a second move nobody asked for.
func TestAMoveOfARootAnotherMoveCarriedIsRefused(t *testing.T) {
	t.Parallel()
	r := moveFixture(t, "m-kid")
	if _, err := r.writer.MoveTaskToProject(t.Context(),
		statelog.NewOpID(time.Now(), "move"), "m-root", "OPS", nil); err != nil {
		t.Fatalf("the first move: %v", err)
	}
	r.drain()
	end := r.logEnd(t)
	_, err := r.writer.MoveTaskToProject(t.Context(),
		statelog.NewOpID(time.Now(), "move-again"), "m-root", "OPS", nil)
	if err == nil || !strings.Contains(err.Error(), "already in OPS") {
		t.Fatalf("a second move of a root already in OPS = %v, want a refusal", err)
	}
	if got := r.logEnd(t); got != end {
		t.Errorf("the refused move put %d record(s) on the log", got-end)
	}
}

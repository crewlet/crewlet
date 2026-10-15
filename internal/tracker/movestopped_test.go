package tracker_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A MOVE WHOSE ROOT LANDED AND WHOSE WALK STOPPED ANSWERS A STOP, NEVER A
// REFUSAL — naming what moved, what did not, and the operation that finishes
// it.
//
// The walk answered the bare error of the step it stopped at, and every
// caller rendered that as it renders a refusal: "the change was NOT made",
// about a root already re-keyed into the target. A model told that moved it
// again, under a new operation — which is refused, because the root is in the
// target and that operation's ledger never put it there — and a person told
// that went looking for the item under a key it no longer has.
//
// Mutation: have the walk answer the step's error as it is, and errors.As
// finds no stop.
func TestAStoppedMoveSaysWhatMovedAndWhatDidNot(t *testing.T) {
	t.Parallel()
	r := moveFixture(t, "m-kid-a", "m-kid-b", "m-kid-c")
	lossy, log := r.lossyWriter(t)
	log.refuse("m-kid-b")
	op := statelog.NewOpID(time.Now(), "move")
	got, err := lossy.MoveTaskToProject(t.Context(), op, "m-root", "OPS", nil)
	log.refuse("")
	r.drain()

	var stopped *tracker.MoveStopped
	if !errors.As(err, &stopped) {
		t.Fatalf("a move stopped at m-kid-b answered %v, want a *tracker.MoveStopped", err)
	}
	root := oneTask(t, r, "m-root")
	switch {
	case got.Outcome != statelog.OutcomeApplied:
		t.Errorf("the stop carries the root's outcome %q, want applied — the "+
			"root's own move landed", got.Outcome)
	case root.Project != "OPS":
		t.Fatalf("the premise: the root is in %q, want OPS", root.Project)
	}
	// (depth, id) order: m-kid-a moved, m-kid-b was refused, m-kid-c was
	// never reached.
	if stopped.Root != "m-root" || stopped.Key != root.Key || stopped.Target != "OPS" ||
		stopped.Followed != 1 || stopped.Of != 3 || stopped.OpID != op ||
		stopped.Waiting != "" {
		t.Errorf("the stop is %+v, want m-root as %s in OPS with 1 of 3 followed "+
			"under %s", *stopped, root.Key, op)
	}
	if stopped.Err == nil || !strings.Contains(err.Error(), op) {
		t.Errorf("the stop says %q with cause %v, want it to name the operation "+
			"that finishes it and keep why it stopped", err, stopped.Err)
	}

	// A NEW OPERATION IS REFUSED, which is why the stop names its own: the
	// root is in OPS and that operation's ledger never moved it there.
	_, fresh := r.writer.MoveTaskToProject(t.Context(),
		statelog.NewOpID(time.Now(), "move"), "m-root", "OPS", nil)
	if fresh == nil || errors.As(fresh, new(*tracker.MoveStopped)) {
		t.Errorf("a new operation over the stopped move answered %v, want a "+
			"refusal — it is somebody else's move", fresh)
	}

	// AND THE SAME ONE FINISHES IT.
	if _, err := r.writer.MoveTaskToProject(t.Context(), op, "m-root", "OPS", nil); err != nil {
		t.Fatalf("the same operation again: %v", err)
	}
	r.drain()
	for _, id := range []string{"m-kid-a", "m-kid-b", "m-kid-c"} {
		if got := oneTask(t, r, id); got.Project != "OPS" {
			t.Errorf("%s is in %q after the same operation, want OPS", id, got.Project)
		}
	}
	if oneTask(t, r, "m-root").Moving {
		t.Error("the root is still marked mid-move once everything under it moved")
	}
}

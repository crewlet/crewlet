package tracker_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A GESTURE'S SECOND APPEND ON ONE SUBJECT WAITS FOR ITS FIRST, and it waits
// BEFORE it opens a snapshot.
//
// This is the state every multi-append sequence here is in between its own
// steps: the row this node holds is the one the earlier append MOVED, and the
// record that moved it is still on its way to this node's applier. Deciding
// from that row forms an expectation the earlier append has already made
// stale, so the broker refuses it — and the framework then waits for exactly
// the position the writer was holding all along, having spent a snapshot, a
// decide and a round trip to discover it.
//
// [tracker.Writer.After] is that wait moved ahead of all of it, and until
// something called it [statelog.Request.Session] had no producer at all.
func TestASecondWriteOnOneSubjectWaitsBeforeItSnapshots(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if _, err := r.writer.CreateTask(t.Context(), "op-1", newTask("t-1"), nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r.drain()

	first := "first"
	one, err := r.writer.UpdateTask(t.Context(), "op-2", "t-1", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Title: &first}, tracker.ChangeFields, nil)
	if err != nil {
		t.Fatalf("the first write: %v", err)
	}
	// DELIBERATELY NOT DRAINED. The row is here; the record that changed
	// it is not.

	second := "second"
	two, err := r.writer.After(one.Position).UpdateTask(t.Context(), "op-3", "t-1",
		"ENG", tracker.NoIfMatch, tracker.TaskPatch{Title: &second},
		tracker.ChangeFields, nil)
	var refusal *statelog.Unavailable
	switch {
	case err == nil:
		t.Fatal("a second write decided from a state below the caller's own first")
	case !errors.As(err, &refusal):
		t.Fatalf("the refusal is %v, want a named one a caller can act on", err)
	case refusal.Reason != statelog.ReasonBehind:
		t.Fatalf("the reason is %q, want %q", refusal.Reason, statelog.ReasonBehind)
	}
	// NO SNAPSHOT WAS OPENED, which is the whole of what the mark buys:
	// the framework's own recovery reaches the same wait, but only after a
	// round has formed a decision from rows that are about to move.
	if two.Rounds != 0 {
		t.Errorf("the write reports %d round(s), so it snapshotted and decided "+
			"before it found out it was behind its own previous write", two.Rounds)
	}
	// AND THE REFUSAL SAYS WHOSE WRITE IT IS WAITING FOR. "My applier is
	// lagging" and "a colleague is editing this" are one reason and two
	// different things to do about it.
	if !strings.Contains(refusal.Detail, "own") {
		t.Errorf("the detail is %q and does not name the write as the "+
			"caller's own", refusal.Detail)
	}
}

// A DEPENDENCY NAMING BOTH DIRECTIONS WRITES ITS OWN TASK TWICE, and the
// second of those commits carries the first as its mark.
//
// Step 2 publishes this task's `waiting_on` edges on its own subject, and step
// 3 publishes the dependents it gains on that same subject. Nothing about the
// two is visible in the result — a mirror that loses is collected as one-sided
// rather than raised — so the instrument is the witness: the session wait is a
// histogram, and it is observed by nothing else in this package.
func TestADependencyOnBothSidesWaitsForItsOwnAuthoredCommit(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	inSprint(t, r, "mid", nil)
	inSprint(t, r, "up", nil)
	inSprint(t, r, "down", nil)

	if _, err := r.writer.Depend(t.Context(), "op-both", tracker.DependencyChange{
		Task: "mid", Project: "ENG",
		WaitingOnAdd: []string{"up"},
		BlockingAdd:  []string{"down"},
	}, fixedLeads{}); err != nil {
		t.Fatalf("Depend: %v", err)
	}
	if waits := sessionWaits(r); waits != 1 {
		t.Fatalf("the call made %d session wait(s) and it writes task `mid` "+
			"twice — the mirror on its own subject is the one append here "+
			"that has to see this gesture's own earlier record", waits)
	}
}

// sessionWaits is how many times a write waited for the caller's own previous
// record on this rig.
func sessionWaits(r *roundTrip) int {
	var n uint64
	for _, snap := range r.metrics.Read() {
		if snap.Name == metrics.StatelogWriteSessionWait {
			n += snap.Count
		}
	}
	return int(n)
}

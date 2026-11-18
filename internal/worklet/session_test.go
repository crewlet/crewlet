package worklet_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A GESTURE'S SECOND APPEND ON ONE SUBJECT WAITS FOR ITS FIRST, and it waits
// BEFORE it opens a snapshot.
//
// Between a multi-append sequence's steps, this node's row is the one the
// earlier append MOVED while its record is still on the way to the applier. A
// decision from that row is already stale, so the broker refuses it and the
// framework waits for the position the writer held all along, a snapshot, a
// decide and a round trip later. [worklet.Writer.After] moves that wait ahead
// of all of it, as [statelog.Request.Session]'s producer.
func TestASecondWriteOnOneSubjectWaitsBeforeItSnapshots(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if _, err := r.writer.CreateTask(t.Context(), "op-1", newTask("t-1"), nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r.drain()

	first := "first"
	one, err := r.writer.UpdateTask(t.Context(), "op-2", "t-1", "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Title: &first}, worklet.ChangeFields, nil)
	if err != nil {
		t.Fatalf("the first write: %v", err)
	}
	// DELIBERATELY NOT DRAINED: the row is here, its record is not.

	second := "second"
	two, err := r.writer.After(one.Position).UpdateTask(t.Context(), "op-3", "t-1",
		"ENG", worklet.NoIfMatch, worklet.TaskPatch{Title: &second},
		worklet.ChangeFields, nil)
	var refusal *statelog.Unavailable
	switch {
	case err == nil:
		t.Fatal("a second write decided from a state below the caller's own first")
	case !errors.As(err, &refusal):
		t.Fatalf("the refusal is %v, want a named one a caller can act on", err)
	case refusal.Reason != statelog.ReasonBehind:
		t.Fatalf("the reason is %q, want %q", refusal.Reason, statelog.ReasonBehind)
	}
	// NO SNAPSHOT WAS OPENED, which is what the mark buys: the framework's own
	// recovery reaches the same wait only after a round decided from stale rows.
	if two.Rounds != 0 {
		t.Errorf("the write reports %d round(s), so it snapshotted and decided "+
			"before it found out it was behind its own previous write", two.Rounds)
	}
	// AND THE REFUSAL SAYS WHOSE WRITE IT WAITS FOR: "my applier is lagging"
	// and "a colleague is editing this" are one reason, two different remedies.
	if !strings.Contains(refusal.Detail, "own") {
		t.Errorf("the detail is %q and does not name the write as the "+
			"caller's own", refusal.Detail)
	}
}

// A DEPENDENCY NAMING BOTH DIRECTIONS WRITES ITS OWN TASK TWICE, and the
// second of those commits carries the first as its mark.
//
// Step 2 publishes this task's `waiting_on` edges and step 3 the dependents it
// gains, both on its own subject. A mirror that loses is collected as
// one-sided rather than raised, so the result shows nothing: the witness is the
// session-wait histogram, which nothing else in this package observes.
func TestADependencyOnBothSidesWaitsForItsOwnAuthoredCommit(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "mid")
	filedTask(t, r, "up")
	filedTask(t, r, "down")

	if _, err := r.writer.Depend(t.Context(), "op-both", worklet.DependencyChange{
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

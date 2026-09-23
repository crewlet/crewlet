package tracker_test

import (
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A DEPENDENCY CHANGE'S STEPS ARE NAMED BY THE TASK THEY WRITE, so a retry whose
// lists moved still answers each step with its own write.
//
// A caller recomputes a whole-set change against what the first run already
// landed, so a retry's lists are not the first run's: an edge that landed is
// not authored again, and every step after it moves up a place. Named by
// position, the moved step was answered with the ledger row of the step that
// used to hold its place — a row on another task, refused as an operation that
// landed elsewhere — and its own write was never made.
func TestADependencyRetryAnswersEachStepWithItsOwnWrite(t *testing.T) {
	t.Parallel()

	t.Run("a mirror that moved up a place", func(t *testing.T) {
		t.Parallel()
		r := newRoundTrip(t)
		r.applyWhileWriting()
		for _, id := range []string{"dep", "blk", "dnt"} {
			filedTask(t, r, id)
		}
		lossy, lost := r.lossyWriter(t)
		op := statelog.NewOpID(time.Now(), "depend")

		// THE FIRST RUN: this task now waits on blk and blocks dnt. Its
		// mirrors are blk's and then its own, and its own is refused — the
		// broker's log full — once blk's has landed.
		lost.afterAppendTo("blk", func() { lost.refuse("dep") })
		first, err := lossy.Depend(t.Context(), op, tracker.DependencyChange{
			Task: "dep", Project: "ENG", WaitingOnAdd: []string{"blk"},
			BlockingAdd: []string{"dnt"},
		}, fixedLeads{project: "eng-lead"})
		if err != nil || !slices.Contains(first.OneSided, "dep") {
			t.Fatalf("the premise: the first run = (%+v, %v), want dep's own "+
				"mirror one-sided", first, err)
		}
		lost.refuse("")
		r.drain()

		// THE RETRY, recomputed against what landed: blk's edge is already
		// there, so only the blocking half is left — and this task's own
		// mirror is now the first.
		retry, err := lossy.Depend(t.Context(), op, tracker.DependencyChange{
			Task: "dep", Project: "ENG", BlockingAdd: []string{"dnt"},
		}, fixedLeads{project: "eng-lead"})
		if err != nil {
			t.Fatalf("the retry: %v", err)
		}
		r.drain()
		if !slices.Contains(retry.Mirrored, "dep") {
			t.Errorf("the retry reports dep's own mirror %v / one-sided %v — it "+
				"was answered with blk's mirror, the step that held its place",
				retry.Mirrored, retry.OneSided)
		}
		if got := r.task(t, "dep").Task.Dependents; !slices.Contains(got, "dnt") {
			t.Errorf("dep lists dependents %v after the retry, want dnt", got)
		}
	})

	t.Run("an authored edge that moved up a place", func(t *testing.T) {
		t.Parallel()
		r := newRoundTrip(t)
		r.applyWhileWriting()
		for _, id := range []string{"dep", "z1", "z2"} {
			filedTask(t, r, id)
		}
		lossy, lost := r.lossyWriter(t)
		op := statelog.NewOpID(time.Now(), "depend")

		lost.refuse("z2")
		if _, err := lossy.Depend(t.Context(), op, tracker.DependencyChange{
			Task: "dep", Project: "ENG", BlockingAdd: []string{"z1", "z2"},
		}, fixedLeads{project: "eng-lead"}); err == nil {
			t.Fatal("the premise: a change whose second edge was refused succeeded")
		}
		lost.refuse("")
		r.drain()

		// THE RETRY, with z1's edge no longer in the list.
		if _, err := lossy.Depend(t.Context(), op, tracker.DependencyChange{
			Task: "dep", Project: "ENG", BlockingAdd: []string{"z2"},
		}, fixedLeads{project: "eng-lead"}); err != nil {
			t.Fatalf("the retry was refused — z2's edge was answered with z1's, "+
				"the step that held its place: %v", err)
		}
		r.drain()
		waits := slices.ContainsFunc(r.task(t, "z2").Task.Relations, func(rel tracker.Relation) bool {
			return rel.Kind == tracker.RelationWaitingOn && rel.Other == "dep"
		})
		if !waits {
			t.Error("z2 does not wait on dep after the retry")
		}
	})
}

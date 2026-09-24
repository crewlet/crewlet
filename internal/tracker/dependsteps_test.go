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

// A DEPENDENCY CHANGE REPORTS ITS OWN TASK'S VERSION, and an outcome whatever
// its mirrors did.
//
// The result's version is the LAST COMMIT's, and a dependency change's last
// commit is usually another task's — a blocker's mirror, a dependent's edge. A
// caller reporting it as the version of the task it named handed back an
// `if_match` that task never had, refused as stale. And a `blocking`-only
// change whose own mirror was left one-sided reported nothing at all: the
// authored edges on the other tasks were never counted, so it answered the
// zero result — an outcome that is none of the three, and no position.
func TestADependencyChangeReportsItsOwnTasksVersion(t *testing.T) {
	t.Parallel()

	t.Run("the waiting side, whose last commit is the blocker's", func(t *testing.T) {
		t.Parallel()
		r := newRoundTrip(t)
		// APPLIED, because a version is what an applied write reports.
		r.applyWhileWriting()
		filedTask(t, r, "dep")
		filedTask(t, r, "blk")
		got, err := r.writer.Depend(t.Context(), statelog.NewOpID(time.Now(), "depend"),
			tracker.DependencyChange{
				Task: "dep", Project: "ENG", WaitingOnAdd: []string{"blk"},
			}, fixedLeads{project: "eng-lead"})
		if err != nil || len(got.OneSided) != 0 {
			t.Fatalf("the premise: Depend = (%+v, %v), want both ends written", got, err)
		}
		r.drain()
		if got.Version == got.TaskVersion {
			t.Fatalf("the premise: the last commit is dep's own (%d), so this "+
				"case shows nothing", got.Version)
		}
		if want := int64(r.task(t, "dep").Task.Version); got.TaskVersion != want {
			t.Errorf("TaskVersion = %d, want dep's own version %d — %d is the "+
				"blocker's mirror", got.TaskVersion, want, got.Version)
		}
	})

	t.Run("a blocking-only change whose own mirror did not land", func(t *testing.T) {
		t.Parallel()
		r := newRoundTrip(t)
		r.applyWhileWriting()
		filedTask(t, r, "blk")
		filedTask(t, r, "dep")
		lossy, lost := r.lossyWriter(t)
		lost.afterAppendTo("dep", func() { lost.refuse("blk") })
		got, err := lossy.Depend(t.Context(), statelog.NewOpID(time.Now(), "depend"),
			tracker.DependencyChange{
				Task: "blk", Project: "ENG", BlockingAdd: []string{"dep"},
			}, fixedLeads{})
		if err != nil || !slices.Contains(got.OneSided, "blk") {
			t.Fatalf("the premise: Depend = (%+v, %v), want blk's own mirror "+
				"one-sided", got, err)
		}
		if !got.Outcome.Valid() || got.Position.Seq == 0 {
			t.Errorf("the change answered outcome %q at %+v — dep's authored edge "+
				"landed, and that commit is what it has to report", got.Outcome,
				got.Position)
		}
		if got.TaskVersion != 0 {
			t.Errorf("TaskVersion = %d, but nothing landed on blk's own subject",
				got.TaskVersion)
		}
	})
}

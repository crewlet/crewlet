package tracker_test

import (
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A TRASH GESTURE THAT STOPS AFTER ITS ROOT SAYS SO, AND A SECOND CALL FINISHES
// IT.
//
// Every other refusal a write returns means nothing was written, and a caller
// told that about a removal whose root is already in the trash reports a state
// that is not true. The partial gesture is typed so a caller can ask, and it
// says whether calling it again finishes it — asserted here by calling again
// under an operation of its own, which is what a caller's second call is.
//
// Mutation: return the walk's refusal as a plain error in RemoveTask or in
// RestoreTask and that half fails to find the type.
func TestATrashGestureThatStopsAfterItsRootIsPartialAndACallAgainFinishesIt(t *testing.T) {
	t.Parallel()
	broker := &refusingAppender{subject: tracker.TaskSubject("t-b").Wire()}
	r := newRoundTripAppending(t, func(a statelog.Appender) statelog.Appender {
		broker.Appender = a
		return broker
	})
	r.applyWhileWriting()
	subtreeOfThree(t, r)

	// THE REMOVAL, stopped at t-b.
	broker.refusing(true)
	_, err := r.writer.RemoveTask(t.Context(), "op-remove", "t-root", "ENG", true, nil)
	r.drain()
	broker.refusing(false)
	assertPartial(t, "the removal", err, true)
	if !removed(t, r, "t-root") || !removed(t, r, "t-a") || removed(t, r, "t-b") {
		t.Fatal("the removal did not stop between t-a and t-b, so this case is " +
			"not the shape it names")
	}
	if _, err := r.writer.RemoveTask(t.Context(), "op-remove-again", "t-root",
		"ENG", true, nil); err != nil {
		t.Fatalf("the second removal: %v", err)
	}
	r.drain()
	if !removed(t, r, "t-b") {
		t.Error("t-b is live after a second removal of its parent reported " +
			"success, and the partial error said a second call finishes it")
	}

	// THE RESTORE, stopped at t-b the same way.
	broker.refusing(true)
	_, err = r.writer.RestoreTask(t.Context(), "op-restore", "t-root", "ENG", nil)
	r.drain()
	broker.refusing(false)
	assertPartial(t, "the restore", err, true)
	if removed(t, r, "t-root") || removed(t, r, "t-a") || !removed(t, r, "t-b") {
		t.Fatal("the restore did not stop between t-a and t-b, so this case is " +
			"not the shape it names")
	}
	// A LIVE ROOT, and the second call still finishes: its root step has
	// nothing to do and its walk brings back what is left.
	if _, err := r.writer.RestoreTask(t.Context(), "op-restore-again", "t-root",
		"ENG", nil); err != nil {
		t.Fatalf("the second restore: %v", err)
	}
	r.drain()
	if removed(t, r, "t-b") {
		t.Error("t-b is still in the trash after a second restore of its " +
			"parent reported success")
	}
}

// A GESTURE REFUSED AT ITS ROOT WROTE NOTHING, AND IS NOT PARTIAL.
//
// The control for the case above: a type every trash refusal carried would say
// "part of this landed" about a removal that wrote nothing at all.
//
// Mutation: type the root's refusal as partial and this fails.
func TestAGestureRefusedAtItsRootIsNotPartial(t *testing.T) {
	t.Parallel()
	broker := &refusingAppender{subject: tracker.TaskSubject("t-root").Wire()}
	r := newRoundTripAppending(t, func(a statelog.Appender) statelog.Appender {
		broker.Appender = a
		return broker
	})
	r.applyWhileWriting()
	subtreeOfThree(t, r)

	broker.refusing(true)
	_, err := r.writer.RemoveTask(t.Context(), "op-remove", "t-root", "ENG", true, nil)
	r.drain()
	if err == nil {
		t.Fatal("the removal reported success with its root refused")
	}
	var part *tracker.PartialError
	if errors.As(err, &part) {
		t.Errorf("a removal refused at its root is typed as partial: %v", err)
	}
	for _, id := range []string{"t-root", "t-a", "t-b"} {
		if removed(t, r, id) {
			t.Errorf("%s is in the trash after a removal refused at its root", id)
		}
	}
}

// A MERGE THAT STOPS AFTER ITS MARK IS PARTIAL, AND LEFT TO THE DUTY.
//
// The mark is the merge's first commit: from it on, the duplicate is visibly
// mid-merge and the tracker duty completes what the call did not. So it is
// partial — the duplicate is marked — and not a gesture a second call is what
// finishes.
//
// Mutation: return the walk's refusal from MergeDuplicates as it came and the
// merge reads as one that wrote nothing.
func TestAMergeThatStopsAfterItsMarkIsPartialAndLeftToTheDuty(t *testing.T) {
	t.Parallel()
	broker := &refusingAppender{subject: tracker.TaskSubject("kid").Wire()}
	r := newRoundTripAppending(t, func(a statelog.Appender) statelog.Appender {
		broker.Appender = a
		return broker
	})
	r.applyWhileWriting()
	filedTask(t, r, "keep")
	filedTask(t, r, "dup")
	parent := "dup"
	child := newTask("kid")
	child.Key = "ENG-kid"
	child.Parent, child.Depth = &parent, 1
	if _, err := r.writer.CreateTask(t.Context(), "op-kid", child, nil); err != nil {
		t.Fatalf("CreateTask kid: %v", err)
	}
	r.drain()

	broker.refusing(true)
	_, err := r.writer.MergeDuplicates(t.Context(), "op-merge", "dup", "keep", true, nil)
	r.drain()
	broker.refusing(false)
	assertPartial(t, "the merge", err, false)
	if !r.task(t, "dup").Task.Merging {
		t.Error("the duplicate is not marked as merging, so the merge did not " +
			"stop after its mark and this case is not the shape it names")
	}
}

// A CROSS-PROJECT MOVE THAT STOPS AFTER ITS ROOT IS PARTIAL, AND A SECOND CALL
// DOES NOT FINISH IT: the root has moved, so a second call is refused.
//
// Mutation: return either arm of the move's walk as a plain error and that
// case fails to find the type.
func TestAMoveThatStopsAfterItsRootIsPartialAndNotFinishedByACallAgain(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// broker is what the descendant's own commit meets: a refusal, or
		// no answer at all, which makes its outcome `unknown`.
		broker func(subject string) (statelog.Appender, func(bool),
			func(statelog.Appender))
	}{
		{"a refused descendant", func(subject string) (statelog.Appender,
			func(bool), func(statelog.Appender)) {
			a := &refusingAppender{subject: subject}
			return a, a.refusing, func(inner statelog.Appender) { a.Appender = inner }
		}},
		{"an unanswered descendant", func(subject string) (statelog.Appender,
			func(bool), func(statelog.Appender)) {
			a := &silentAppender{subject: subject}
			return a, a.quiet, func(inner statelog.Appender) { a.Appender = inner }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			broker, stop, wrap := tc.broker(tracker.TaskSubject("m-kid").Wire())
			r := newRoundTripAppending(t, func(a statelog.Appender) statelog.Appender {
				wrap(a)
				return broker
			})
			r.applyWhileWriting()
			if _, err := r.writer.WriteDocument(t.Context(), "op-ops",
				tracker.ProjectSubject("OPS"), "", tracker.Project{
					V: 1, Key: "OPS", Name: "Operations",
					CreatedAt: wednesday, UpdatedAt: wednesday,
				}, tracker.ChangeProjectCreated, nil); err != nil {
				t.Fatalf("seed the target project: %v", err)
			}
			r.drain()
			if _, err := r.writer.CreateTask(t.Context(), "op-root",
				newTask("m-root"), nil); err != nil {
				t.Fatalf("CreateTask root: %v", err)
			}
			r.drain()
			kid := newTask("m-kid")
			kid.Key = "ENG-2"
			parent := "m-root"
			kid.Parent, kid.Depth = &parent, 1
			if _, err := r.writer.CreateTask(t.Context(), "op-kid", kid, nil); err != nil {
				t.Fatalf("CreateTask kid: %v", err)
			}
			r.drain()

			stop(true)
			_, err := r.writer.MoveTaskToProject(t.Context(), "op-move", "m-root", "OPS", nil)
			r.drain()
			stop(false)
			assertPartial(t, "the move", err, false)
			if got := oneTask(t, r, "m-root").Project; got != "OPS" {
				t.Fatalf("the root is in %s, so the move did not stop after its "+
					"root and this case is not the shape it names", got)
			}
		})
	}
}

// A PROMOTION THAT STOPS AFTER ITS SUBTASK IS PARTIAL, AND A SECOND CALL
// FINISHES IT — TestARerunPromotionMarksTheParentItDidNotReach is that second
// call.
//
// Mutation: return the parent mark's refusal as a plain error and this fails
// to find the type.
func TestAPromotionThatStopsAfterItsSubtaskIsPartial(t *testing.T) {
	t.Parallel()
	broker := &refusingAppender{subject: tracker.TaskSubject("t-1").Wire()}
	r := newRoundTripAppending(t, func(a statelog.Appender) statelog.Appender {
		broker.Appender = a
		return broker
	})
	parent := newTask("t-1")
	parent.Checklists = []tracker.Checklist{{
		ID: "l-1", Name: "steps",
		Items: []tracker.ChecklistItem{{ID: "i-1", Name: "wire it"}},
	}}
	if _, err := r.writer.CreateTask(t.Context(), "op-1", parent, nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r.drain()

	broker.refusing(true)
	_, err := r.writer.PromoteItem(t.Context(), "op-2", "t-1", "i-1", newTask("t-2"), nil)
	r.drain()
	broker.refusing(false)
	assertPartial(t, "the promotion", err, true)
	if oneTask(t, r, "t-2").Parent == nil {
		t.Fatal("the subtask was not filed, so the promotion did not stop " +
			"after it and this case is not the shape it names")
	}
}

// assertPartial fails unless err is a [tracker.PartialError] whose Rerun is
// rerun.
func assertPartial(t *testing.T, what string, err error, rerun bool) {
	t.Helper()
	var part *tracker.PartialError
	switch {
	case err == nil:
		t.Fatalf("%s reported success with one of its commits refused", what)
	case !errors.As(err, &part):
		t.Fatalf("%s stopped after its first commit landed and is not typed as "+
			"partial: %v — a caller reads it as a change that was not made", what, err)
	case part.Rerun != rerun:
		t.Errorf("%s says a second call finishes it = %v, want %v: %v",
			what, part.Rerun, rerun, err)
	}
}

// A PARTIAL ERROR CARRYING NO ACCOUNT STILL PRINTS, rather than panicking in
// the frame that reports it — its zero value is a value a caller outside this
// package can build.
//
// Mutation: read the account without checking it is there and this panics.
func TestAPartialErrorWithNoAccountStillPrints(t *testing.T) {
	t.Parallel()
	var zero tracker.PartialError
	if got := zero.Error(); got == "" {
		t.Error("a PartialError with no account prints nothing")
	}
	if zero.Unwrap() != nil {
		t.Error("a PartialError with no account unwraps to a cause")
	}
}

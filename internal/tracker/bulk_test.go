package tracker_test

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A RE-RUN OF A BULK EDIT ANSWERS EACH TASK WITH ITS OWN RECORD, whatever
// order it names them in.
//
// Each task's step was numbered by its place in the list, and the ledger
// answers a step's id with whatever it recorded under it — so a re-run naming
// the tasks in another order, or only the ones that failed, answered one task
// with another's record and reported it changed when nothing had touched it.
func TestARerunBulkEditAnswersEachTaskWithItsOwnRecord(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.applyWhileWriting()
	filedTask(t, r, "t-1")
	filedTask(t, r, "t-2")
	op := statelog.NewOpID(time.Now(), "bulk")
	done := tracker.StatusDone
	closeTasks := func(ids ...string) tracker.WriteResult {
		t.Helper()
		result, err := r.writer.UpdateTasks(t.Context(), op, ids, "ENG",
			tracker.TaskPatch{Status: &done}, tracker.ChangeStatus, nil)
		if err != nil {
			t.Fatalf("UpdateTasks %v: %v", ids, err)
		}
		return result
	}

	// THE FIRST RUN reached only t-1.
	closeTasks("t-1")
	r.drain()
	// THE RE-RUN names both, t-2 first.
	result := closeTasks("t-2", "t-1")
	r.drain()
	if len(result.Applied) != 2 || len(result.Failed) != 0 {
		t.Fatalf("the re-run applied %v and failed %v, want both applied",
			result.Applied, result.Failed)
	}
	if got := taskOf(t, r, "t-2").Status; got != tracker.StatusDone {
		t.Errorf("t-2 is %q after a re-run that reported it applied — its step "+
			"was answered with t-1's record", got)
	}
}

// A TASK WHOSE CHANGE HAS AN UNKNOWN OUTCOME IS A FAILURE, NOT AN APPLICATION,
// and the re-run under the same operation id answers it.
func TestABulkEditReportsAnUnknownOutcomeAsAFailure(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.applyWhileWriting()
	filedTask(t, r, "t-1")
	lossy, lost := r.lossyWriter(t)
	op := statelog.NewOpID(time.Now(), "bulk")
	done := tracker.StatusDone

	// THE APPEND LANDS AND ITS ANSWER IS LOST, and so is the probe's.
	lost.drop(2)
	first, err := lossy.UpdateTasks(t.Context(), op, []string{"t-1"}, "ENG",
		tracker.TaskPatch{Status: &done}, tracker.ChangeStatus, nil)
	if err != nil {
		t.Fatalf("UpdateTasks: %v", err)
	}
	if len(first.Applied) != 0 || !strings.Contains(first.Failed["t-1"], "unknown") {
		t.Fatalf("a change with an unknown outcome = applied %v, failed %v — "+
			"counting it applied tells the caller to stop retrying a change "+
			"that may never have landed", first.Applied, first.Failed)
	}
	r.drain()
	end := r.logEnd(t)

	retry, err := r.writer.UpdateTasks(t.Context(), op, []string{"t-1"}, "ENG",
		tracker.TaskPatch{Status: &done}, tracker.ChangeStatus, nil)
	if err != nil || len(retry.Applied) != 1 {
		t.Fatalf("the re-run = (applied %v, failed %v, %v), want t-1 applied",
			retry.Applied, retry.Failed, err)
	}
	if got := r.logEnd(t); got != end {
		t.Errorf("the re-run put %d record(s) on the log for a change that had "+
			"landed", got-end)
	}
}

// A TASK OUTSIDE THE PROJECT A WRITE NAMES IS REFUSED, not written under a
// scope that names another container — which is what a bulk edit's one
// project did to every task in the list that lived elsewhere.
func TestAWriteNamingTheWrongProjectIsRefused(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.applyWhileWriting()
	filedTask(t, r, "t-1")
	if _, err := r.writer.WriteDocument(t.Context(), "op-ops",
		tracker.ProjectSubject("OPS"), "", tracker.Project{
			V: 1, Key: "OPS", Name: "Operations",
			CreatedAt: wednesday, UpdatedAt: wednesday,
		}, tracker.ChangeProjectCreated, nil); err != nil {
		t.Fatalf("file the second project: %v", err)
	}
	r.drain()
	elsewhere := newTask("o-1")
	elsewhere.Project = "OPS"
	if _, err := r.writer.CreateTask(t.Context(), "op-o-1", elsewhere, nil); err != nil {
		t.Fatalf("file a task in OPS: %v", err)
	}
	r.drain()
	end := r.logEnd(t)

	done := tracker.StatusDone
	result, err := r.writer.UpdateTasks(t.Context(), "op-bulk",
		[]string{"t-1", "o-1"}, "ENG",
		tracker.TaskPatch{Status: &done}, tracker.ChangeStatus, nil)
	if err != nil {
		t.Fatalf("UpdateTasks: %v", err)
	}
	if len(result.Applied) != 1 || result.Applied[0] != "t-1" {
		t.Errorf("the batch applied %v, want t-1 alone", result.Applied)
	}
	if reason := result.Failed["o-1"]; !strings.Contains(reason, "project OPS") {
		t.Errorf("o-1's failure is %q, want one naming the project it is in", reason)
	}
	r.drain()
	if got := r.logEnd(t); got != end+1 {
		t.Errorf("the batch put %d record(s) on the log, want t-1's one", got-end)
	}
	if got := taskOf(t, r, "o-1").Status; got == tracker.StatusDone {
		t.Error("o-1 was closed under a scope naming a project it is not in")
	}
}

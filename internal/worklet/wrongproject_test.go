package worklet_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A TASK OUTSIDE THE PROJECT A WRITE NAMES IS REFUSED, not written under a
// scope that names another container. The caller resolved the task and read
// its project outside the write's snapshot, so a task moved since — or named
// with the wrong project — would otherwise be decided on one project's rules
// and filed under its container while it lives in another.
func TestAWriteNamingTheWrongProjectIsRefused(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.applyWhileWriting()
	filedTask(t, r, "t-1")
	if _, err := r.writer.WriteDocument(t.Context(), "op-ops",
		worklet.ProjectSubject("OPS"), "", worklet.Project{
			V: 1, Key: "OPS", Name: "Operations",
			CreatedAt: wednesday, UpdatedAt: wednesday,
		}, worklet.ChangeProjectCreated, nil); err != nil {
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

	done := worklet.StatusDone
	_, err := r.writer.UpdateTask(t.Context(), "op-close-o-1", "o-1", "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Status: &done}, worklet.ChangeStatus, nil)
	switch {
	case err == nil:
		t.Fatal("a write naming ENG closed a task that lives in OPS")
	case !errors.Is(err, statelog.ErrConflict):
		t.Fatalf("the refusal is %v, want a conflict: nothing is wrong with the "+
			"request, the caller reads the task again and decides on its project", err)
	case !strings.Contains(err.Error(), "project OPS"):
		t.Fatalf("the refusal is %v, want one naming the project the task is in", err)
	}
	r.drain()
	if got := r.logEnd(t); got != end {
		t.Errorf("the refused write put %d record(s) on the log", got-end)
	}
	if got := taskOf(t, r, "o-1").Status; got == worklet.StatusDone {
		t.Error("o-1 was closed under a scope naming a project it is not in")
	}

	// THE CONTROL: the same write naming the task's own project lands.
	if _, err := r.writer.UpdateTask(t.Context(), "op-close-t-1", "t-1", "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Status: &done}, worklet.ChangeStatus, nil); err != nil {
		t.Fatalf("close t-1 in its own project: %v", err)
	}
	r.drain()
	if got := taskOf(t, r, "t-1").Status; got != worklet.StatusDone {
		t.Errorf("t-1 is %q after a close naming its own project, want done", got)
	}
}

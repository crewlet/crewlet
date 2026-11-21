package executor_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// AN INBOX ID IS THE JOB'S AND THE NOTE'S, AND STABLE ([executor.OpenCodeInboxID]).
func TestAnOpenCodeInboxIDIsStableAndTheJobsOwn(t *testing.T) {
	t.Parallel()
	id := executor.OpenCodeInboxID("job-1", "note-1")
	if id != executor.OpenCodeInboxID("job-1", "note-1") {
		t.Error("one note's id is not stable for its job")
	}
	if !strings.HasPrefix(id, "msg_") || len(id) != len("msg_")+32 {
		t.Errorf("the id %q is not msg_ and 32 hex digits", id)
	}
	for _, other := range []string{
		executor.OpenCodeInboxID("job-2", "note-1"),
		executor.OpenCodeInboxID("job-1", "note-2"),
		executor.OpenCodeInboxID("job-1note-1", ""),
	} {
		if other == id {
			t.Errorf("two notes or two jobs share the id %q", id)
		}
	}
}

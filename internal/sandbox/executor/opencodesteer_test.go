package executor_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// AN INBOX ID IS THE JOB'S AND THE NOTE'S, AND STABLE ([executor.InboxID]).
func TestAnOpenCodeInboxIDIsStableAndTheJobsOwn(t *testing.T) {
	t.Parallel()
	id := executor.InboxID(executor.OpenCodeName, "job-1", "note-1")
	if id != executor.InboxID(executor.OpenCodeName, "job-1", "note-1") {
		t.Error("one note's id is not stable for its job")
	}
	if !strings.HasPrefix(id, "msg_") || len(id) != len("msg_")+32 {
		t.Errorf("the id %q is not msg_ and 32 hex digits", id)
	}
	for _, other := range []string{
		executor.InboxID(executor.OpenCodeName, "job-2", "note-1"),
		executor.InboxID(executor.OpenCodeName, "job-1", "note-2"),
		executor.InboxID(executor.OpenCodeName, "job-1note-1", ""),
	} {
		if other == id {
			t.Errorf("two notes or two jobs share the id %q", id)
		}
	}
}

// THE IDS ARE THEIR DERIVATION, PINNED: a note's inbox id and its second id
// for one job, as the plugin admits them and a 2.3.2 session's messages carry
// them, so a change to either derivation, which orphans every admission a
// running plugin made, cannot pass unseen.
//
// Mutation: change the hash's input or its cut, or derive the second id
// without its suffix, and this goes red.
func TestAnOpenCodeNotesIDsAreTheirDerivation(t *testing.T) {
	t.Parallel()
	if got := executor.InboxID(executor.OpenCodeName, "job-1", "note-1"); got != "msg_851130aa607ac525a700344a00ecb940" {
		t.Errorf("note-1's inbox id for job-1 is %q", got)
	}
	if got := executor.AgainID(executor.OpenCodeName, "job-1", "note-1"); got != "msg_f2364a7946a1a890cdecc9a36e911001" {
		t.Errorf("note-1's second id for job-1 is %q", got)
	}
}

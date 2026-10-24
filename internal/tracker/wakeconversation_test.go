package tracker_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A TASK'S CONVERSATION IS THE TASK, BY ITS ID — never the key it answers to.
//
// Two facts about a key make it the wrong name for a thread. Two tasks can
// hold one: a counter restored beside newer work mints numbers they already
// hold, and the applier flags every holder but the claimant `key_collision`.
// Keyed on the key, a wake about the duplicate coalesced with the claimant's
// into one digest and filed its turn in the claimant's ledger, so the seat was
// handed another task's thread as its own. And a key moves: a cross-project
// move re-keys the task, and keyed on the key its conversation started over
// under the new one, every earlier turn filed where the next never looked.
//
// THROUGH THE PARSER AND THE SPINE'S OWN NAMESPACING, because the partition a
// seat's inbox groups on is what the parser stamps and the registry composes,
// and a case that called PartitionKey with metadata it built itself would pass
// whatever the parser put there.
//
// Mutation: key PartitionKey on MetaTaskKey again and both halves fail — the
// two holders of ENG-7 share one conversation, and the moved task has two.
func TestATasksConversationIsItsIDNotItsKey(t *testing.T) {
	t.Parallel()
	prompts := notify.Prompts{}.With(tracker.Prompt{})
	woken := func(task, key, opID string) notify.Inbound {
		t.Helper()
		record := parseRecord(&tracker.Notify{
			Kind:    tracker.ChangeComment,
			Excerpt: "which one is this?",
			Snapshot: tracker.Snapshot{
				Key: key, Project: "ENG", Title: "the work",
				Status: tracker.StatusTodo, Assignee: "ana",
				CommentAuthorKind: tracker.AuthorHuman,
			},
		})
		record.Subject, record.OpID, record.Actor = tracker.TaskSubject(task), opID, "bo"
		routed, err := tracker.NewParser(tracker.ParserOptions{}).Parse(
			t.Context(), delivery(t, record), registry(t, "ana", "bo"))
		if err != nil {
			t.Fatalf("parse the wake about %s: %v", task, err)
		}
		for _, r := range routed {
			if r.To.Handle == "ana" {
				return r.Inbound
			}
		}
		t.Fatalf("the wake about %s reached %+v and not its assignee", task, routed)
		return notify.Inbound{}
	}
	conversation := func(n notify.Inbound) (partition, identity string) {
		return prompts.Partition(n), prompts.Conversation(n)
	}

	claimant, claimantID := conversation(woken("t-1", "ENG-7", "op-1"))
	duplicate, duplicateID := conversation(woken("t-2", "ENG-7", "op-2"))
	if claimant == "" || duplicate == "" {
		t.Fatalf("a task wake derived no partition (%q, %q), so it would never "+
			"be merged with anything and no ledger could hold it", claimant, duplicate)
	}
	if claimant == duplicate {
		t.Errorf("two tasks holding ENG-7 partition together on %q — a seat "+
			"woken about the duplicate is handed one digest of both tasks' "+
			"changes", claimant)
	}
	if claimantID == duplicateID {
		t.Errorf("two tasks holding ENG-7 share the conversation %q — the "+
			"duplicate's turns are filed in the claimant's ledger, and each "+
			"seat reads the other task's thread as its own", claimantID)
	}
	again, againID := conversation(woken("t-2", "ENG-7", "op-3"))
	if again != duplicate || againID != duplicateID {
		t.Errorf("two wakes about one task are (%q, %q) and (%q, %q) — a "+
			"task's own comments are one conversation", duplicate,
			duplicateID, again, againID)
	}

	// A MOVE RE-KEYS THE TASK AND KEEPS ITS THREAD.
	before, beforeID := conversation(woken("t-3", "ENG-9", "op-4"))
	after, afterID := conversation(woken("t-3", "OPS-3", "op-5"))
	if before != after || beforeID != afterID {
		t.Errorf("t-3 was the conversation (%q, %q) as ENG-9 and (%q, %q) as "+
			"OPS-3 — a move started its thread over, and every turn before it "+
			"is filed where the next one never looks", before, beforeID,
			after, afterID)
	}
}

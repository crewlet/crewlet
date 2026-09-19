package tracker_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// handlesOf is n distinct handles, for the cases that are about a COUNT.
func handlesOf(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("person-%03d", i)
	}
	return out
}

// EVERY COLLECTION A ROUTING SNAPSHOT CARRIES IS BOUNDED AT THE PUBLISH
// BOUNDARY.
//
// [tracker.Notify.Validate] read `Kind`, `Fields` and `Excerpt` and NOTHING on
// the snapshot, so ten collections went onto the log unbounded. That is not a
// screen rendering badly: a snapshot is copied onto the log, replicated to
// every node, held for the stream's whole retention window and read back by
// every applier — and the design's own arithmetic sums these caps to about
// 19 KiB worst and sizes MaxCommitBytes from it. The caps that held were
// incidental properties of whichever builder happened to read a bounded table.
//
// THE WALK IS PER FIELD, because a check that bounded nine of ten would pass
// any case written about the interesting one.
func TestEverySnapshotCollectionIsBounded(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		field string
		max   int
		over  func(*tracker.Notify)
	}{
		{"watchers", tracker.MaxWatchers, func(n *tracker.Notify) {
			n.Snapshot.Watchers = handlesOf(tracker.MaxWatchers + 1)
		}},
		{"removed_watchers", tracker.MaxWatchers, func(n *tracker.Notify) {
			n.Kind = tracker.ChangeWatchers
			n.Snapshot.RemovedWatchers = handlesOf(tracker.MaxWatchers + 1)
		}},
		{"collaborators", tracker.MaxCollaborators, func(n *tracker.Notify) {
			n.Snapshot.Collaborators = handlesOf(tracker.MaxCollaborators + 1)
		}},
		{"thread_participants", tracker.MaxThreadParticipants, func(n *tracker.Notify) {
			n.Snapshot.ThreadParticipants = handlesOf(tracker.MaxThreadParticipants + 1)
		}},
		{"checklist_assignees", tracker.MaxChecklists, func(n *tracker.Notify) {
			n.Snapshot.ChecklistAssignees = handlesOf(tracker.MaxChecklists + 1)
		}},
		{"goal_owners", tracker.MaxGoalOwners, func(n *tracker.Notify) {
			n.Snapshot.GoalOwners = handlesOf(tracker.MaxGoalOwners + 1)
		}},
		{"goal_members", tracker.MaxGoalMembers, func(n *tracker.Notify) {
			n.Snapshot.GoalMembers = handlesOf(tracker.MaxGoalMembers + 1)
		}},
		{"mentions", tracker.MaxMentions, func(n *tracker.Notify) {
			n.Mentions = handlesOf(tracker.MaxMentions + 1)
		}},
		{"unblocked", tracker.MaxDependents, func(n *tracker.Notify) {
			n.Snapshot.Unblocked = taskParties(tracker.MaxDependents + 1)
		}},
		{"dependents", tracker.MaxDependents, func(n *tracker.Notify) {
			n.Snapshot.Dependents = taskParties(tracker.MaxDependents + 1)
		}},
	} {
		t.Run(tc.field, func(t *testing.T) {
			t.Parallel()
			over := &tracker.Notify{Kind: tracker.ChangeFields}
			tc.over(over)
			err := over.Validate()
			if err == nil {
				t.Fatalf("a notification naming %d %s was accepted against a "+
					"cap of %d — the snapshot goes onto the log, to every "+
					"node, for the stream's whole retention window",
					tc.max+1, tc.field, tc.max)
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Errorf("the refusal does not name %s: %v", tc.field, err)
			}

			// AND AT THE CAP IT IS ACCEPTED, so this bounds the
			// collection rather than forbidding it.
			at := &tracker.Notify{Kind: tracker.ChangeFields}
			tc.over(at)
			trimToCap(at, tc.field, tc.max)
			if err := at.Validate(); err != nil {
				t.Errorf("a notification naming exactly %d %s was refused: %v",
					tc.max, tc.field, err)
			}
		})
	}
}

func taskParties(n int) []tracker.TaskParty {
	out := make([]tracker.TaskParty, n)
	for i := range out {
		out[i] = tracker.TaskParty{
			Task: fmt.Sprintf("t-%03d", i), Key: fmt.Sprintf("ENG-%d", i),
			Assignee: fmt.Sprintf("person-%03d", i),
		}
	}
	return out
}

func trimToCap(n *tracker.Notify, field string, max int) {
	switch field {
	case "watchers":
		n.Snapshot.Watchers = n.Snapshot.Watchers[:max]
	case "removed_watchers":
		n.Snapshot.RemovedWatchers = n.Snapshot.RemovedWatchers[:max]
	case "collaborators":
		n.Snapshot.Collaborators = n.Snapshot.Collaborators[:max]
	case "thread_participants":
		n.Snapshot.ThreadParticipants = n.Snapshot.ThreadParticipants[:max]
	case "checklist_assignees":
		n.Snapshot.ChecklistAssignees = n.Snapshot.ChecklistAssignees[:max]
	case "goal_owners":
		n.Snapshot.GoalOwners = n.Snapshot.GoalOwners[:max]
	case "goal_members":
		n.Snapshot.GoalMembers = n.Snapshot.GoalMembers[:max]
	case "mentions":
		n.Mentions = n.Mentions[:max]
	case "unblocked":
		n.Snapshot.Unblocked = n.Snapshot.Unblocked[:max]
	case "dependents":
		n.Snapshot.Dependents = n.Snapshot.Dependents[:max]
	}
}

// AN UNWATCH IS NEVER REFUSED, whatever the set is holding.
//
// The cap check ran on BOTH branches of the watch gesture, over a set an
// unwatch can only shrink. So a task that had grown past MaxWatchers — which
// the whole-set spelling could do, since nothing bounded it — was one nobody
// could leave: the only gesture that could have brought it back under the cap
// was the one being refused, for being over it.
func TestAnUnwatchIsNeverRefusedHoweverManyAreWatching(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	task := newTask("t-1")
	// AT THE CAP, which a create may set: the gesture's check is about
	// growth past it, and this fixture is what growth would start from.
	task.Watchers = handlesOf(tracker.MaxWatchers)
	if _, err := r.writer.CreateTask(t.Context(), "op-t-1", task, nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	r.drain()

	// GROWING PAST IT IS REFUSED, which is the cap doing its job.
	if _, err := r.writer.UpdateTask(t.Context(), "op-watch", "t-1", "ENG",
		tracker.NoIfMatch,
		tracker.TaskPatch{Watch: &tracker.WatchIntent{
			Handle: "one-more", Watch: true,
		}}, tracker.ChangeWatchers, nil); err == nil {
		t.Error("a sixty-fifth explicit watch was accepted")
	}

	// AND LEAVING IS NOT, which is the half that was broken.
	if _, err := r.writer.UpdateTask(t.Context(), "op-unwatch", "t-1", "ENG",
		tracker.NoIfMatch,
		tracker.TaskPatch{Watch: &tracker.WatchIntent{
			Handle: "person-000", Watch: false,
		}}, tracker.ChangeWatchers, nil); err != nil {
		t.Fatalf("an unwatch on a full task was refused: %v — the set can "+
			"only shrink here, and refusing it leaves a task nobody can "+
			"leave and nothing can bring back under the cap", err)
	}
	r.drain()
}

// AN AUTOMATIC WATCH IS SKIPPED AT THE CAP, never refused — which is what lets
// somebody comment on a task sixty-four people are already watching.
//
// MaxWatchers' own doc states the two modes ("an explicit sixty-fifth watch is
// refused, and an automatic one is skipped") and neither was implemented for
// the comment path, which sent the whole watcher set instead and checked
// nothing.
func TestAnAutomaticWatchIsSkippedRatherThanRefusingTheWrite(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	task := newTask("t-1")
	task.Watchers = handlesOf(tracker.MaxWatchers)
	if _, err := r.writer.CreateTask(t.Context(), "op-t-1", task, nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	r.drain()

	if _, err := r.writer.UpdateTask(t.Context(), "op-comment", "t-1", "ENG",
		tracker.NoIfMatch,
		tracker.TaskPatch{
			Comment: &tracker.Comment{
				ID: "c-1", Task: "t-1", Author: "newcomer",
				AuthorKind: tracker.AuthorHuman, Body: "a thought",
				CreatedAt: wednesday,
			},
			Watch: &tracker.WatchIntent{
				Handle: "newcomer", Watch: true, Auto: true,
			},
		}, tracker.ChangeComment, nil); err != nil {
		t.Fatalf("a comment on a task at the watcher cap was refused: %v — "+
			"the comment has nothing to do with how many people watch", err)
	}
	r.drain()

	// THE COMMENT LANDED AND THE WATCH DID NOT.
	detail, err := r.reader.Task(t.Context(), "t-1",
		tracker.DetailWants{Comments: true}, statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		t.Fatalf("read the task back: %v", err)
	}
	if len(detail.Comments) != 1 {
		t.Errorf("%d comments, want the one that was posted", len(detail.Comments))
	}
	for _, w := range detail.Task.Watchers {
		if w == "newcomer" {
			t.Error("the automatic watch was applied past the cap — the cap " +
				"is what stops a task becoming a broadcast")
		}
	}
}

// A TASK'S OWN TEXT IS REFUSED AT THE WRITE, WHICH IS WHAT THE CAPS SAID ALL
// ALONG AND NOTHING DID.
//
// [tracker.MaxTitle], [tracker.MaxBody] and [tracker.MaxCommentBody] are
// declared under a sentence promising each is "refused at WRITE naming the
// field, never cut" — and not one of them had a comparison anywhere in the
// tree. A search found the three constants, one alias and four doc comments.
// The nearest bound that fired was MaxCommitBytes, about forty times these
// and phrased about the record rather than the field, so a caller past a cap
// got either silence or a number they could not act on.
//
// The comment is where the cost showed: the thread page elides at
// [tracker.CommentBodyShown] BECAUSE a whole body may be large, and the
// single-comment read exists to return the rest. Both are sized against
// MaxCommentBody, so a body stored past it was elided in the page and too
// heavy for the read that would have returned it — reachable through no tool
// in the engine. The cap is what makes that elision a pointer instead of a
// loss.
//
// PER FIELD, because a check that bounded two of three would pass any case
// written about the interesting one.
func TestATasksOwnTextIsRefusedPastItsCap(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		field string
		max   int
		// write applies a value of n bytes and returns what the writer said.
		write func(*roundTrip, tracker.Task, int) error
	}{
		{"title", tracker.MaxTitle, func(r *roundTrip, task tracker.Task, n int) error {
			_, err := r.writer.UpdateTask(r.t.Context(), "op-title", task.ID, "ENG",
				tracker.NoIfMatch, tracker.TaskPatch{Title: strptr(strings.Repeat("t", n))},
				tracker.ChangeFields, nil)
			return err
		}},
		{"body", tracker.MaxBody, func(r *roundTrip, task tracker.Task, n int) error {
			_, err := r.writer.UpdateTask(r.t.Context(), "op-body", task.ID, "ENG",
				tracker.NoIfMatch, tracker.TaskPatch{Body: strptr(strings.Repeat("b", n))},
				tracker.ChangeFields, nil)
			return err
		}},
		{"comment body", tracker.MaxCommentBody, func(r *roundTrip, task tracker.Task, n int) error {
			_, err := r.writer.UpdateTask(r.t.Context(), "op-comment", task.ID, "ENG",
				tracker.NoIfMatch, tracker.TaskPatch{Comment: &tracker.Comment{
					ID: "cm-cap", Task: task.ID, Author: "ana",
					AuthorKind: tracker.AuthorHuman,
					Body:       strings.Repeat("c", n), CreatedAt: wednesday,
				}}, tracker.ChangeComment, nil)
			return err
		}},
	} {
		t.Run(tc.field, func(t *testing.T) {
			t.Parallel()
			r := newRoundTrip(t)
			task := r.createTask("A task to write text onto")

			// AT THE CAP IT LANDS, which is the half that keeps the
			// refusal from being a cap somebody quietly lowered.
			if err := tc.write(r, task, tc.max); err != nil {
				t.Fatalf("a %s of exactly %d bytes was refused: %v",
					tc.field, tc.max, err)
			}
			r.drain()

			// ONE BYTE PAST IT IS REFUSED, NAMING THE FIELD — because a
			// caller told only that "the record is too large" cannot tell
			// which of the eleven things it sent was the problem.
			err := tc.write(r, task, tc.max+1)
			if err == nil {
				t.Fatalf("a %s of %d bytes was accepted against a cap of %d — "+
					"stored past what any read returns whole, which is where "+
					"text goes to become unreachable", tc.field, tc.max+1, tc.max)
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Errorf("the refusal does not name %q, so a caller cannot "+
					"tell which value to shorten: %v", tc.field, err)
			}
		})
	}
}

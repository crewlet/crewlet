package worklet_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
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
// [worklet.Notify.Validate] read `Kind`, `Fields` and `Excerpt` and NOTHING on
// the snapshot, so ten collections went onto the log unbounded. That is not a
// screen rendering badly: a snapshot is copied onto the log, held by each of
// the stream's replicas for its whole retention window and applied into every
// data node's rows — and the design's own arithmetic sums these caps to about
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
		over  func(*worklet.Notify)
	}{
		{"watchers", worklet.MaxWatchers, func(n *worklet.Notify) {
			n.Snapshot.Watchers = handlesOf(worklet.MaxWatchers + 1)
		}},
		{"removed_watchers", worklet.MaxWatchers, func(n *worklet.Notify) {
			n.Kind = worklet.ChangeWatchers
			n.Snapshot.RemovedWatchers = handlesOf(worklet.MaxWatchers + 1)
		}},
		{"collaborators", worklet.MaxCollaborators, func(n *worklet.Notify) {
			n.Snapshot.Collaborators = handlesOf(worklet.MaxCollaborators + 1)
		}},
		{"thread_participants", worklet.MaxThreadParticipants, func(n *worklet.Notify) {
			n.Snapshot.ThreadParticipants = handlesOf(worklet.MaxThreadParticipants + 1)
		}},
		{"checklist_assignees", worklet.MaxChecklists, func(n *worklet.Notify) {
			n.Snapshot.ChecklistAssignees = handlesOf(worklet.MaxChecklists + 1)
		}},
		{"mentions", worklet.MaxMentions, func(n *worklet.Notify) {
			n.Mentions = handlesOf(worklet.MaxMentions + 1)
		}},
		{"unblocked", worklet.MaxDependents, func(n *worklet.Notify) {
			n.Snapshot.Unblocked = taskParties(worklet.MaxDependents + 1)
		}},
		{"dependents", worklet.MaxDependents, func(n *worklet.Notify) {
			n.Snapshot.Dependents = taskParties(worklet.MaxDependents + 1)
		}},
	} {
		t.Run(tc.field, func(t *testing.T) {
			t.Parallel()
			over := &worklet.Notify{Kind: worklet.ChangeFields}
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
			at := &worklet.Notify{Kind: worklet.ChangeFields}
			tc.over(at)
			trimToCap(at, tc.field, tc.max)
			if err := at.Validate(); err != nil {
				t.Errorf("a notification naming exactly %d %s was refused: %v",
					tc.max, tc.field, err)
			}
		})
	}
}

func taskParties(n int) []worklet.TaskParty {
	out := make([]worklet.TaskParty, n)
	for i := range out {
		out[i] = worklet.TaskParty{
			Task: fmt.Sprintf("t-%03d", i), Key: fmt.Sprintf("ENG-%d", i),
			Assignee: fmt.Sprintf("person-%03d", i),
		}
	}
	return out
}

func trimToCap(n *worklet.Notify, field string, max int) {
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
	task.Watchers = handlesOf(worklet.MaxWatchers)
	if _, err := r.writer.CreateTask(t.Context(), "op-t-1", task, nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	r.drain()

	// GROWING PAST IT IS REFUSED, which is the cap doing its job.
	if _, err := r.writer.UpdateTask(t.Context(), "op-watch", "t-1", "ENG",
		worklet.NoIfMatch,
		worklet.TaskPatch{Watch: &worklet.WatchIntent{
			Handle: "one-more", Watch: true,
		}}, worklet.ChangeWatchers, nil); err == nil {
		t.Error("a sixty-fifth explicit watch was accepted")
	}

	// AND LEAVING IS NOT, which is the half that was broken.
	if _, err := r.writer.UpdateTask(t.Context(), "op-unwatch", "t-1", "ENG",
		worklet.NoIfMatch,
		worklet.TaskPatch{Watch: &worklet.WatchIntent{
			Handle: "person-000", Watch: false,
		}}, worklet.ChangeWatchers, nil); err != nil {
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
	task.Watchers = handlesOf(worklet.MaxWatchers)
	if _, err := r.writer.CreateTask(t.Context(), "op-t-1", task, nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	r.drain()

	if _, err := r.writer.UpdateTask(t.Context(), "op-comment", "t-1", "ENG",
		worklet.NoIfMatch,
		worklet.TaskPatch{
			Comment: &worklet.Comment{
				ID: "c-1", Task: "t-1", Author: "newcomer",
				AuthorKind: worklet.AuthorHuman, Body: "a thought",
				CreatedAt: wednesday,
			},
			Watch: &worklet.WatchIntent{
				Handle: "newcomer", Watch: true, Auto: true,
			},
		}, worklet.ChangeComment, nil); err != nil {
		t.Fatalf("a comment on a task at the watcher cap was refused: %v — "+
			"the comment has nothing to do with how many people watch", err)
	}
	r.drain()

	// THE COMMENT LANDED AND THE WATCH DID NOT.
	detail, err := r.reader.Task(t.Context(), "t-1",
		worklet.DetailWants{Comments: true}, statelog.Freshness{Level: statelog.ReadStale})
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
// [worklet.MaxTitle], [worklet.MaxBody] and [worklet.MaxCommentBody] are
// declared under a sentence promising each is "refused at WRITE naming the
// field, never cut" — and not one of them had a comparison anywhere in the
// tree. A search found the three constants, one alias and four doc comments.
// The nearest bound that fired was MaxCommitBytes, about forty times these
// and phrased about the record rather than the field, so a caller past a cap
// got either silence or a number they could not act on.
//
// The comment is where the cost shows: a thread page carries every comment
// WHOLE and at least one, so a body stored past MaxCommentBody would make a
// page too heavy for one tool answer — reachable through no tool in the
// engine. The cap is what keeps "whole" deliverable.
//
// PER FIELD, because a check that bounded two of three would pass any case
// written about the interesting one.
func TestATasksOwnTextIsRefusedPastItsCap(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		field string
		max   int
		// write applies a value of n bytes and returns what the writer said.
		write func(*roundTrip, worklet.Task, int) error
	}{
		{"title", worklet.MaxTitle, func(r *roundTrip, task worklet.Task, n int) error {
			_, err := r.writer.UpdateTask(r.t.Context(), "op-title", task.ID, "ENG",
				worklet.NoIfMatch, worklet.TaskPatch{Title: strptr(strings.Repeat("t", n))},
				worklet.ChangeFields, nil)
			return err
		}},
		{"body", worklet.MaxBody, func(r *roundTrip, task worklet.Task, n int) error {
			_, err := r.writer.UpdateTask(r.t.Context(), "op-body", task.ID, "ENG",
				worklet.NoIfMatch, worklet.TaskPatch{Body: strptr(strings.Repeat("b", n))},
				worklet.ChangeFields, nil)
			return err
		}},
		{"comment body", worklet.MaxCommentBody, func(r *roundTrip, task worklet.Task, n int) error {
			_, err := r.writer.UpdateTask(r.t.Context(), "op-comment", task.ID, "ENG",
				worklet.NoIfMatch, worklet.TaskPatch{Comment: &worklet.Comment{
					ID: "cm-cap", Task: task.ID, Author: "ana",
					AuthorKind: worklet.AuthorHuman,
					Body:       strings.Repeat("c", n), CreatedAt: wednesday,
				}}, worklet.ChangeComment, nil)
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
			// TYPED, carrying what a caller names to a person — the
			// field, what was sent and the cap — and marked as content,
			// so no surface offers a retry that refuses identically.
			var capped *worklet.TextCapError
			if !errors.As(err, &capped) || string(capped.Field) != tc.field ||
				capped.Size != tc.max+1 || capped.Limit != tc.max {
				t.Errorf("the refusal is not a TextCapError{%s, %d, %d}: %#v",
					tc.field, tc.max+1, tc.max, err)
			}
			if !errors.Is(err, worklet.ErrInvalid) {
				t.Errorf("the refusal is not marked as content: %v", err)
			}
			// AND IT NAMES NO TASK: the caller named the item it was
			// editing, and a uuid in the sentence is one more thing a
			// person is shown that they never typed.
			if strings.Contains(err.Error(), task.ID) {
				t.Errorf("the refusal names the task's id: %v", err)
			}
			// AND IT NAMES A REMEDY THAT EXISTS. It used to offer "an
			// attachment", which this tracker has no verb for — a model
			// told to attach went looking for a tool that is not there.
			// A page is, and a link is how a task points at one.
			if !strings.Contains(err.Error(), "on a page and link it") ||
				strings.Contains(err.Error(), "attachment") {
				t.Errorf("the refusal's remedy is not page linking: %v", err)
			}
		})
	}
}

// A CREATE PAST A CAP NAMES NO TASK, because there is none. The create's id is
// minted before the write — so every node derives the same one from the
// request — and a refusal that quoted it told a person filing from the
// dashboard that "the title on task 68c5…" was too long, about a task that was
// never created and could never be looked up.
func TestACreatePastACapNamesNoTask(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	task := newTask("t-born-refused")
	task.Key = ""
	task.Title = strings.Repeat("t", worklet.MaxTitle+1)
	_, err := r.writer.CreateTask(r.t.Context(), "op-born-refused", task, nil)
	var capped *worklet.TextCapError
	if !errors.As(err, &capped) || capped.Field != worklet.TextTitle {
		t.Fatalf("a create with a %d-byte title was not refused as a title "+
			"past its cap: %v", worklet.MaxTitle+1, err)
	}
	if strings.Contains(err.Error(), task.ID) {
		t.Errorf("the refusal names the id of a task that was never created: %v", err)
	}
}

// ONE CALL NAMES AT MOST [worklet.MaxTasksNamed] OTHER TASKS: a dependency
// change's counterparties, and a rank order's own moves beside a re-spread's
// neighbours.
//
// Each counterparty is a commit of its own and each move a placement on one
// record, so a call past the cap is a batch rather than a gesture, and both
// writers refuse it before anything is read or published — naming the count,
// so the caller knows how far to split it. AT the cap neither refusal is the
// cap's: the call goes on to whatever it would have met next.
//
// Mutation: drop either check and its over-the-cap row is accepted.
func TestOneCallNamesAtMostMaxTasksNamedTasks(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	ids := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("t-%04d", i)
		}
		return out
	}
	depend := func(n int) error {
		_, err := r.writer.Depend(t.Context(), fmt.Sprintf("op-depend-%d", n),
			worklet.DependencyChange{Task: "dep", Project: "ENG", WaitingOnAdd: ids(n)},
			fixedLeads{})
		return err
	}
	move := func(n int) error {
		placements := make([]worklet.Placement, n)
		for i, id := range ids(n) {
			rank, err := worklet.IntegerAt(uint64(i + 1))
			if err != nil {
				t.Fatalf("a rank key for placement %d: %v", i, err)
			}
			placements[i] = worklet.Placement{Task: id, Rank: rank}
		}
		_, err := r.writer.MoveTasks(t.Context(), fmt.Sprintf("op-move-%d", n), "ENG", placements)
		return err
	}
	for _, tc := range []struct {
		name   string
		call   func(int) error
		max    int
		refuse string
	}{
		{"a dependency change's counterparties", depend, worklet.MaxTasksNamed,
			"one call may name"},
		{"a rank order's placements", move, worklet.MaxTasksNamed + worklet.RankRespreadInline,
			"one record carries at most"},
	} {
		over := tc.call(tc.max + 1)
		if !errors.Is(over, worklet.ErrInvalid) || !strings.Contains(over.Error(), tc.refuse) ||
			!strings.Contains(over.Error(), fmt.Sprint(tc.max+1)) {
			t.Errorf("%s: %d were answered %v, want the cap's refusal naming the count",
				tc.name, tc.max+1, over)
		}
		if at := tc.call(tc.max); at != nil && strings.Contains(at.Error(), tc.refuse) {
			t.Errorf("%s: exactly %d were refused by the cap: %v", tc.name, tc.max, at)
		}
	}
}

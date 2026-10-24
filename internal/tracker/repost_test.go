package tracker_test

import (
	"database/sql"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A REMARK ALREADY POSTED IS ANSWERED BY ITS ROW, and nothing is published.
//
// A re-run of the call that posted a remark names the same comment id — a
// rebased attempt included, whose operation is new and which the ledger
// therefore does not answer. Published, the post re-sent the remark as first
// written: a second history row, a second wake, and its automatic watch put
// back on the item somebody who had stopped following it. And it is answered
// whatever became of the item since: the post landed, so an item trashed after
// it is no reason to tell its author to restore it first.
func TestARemarkAlreadyPostedIsAnsweredByItsRow(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	created := r.createTask("who owns the rollback")
	question := tracker.Comment{
		ID: "cm-1", Task: created.ID, Author: "ana", AuthorKind: tracker.AuthorHuman,
		Body: "who owns the rollback?", Ask: "bob", CreatedAt: wednesday,
	}
	post := func(op string, comment tracker.Comment) tracker.WriteResult {
		t.Helper()
		got, err := r.writer.UpdateTask(t.Context(), op, created.ID, "ENG",
			tracker.NoIfMatch, tracker.TaskPatch{
				Comment: &comment,
				Watch:   &tracker.WatchIntent{Handle: "ana", Watch: true, Auto: true},
			}, tracker.ChangeComment, nil)
		if err != nil {
			t.Fatalf("%s: %v", op, err)
		}
		r.drain()
		return got
	}
	post("op-1", question)
	answers := "cm-1"
	if _, err := r.writer.UpdateTask(t.Context(), "op-answer", created.ID, "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Comment: &tracker.Comment{
			ID: "cm-2", Task: created.ID, Author: "bob", AuthorKind: tracker.AuthorHuman,
			Body: "platform does", Answers: &answers, CreatedAt: wednesday,
		}}, tracker.ChangeComment, nil); err != nil {
		t.Fatalf("answer: %v", err)
	}
	r.drain()
	if _, err := r.writer.EditComment(t.Context(), "op-edit", created.ID, "ENG",
		"cm-1", "who owns the rollback now?", nil); err != nil {
		t.Fatalf("edit: %v", err)
	}
	r.drain()
	if _, err := r.writer.UpdateTask(t.Context(), "op-unwatch", created.ID, "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{
			Watch: &tracker.WatchIntent{Handle: "ana", Watch: false},
		}, tracker.ChangeWatchers, nil); err != nil {
		t.Fatalf("unwatch: %v", err)
	}
	r.drain()

	end, history := r.logEnd(t), r.historyOf(created.ID)
	// THE CALL AGAIN, under the operation a rebased attempt mints and with
	// the remark as that call first wrote it.
	reposted := question
	reposted.CreatedAt = wednesday.Add(time.Hour)
	got := post("op-1-rebased", reposted)
	if got.Outcome != statelog.OutcomeApplied || got.Position.Seq != 0 {
		t.Errorf("the re-post answered %s at %v, want applied with nothing published",
			got.Outcome, got.Position)
	}
	if now := r.logEnd(t); now != end {
		t.Errorf("the re-post published: the log moved from %d to %d", end, now)
	}
	if now := r.historyOf(created.ID); now != history {
		t.Errorf("the re-post wrote %d history row(s)", now-history)
	}
	detail := r.task(t, created.ID)
	if slices.Contains(detail.Task.Watchers, "ana") || !slices.Contains(detail.Task.Muted, "ana") {
		t.Errorf("after the re-post ana watches %v and is muted %v — the "+
			"re-post's watch followed her again", detail.Task.Watchers, detail.Task.Muted)
	}
	thread, err := r.reader.Task(t.Context(), created.ID,
		tracker.DetailWants{Comment: "cm-1"}, statelog.Freshness{Level: statelog.ReadStale})
	if err != nil || len(thread.Comments) != 1 {
		t.Fatalf("read the question: %v (%d)", err, len(thread.Comments))
	}
	if c := thread.Comments[0]; c.Body != "who owns the rollback now?" || !c.CreatedAt.Equal(wednesday) {
		t.Errorf("the question reads %q from %v after the re-post, want its edit "+
			"from %v", c.Body, c.CreatedAt, wednesday)
	}
	if open := ids(r.ask(map[string]any{
		"container": "project:ENG", "has_open_asks": "true",
	})); len(open) != 0 {
		t.Errorf("has_open_asks answers %v after the re-post", open)
	}

	// AND WHATEVER BECAME OF THE ITEM: trashed after the remark landed, the
	// re-run is still answered by the row.
	if _, err := r.writer.RemoveTask(t.Context(), "op-remove", created.ID, "ENG",
		false, nil); err != nil {
		t.Fatalf("remove: %v", err)
	}
	r.drain()
	end = r.logEnd(t)
	if got := post("op-1-again", reposted); got.Outcome != statelog.OutcomeApplied {
		t.Errorf("a re-post onto a trashed item answered %s", got.Outcome)
	}
	if now := r.logEnd(t); now != end {
		t.Errorf("a re-post onto a trashed item published: the log moved from %d to %d",
			end, now)
	}
}

// A REMARK IS REWRITTEN ONLY THROUGH EditComment.
//
// The edit kind is what the applier rewrites a remark's words on, and
// EditComment is the one door that forms the edit from the stored row and
// refuses anybody but the author. A plain patch stating that kind would be a
// rewrite under an author rule nothing checked.
func TestARemarkIsRewrittenOnlyThroughEditComment(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	created := r.createTask("who owns the rollback")
	question := tracker.Comment{
		ID: "cm-1", Task: created.ID, Author: "ana", AuthorKind: tracker.AuthorHuman,
		Body: "who owns the rollback?", Ask: "bob", CreatedAt: wednesday,
	}
	if _, err := r.writer.UpdateTask(t.Context(), "op-ask", created.ID, "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Comment: &question},
		tracker.ChangeComment, nil); err != nil {
		t.Fatalf("ask: %v", err)
	}
	r.drain()
	bob := r.writer.As("bob", tracker.AuthorHuman, tracker.Provenance{})
	rewritten := question
	rewritten.Body = "nobody owns it"
	if _, err := bob.UpdateTask(t.Context(), "op-rewrite", created.ID, "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Comment: &rewritten},
		tracker.ChangeCommentEdited, nil); !errors.Is(err, tracker.ErrInvalid) {
		t.Fatalf("bob's patch rewriting ana's remark answered %v, want invalid", err)
	}
	r.drain()
	thread, err := r.reader.Task(t.Context(), created.ID,
		tracker.DetailWants{Comment: "cm-1"}, statelog.Freshness{Level: statelog.ReadStale})
	if err != nil || len(thread.Comments) != 1 || thread.Comments[0].Body != question.Body {
		t.Errorf("ana's remark reads %+v after bob's refused rewrite (%v)",
			thread.Comments, err)
	}
}

// A REMARK'S ID IS ITS ITEM'S: a post under an id another item's remark holds
// is refused rather than answered by that row. Answered, it would tell its
// caller a remark landed on this item when nothing did — the row it met is
// somebody else's remark, on somebody else's thread.
func TestAPostUnderAnIdAnotherItemHoldsIsRefused(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	rollback := r.createTask("who owns the rollback")
	pager := r.createTask("who holds the pager")
	post := func(op, task string) error {
		t.Helper()
		_, err := r.writer.UpdateTask(t.Context(), op, task, "ENG",
			tracker.NoIfMatch, tracker.TaskPatch{Comment: &tracker.Comment{
				ID: "cm-1", Task: task, Author: "ana", AuthorKind: tracker.AuthorHuman,
				Body: "who owns this?", CreatedAt: wednesday,
			}}, tracker.ChangeComment, nil)
		r.drain()
		return err
	}
	if err := post("op-rollback", rollback.ID); err != nil {
		t.Fatalf("the remark on %s: %v", rollback.ID, err)
	}
	end := r.logEnd(t)
	if err := post("op-pager", pager.ID); !errors.Is(err, tracker.ErrInvalid) {
		t.Errorf("a post on %s under the id of %s's remark answered %v, want "+
			"invalid", pager.ID, rollback.ID, err)
	}
	if now := r.logEnd(t); now != end {
		t.Errorf("the refused post published: the log moved from %d to %d", end, now)
	}
	if now := r.historyOf(pager.ID); now != 1 {
		t.Errorf("%s holds %d history rows after the refused post, want its create",
			pager.ID, now)
	}
}

// historyOf counts the history rows one task has.
func (r *roundTrip) historyOf(id string) int {
	r.t.Helper()
	var n int
	if err := r.db.Read(r.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(r.t.Context(),
			`SELECT COUNT(*) FROM tracker_history WHERE subject_id = ?`, id).Scan(&n)
	}); err != nil {
		r.t.Fatalf("count the history of %s: %v", id, err)
	}
	return n
}

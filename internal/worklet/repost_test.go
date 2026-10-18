package worklet_test

import (
	"database/sql"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
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
	question := worklet.Comment{
		ID: "cm-1", Task: created.ID, Author: "ana", AuthorKind: worklet.AuthorHuman,
		Body: "who owns the rollback?", Ask: "bob", CreatedAt: wednesday,
	}
	post := func(op string, comment worklet.Comment) worklet.WriteResult {
		t.Helper()
		got, err := r.writer.UpdateTask(t.Context(), op, created.ID, "ENG",
			worklet.NoIfMatch, worklet.TaskPatch{
				Comment: &comment,
				Watch:   &worklet.WatchIntent{Handle: "ana", Watch: true, Auto: true},
			}, worklet.ChangeComment, nil)
		if err != nil {
			t.Fatalf("%s: %v", op, err)
		}
		r.drain()
		return got
	}
	post("op-1", question)
	answers := "cm-1"
	if _, err := r.writer.UpdateTask(t.Context(), "op-answer", created.ID, "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Comment: &worklet.Comment{
			ID: "cm-2", Task: created.ID, Author: "bob", AuthorKind: worklet.AuthorHuman,
			Body: "platform does", Answers: &answers, CreatedAt: wednesday,
		}}, worklet.ChangeComment, nil); err != nil {
		t.Fatalf("answer: %v", err)
	}
	r.drain()
	if _, err := r.writer.EditComment(t.Context(), "op-edit", created.ID, "ENG",
		"cm-1", "who owns the rollback now?", nil); err != nil {
		t.Fatalf("edit: %v", err)
	}
	r.drain()
	if _, err := r.writer.UpdateTask(t.Context(), "op-unwatch", created.ID, "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{
			Watch: &worklet.WatchIntent{Handle: "ana", Watch: false},
		}, worklet.ChangeWatchers, nil); err != nil {
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
		worklet.DetailWants{Comment: "cm-1"}, statelog.Freshness{Level: statelog.ReadStale})
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
	question := worklet.Comment{
		ID: "cm-1", Task: created.ID, Author: "ana", AuthorKind: worklet.AuthorHuman,
		Body: "who owns the rollback?", Ask: "bob", CreatedAt: wednesday,
	}
	if _, err := r.writer.UpdateTask(t.Context(), "op-ask", created.ID, "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Comment: &question},
		worklet.ChangeComment, nil); err != nil {
		t.Fatalf("ask: %v", err)
	}
	r.drain()
	bob := r.writer.As("bob", worklet.AuthorHuman, worklet.Provenance{})
	rewritten := question
	rewritten.Body = "nobody owns it"
	if _, err := bob.UpdateTask(t.Context(), "op-rewrite", created.ID, "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Comment: &rewritten},
		worklet.ChangeCommentEdited, nil); !errors.Is(err, worklet.ErrInvalid) {
		t.Fatalf("bob's patch rewriting ana's remark answered %v, want invalid", err)
	}
	r.drain()
	thread, err := r.reader.Task(t.Context(), created.ID,
		worklet.DetailWants{Comment: "cm-1"}, statelog.Freshness{Level: statelog.ReadStale})
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
			worklet.NoIfMatch, worklet.TaskPatch{Comment: &worklet.Comment{
				ID: "cm-1", Task: task, Author: "ana", AuthorKind: worklet.AuthorHuman,
				Body: "who owns this?", CreatedAt: wednesday,
			}}, worklet.ChangeComment, nil)
		r.drain()
		return err
	}
	if err := post("op-rollback", rollback.ID); err != nil {
		t.Fatalf("the remark on %s: %v", rollback.ID, err)
	}
	end := r.logEnd(t)
	if err := post("op-pager", pager.ID); !errors.Is(err, worklet.ErrInvalid) {
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

// A REMARK POSTED AGAIN IS RESOLVED FROM ITS ROW, before anything is inferred.
//
// A re-run of the call that posted a remark names the same comment id, and the
// write answers it from that row with nothing published. The thread read used
// to resolve it from the asks open NOW, which its own post had changed: an
// answer whose question it had inferred found that question closed, so its
// choice had no question to be one of and its words answered nothing, and a
// question asked since was claimed as the one it answered while its write
// closed nothing. Resolved from the row, the re-run asks and answers what its
// post did — a choice still checked against the decision it chose from — and a
// remark that answered nothing answers nothing once an ask has arrived. A new
// remark is the control: it infers what is open now. And the write reports the
// remark it answered from ([worklet.WriteResult.Posted]), whatever the re-run
// carried.
//
// Mutation: skip the row in Reader.Thread and the inferred answer's re-run
// answers nothing (with its choice or without), the explicit one is refused as
// already answered by itself, and the re-runs after a second ask claim it;
// leave Posted unset and the write reports nothing.
func TestARemarkPostedAgainIsResolvedFromItsRow(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	created := r.createTask("ship or hold")
	stale := statelog.Freshness{Level: statelog.ReadStale}
	read := func(q worklet.ThreadQuery) (worklet.ResolvedThread, error) {
		q.Task = created.ID
		return r.reader.Thread(t.Context(), q, stale)
	}
	resolves := func(name string, q worklet.ThreadQuery, answers string) worklet.ResolvedThread {
		t.Helper()
		got, err := read(q)
		switch {
		case err != nil:
			t.Errorf("%s: the thread read refused: %v", name, err)
		case got.Answers != answers:
			t.Errorf("%s: resolved to answer %q, want %q", name, got.Answers, answers)
		}
		return got
	}
	post := func(op string, c worklet.Comment) worklet.WriteResult {
		t.Helper()
		c.Task, c.CreatedAt = created.ID, wednesday
		got, err := r.writer.UpdateTask(t.Context(), op, created.ID, "ENG",
			worklet.NoIfMatch, worklet.TaskPatch{Comment: &c}, worklet.ChangeComment, nil)
		if err != nil {
			t.Fatalf("%s: %v", op, err)
		}
		r.drain()
		return got
	}

	// A REMARK OF ANA'S MADE WHILE NOBODY HAD ASKED HER ANYTHING.
	resolves("a remark with nothing open", worklet.ThreadQuery{Author: "ana",
		Comment: "c-plain"}, "")
	post("op-plain", worklet.Comment{ID: "c-plain", Author: "ana",
		AuthorKind: worklet.AuthorHuman, Body: "noted"})
	// THE QUESTION, AND ANA'S ANSWER AS ITS FIRST ATTEMPT RESOLVED IT:
	// inferred, choosing an option.
	post("op-ask", worklet.Comment{ID: "c-ask", Author: "dev",
		AuthorKind: worklet.AuthorAgent, Body: "Friday?", Ask: "ana",
		Decision: decisionFixture()})
	first := resolves("the first attempt", worklet.ThreadQuery{Author: "ana",
		Comment: "c-answer", Choice: "hold"}, "c-ask")
	post("op-answer", worklet.Comment{ID: "c-answer", Author: "ana",
		AuthorKind: worklet.AuthorHuman, Body: "after the audit",
		Answers: &first.Answers, Choice: "hold"})

	// THE SAME CALL AGAIN, three ways.
	again := resolves("the re-run with its choice", worklet.ThreadQuery{Author: "ana",
		Comment: "c-answer", Choice: "hold"}, "c-ask")
	if again.AnswersDecision == nil || again.AnswersDecision.Question != decisionFixture().Question ||
		again.AnsweredAuthor != "dev" {
		t.Errorf("the re-run resolved the decision %+v asked by %q, want c-ask's, by dev",
			again.AnswersDecision, again.AnsweredAuthor)
	}
	resolves("the re-run in words", worklet.ThreadQuery{Author: "ana",
		Comment: "c-answer"}, "c-ask")
	resolves("the re-run naming its question", worklet.ThreadQuery{Author: "ana",
		Comment: "c-answer", Answers: "c-ask"}, "c-ask")
	// A CHOICE IS STILL CHECKED against the decision it chose from.
	if _, err := read(worklet.ThreadQuery{Author: "ana", Comment: "c-answer",
		Choice: "wait"}); !errors.Is(err, worklet.ErrInvalid) {
		t.Errorf("a re-run choosing an option c-ask never offered answered %v, "+
			"want invalid", err)
	}

	// A SECOND QUESTION TO ANA, asked after her answer landed.
	post("op-ask-2", worklet.Comment{ID: "c-ask-2", Author: "dev",
		AuthorKind: worklet.AuthorAgent, Body: "And the changelog?", Ask: "ana"})
	resolves("the re-run after a second ask", worklet.ThreadQuery{Author: "ana",
		Comment: "c-answer"}, "c-ask")
	resolves("the remark that answered nothing, after an ask arrived",
		worklet.ThreadQuery{Author: "ana", Comment: "c-plain"}, "")
	resolves("a new remark", worklet.ThreadQuery{Author: "ana", Comment: "c-new"}, "c-ask-2")
	// AN ASK MADE AGAIN asks whom its row asked.
	if asked := resolves("the question made again", worklet.ThreadQuery{
		Author: "dev", Comment: "c-ask", Ask: "ana"}, ""); asked.Asked != "ana" {
		t.Errorf("the question made again asks %q, want ana", asked.Asked)
	}

	// A REMARK THE ROW SAYS IS NOT THIS CALL'S is refused as the write
	// refuses it: another item's, or another author's.
	other := r.createTask("who holds the pager")
	if _, err := r.writer.UpdateTask(t.Context(), "op-elsewhere", other.ID, "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Comment: &worklet.Comment{
			ID: "c-elsewhere", Task: other.ID, Author: "ana",
			AuthorKind: worklet.AuthorHuman, Body: "me", CreatedAt: wednesday,
		}}, worklet.ChangeComment, nil); err != nil {
		t.Fatalf("the remark on %s: %v", other.ID, err)
	}
	r.drain()
	for name, q := range map[string]worklet.ThreadQuery{
		"another item's remark":   {Author: "ana", Comment: "c-elsewhere"},
		"another author's remark": {Author: "bob", Comment: "c-answer"},
	} {
		if _, err := read(q); !errors.Is(err, worklet.ErrInvalid) {
			t.Errorf("%s answered %v, want invalid", name, err)
		}
	}

	// AND THE WRITE SAYS WHAT IT ANSWERED FROM. A re-run that read the thread
	// before its first attempt landed carries what it inferred then — here
	// the question asked since — and the row answers it.
	end := r.logEnd(t)
	later := "c-ask-2"
	got := post("op-answer-rebased", worklet.Comment{ID: "c-answer", Author: "ana",
		AuthorKind: worklet.AuthorHuman, Body: "after the audit", Answers: &later})
	if got.Posted == nil || *got.Posted != (worklet.PostedRemark{Answers: "c-ask"}) {
		t.Errorf("the re-post reports %+v, want the row's answer to c-ask", got.Posted)
	}
	if now := r.logEnd(t); now != end {
		t.Errorf("the re-post published: the log moved from %d to %d", end, now)
	}
	resolves("a new remark after the re-post", worklet.ThreadQuery{Author: "ana",
		Comment: "c-newer"}, "c-ask-2")
	if got := post("op-ask-again", worklet.Comment{ID: "c-ask", Author: "dev",
		AuthorKind: worklet.AuthorAgent, Body: "Friday?", Ask: "ana",
		Decision: decisionFixture()}); got.Posted == nil ||
		*got.Posted != (worklet.PostedRemark{Asked: "ana"}) {
		t.Errorf("the question posted again reports %+v, want the row's ask of ana",
			got.Posted)
	}
	if got := post("op-fresh", worklet.Comment{ID: "c-fresh", Author: "dev",
		AuthorKind: worklet.AuthorAgent, Body: "fyi"}); got.Posted != nil {
		t.Errorf("a new remark reports %+v as already posted", got.Posted)
	}
}

// A POST UNDER AN ID ANOTHER AUTHOR'S REMARK HOLDS IS REFUSED, as one another
// item's holds is. An id is derived from its author's own call, so the row is
// somebody else's remark; answered from it, the post would tell its caller that
// a remark they never made was theirs.
//
// Mutation: drop the author from postedComment's check and the post answers
// applied from bob's row.
func TestAPostUnderAnIdAnotherAuthorsRemarkHoldsIsRefused(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	created := r.createTask("who owns the rollback")
	post := func(op, author string) error {
		t.Helper()
		_, err := r.writer.UpdateTask(t.Context(), op, created.ID, "ENG",
			worklet.NoIfMatch, worklet.TaskPatch{Comment: &worklet.Comment{
				ID: "cm-1", Task: created.ID, Author: author,
				AuthorKind: worklet.AuthorHuman, Body: "platform does",
				CreatedAt: wednesday,
			}}, worklet.ChangeComment, nil)
		r.drain()
		return err
	}
	if err := post("op-bob", "bob"); err != nil {
		t.Fatalf("bob's remark: %v", err)
	}
	end := r.logEnd(t)
	if err := post("op-ana", "ana"); !errors.Is(err, worklet.ErrInvalid) {
		t.Errorf("ana's post under bob's remark's id answered %v, want invalid", err)
	}
	if now := r.logEnd(t); now != end {
		t.Errorf("the refused post published: the log moved from %d to %d", end, now)
	}
}

// A REMARK IS TAKEN DOWN BY NO PATCH.
//
// No gesture takes a tracker comment down, so nothing forms the removal kind,
// and the applier writes nothing for a record of it. A patch carrying a
// comment under that kind was taken for a post, as a comment under any kind
// but the edit and the resolve is: it posted the remark it said it took down,
// and answered one already on the thread as applied while the remark stood.
// It is refused before anything is published, its author's own included.
//
// Mutation: drop UpdateTask's refusal of the kind and the removal of a remark
// nobody posted posts it, and the removal of a posted one answers applied.
func TestARemarkIsTakenDownByNoPatch(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	created := r.createTask("who owns the rollback")
	remark := worklet.Comment{
		ID: "cm-1", Task: created.ID, Author: "ana", AuthorKind: worklet.AuthorHuman,
		Body: "who owns the rollback?", CreatedAt: wednesday,
	}
	if _, err := r.writer.UpdateTask(t.Context(), "op-post", created.ID, "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Comment: &remark},
		worklet.ChangeComment, nil); err != nil {
		t.Fatalf("post: %v", err)
	}
	r.drain()
	end := r.logEnd(t)
	unposted := remark
	unposted.ID = "cm-2"
	for name, c := range map[string]worklet.Comment{
		"the remark on the thread": remark, "a remark nobody posted": unposted,
	} {
		c.Removed = true
		if _, err := r.writer.UpdateTask(t.Context(), "op-remove-"+c.ID, created.ID,
			"ENG", worklet.NoIfMatch, worklet.TaskPatch{Comment: &c},
			worklet.ChangeCommentRemoved, nil); !errors.Is(err, worklet.ErrInvalid) {
			t.Errorf("a patch taking down %s answered %v, want invalid", name, err)
		}
	}
	r.drain()
	if now := r.logEnd(t); now != end {
		t.Errorf("a refused removal published: the log moved from %d to %d", end, now)
	}
	thread, err := r.reader.Task(t.Context(), created.ID,
		worklet.DetailWants{Comments: true}, statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		t.Fatalf("read the thread: %v", err)
	}
	if len(thread.Comments) != 1 || thread.Comments[0].ID != "cm-1" ||
		thread.Comments[0].Removed || thread.Comments[0].Body != remark.Body {
		t.Errorf("after the refused removals the thread reads %+v, want cm-1 "+
			"standing as posted and nothing else", thread.Comments)
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

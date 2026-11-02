package worklet_test

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A REMARK IS RESOLVED BY WHOEVER MADE IT AND BY NOBODY ELSE.
//
// A question resolved without an answer is closed for its asker: the person
// it asks answers it instead, and anybody else resolving it would take the
// asker's question off every board on their behalf. So the refusal is the
// writer's own, decided against the row it read, and a caller holding every
// grant there is is still refused here. A remark that is not on the item, not
// there at all or another item's, is not one to resolve on it.
func TestOnlyACommentsAuthorMayResolveIt(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	rollback := r.createTask("who owns the rollback")
	pager := r.createTask("who holds the pager")
	for _, c := range []struct{ id, task string }{
		{"cm-1", rollback.ID}, {"cm-2", pager.ID},
	} {
		if _, err := r.writer.UpdateTask(t.Context(), "op-"+c.id, c.task, "ENG",
			worklet.NoIfMatch, worklet.TaskPatch{Comment: &worklet.Comment{
				ID: c.id, Task: c.task, Author: "ana", AuthorKind: worklet.AuthorHuman,
				Body: "who owns this?", Ask: "bob", CreatedAt: wednesday,
			}}, worklet.ChangeComment, nil); err != nil {
			t.Fatalf("ask %s: %v", c.id, err)
		}
		r.drain()
	}

	// THE PERSON ASKED, however they are authorised.
	end := r.logEnd(t)
	bob := r.writer.As("bob", worklet.AuthorHuman, worklet.Provenance{})
	if _, err := bob.ResolveComment(t.Context(), "op-bob", rollback.ID, "ENG",
		"cm-1", nil); !errors.Is(err, worklet.ErrNotAuthor) {
		t.Fatalf("bob resolving ana's question answered %v, want not its author", err)
	}
	r.drain()
	if now := r.logEnd(t); now != end {
		t.Errorf("bob's refused resolve published: the log moved from %d to %d", end, now)
	}
	if c := r.remarkOn(rollback.ID, "cm-1"); c.Resolved {
		t.Errorf("ana's question reads resolved by %q after bob's refused resolve",
			c.ResolvedBy)
	}

	// THE AUTHOR: resolved as herself, at her writer's clock.
	r.at = wednesday.Add(time.Hour)
	if _, err := r.writer.ResolveComment(t.Context(), "op-ana", rollback.ID, "ENG",
		"cm-1", nil); err != nil {
		t.Fatalf("ana's own resolve: %v", err)
	}
	r.drain()
	if c := r.remarkOn(rollback.ID, "cm-1"); !c.Resolved || c.ResolvedBy != "ana" ||
		c.ResolvedAt == nil || !c.ResolvedAt.Equal(r.at) {
		t.Errorf("ana's question reads resolved=%v by %q at %v after her resolve, "+
			"want resolved by ana at %v", c.Resolved, c.ResolvedBy, c.ResolvedAt, r.at)
	}

	// AND A REMARK THAT IS NOT ON THE ITEM is not one anybody wrote there:
	// one nobody posted, and another item's.
	for _, id := range []string{"cm-9", "cm-2"} {
		if _, err := r.writer.ResolveComment(t.Context(), "op-resolve-"+id,
			rollback.ID, "ENG", id, nil); !errors.Is(err, worklet.ErrNoComment) {
			t.Errorf("a resolve of %s on %s answered %v, want no such comment",
				id, rollback.ID, err)
		}
	}
	if c := r.remarkOn(pager.ID, "cm-2"); c.Resolved {
		t.Errorf("a resolve naming %s's remark on %s resolved it", pager.ID, rollback.ID)
	}
}

// A REMARK IS RESOLVED ONLY THROUGH ResolveComment.
//
// The resolve kind is what the applier writes a resolution on, and
// ResolveComment is the one door that forms it from the stored row and refuses
// anybody but the author. A plain patch stating that kind would be a
// resolution under an author rule nothing checked, saying whoever it liked
// resolved the remark whenever it liked; so it is refused, the author's own
// included.
func TestARemarkIsResolvedOnlyThroughResolveComment(t *testing.T) {
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
	end := r.logEnd(t)
	at := wednesday.Add(-time.Hour)
	for _, writer := range []*worklet.Writer{
		r.writer.As("bob", worklet.AuthorHuman, worklet.Provenance{}), r.writer,
	} {
		resolved := question
		resolved.Resolved, resolved.ResolvedBy, resolved.ResolvedAt = true, "bob", &at
		if _, err := writer.UpdateTask(t.Context(), "op-resolve-"+writer.Actor,
			created.ID, "ENG", worklet.NoIfMatch,
			worklet.TaskPatch{Comment: &resolved}, worklet.ChangeCommentResolved,
			nil); !errors.Is(err, worklet.ErrInvalid) {
			t.Errorf("%s's patch resolving ana's remark answered %v, want invalid",
				writer.Actor, err)
		}
	}
	r.drain()
	if now := r.logEnd(t); now != end {
		t.Errorf("a refused resolve published: the log moved from %d to %d", end, now)
	}
	if c := r.remarkOn(created.ID, "cm-1"); c.Resolved {
		t.Errorf("ana's remark reads resolved by %q after the refused patches",
			c.ResolvedBy)
	}
}

// A RESOLVE WRITES THE RESOLUTION AND KEEPS THE REST: the question it closes
// stays answered by the answer that closed it, in the words its author's edit
// left and edited when it was, the columns and the remark the thread returns
// alike. A remark already resolved is resolved again, as an edit that changes
// no word is written again: the record says its author resolved it now.
func TestAResolveKeepsTheAnswerTheWordsAndTheEdit(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	created := r.createTask("who owns the rollback")
	if _, err := r.writer.UpdateTask(t.Context(), "op-ask", created.ID, "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Comment: &worklet.Comment{
			ID: "cm-1", Task: created.ID, Author: "ana", AuthorKind: worklet.AuthorHuman,
			Body: "who owns the rollback?", Ask: "bob", CreatedAt: wednesday,
		}}, worklet.ChangeComment, nil); err != nil {
		t.Fatalf("ask: %v", err)
	}
	r.drain()
	answers := "cm-1"
	if _, err := r.writer.UpdateTask(t.Context(), "op-answer", created.ID, "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Comment: &worklet.Comment{
			ID: "cm-2", Task: created.ID, Author: "bob", AuthorKind: worklet.AuthorHuman,
			Body: "platform does", Answers: &answers, CreatedAt: wednesday,
		}}, worklet.ChangeComment, nil); err != nil {
		t.Fatalf("answer: %v", err)
	}
	r.drain()
	edited := wednesday.Add(time.Minute)
	r.at = edited
	if _, err := r.writer.EditComment(t.Context(), "op-edit", created.ID, "ENG",
		"cm-1", "who owns the rollback now?", nil); err != nil {
		t.Fatalf("edit: %v", err)
	}
	r.drain()

	resolve := func(opID string, at time.Time) {
		t.Helper()
		r.at = at
		if _, err := r.writer.ResolveComment(t.Context(), opID, created.ID, "ENG",
			"cm-1", nil); err != nil {
			t.Fatalf("%s: %v", opID, err)
		}
		r.drain()
	}
	first := wednesday.Add(2 * time.Minute)
	resolve("op-resolve", first)

	row := r.resolutionOf("cm-1")
	if row.resolved != 1 || row.resolvedBy.String != "ana" || !row.resolvedAt.Equal(first) {
		t.Errorf("the question's row reads resolved=%d by %q at %v, want resolved "+
			"by ana at %v", row.resolved, row.resolvedBy.String, row.resolvedAt, first)
	}
	if row.answeredBy.String != "cm-2" {
		t.Errorf("after its resolve the question is answered by %q, want cm-2 — "+
			"a resolve reached the answer's column", row.answeredBy.String)
	}
	if row.body != "who owns the rollback now?" || !row.updatedAt.Equal(edited) {
		t.Errorf("after its resolve the question's row reads %q edited at %v, want "+
			"its edit's words at %v", row.body, row.updatedAt, edited)
	}
	c := r.remarkOn(created.ID, "cm-1")
	if !c.Resolved || c.ResolvedBy != "ana" || c.ResolvedAt == nil ||
		!c.ResolvedAt.Equal(first) {
		t.Errorf("the thread returns the question resolved=%v by %q at %v, want "+
			"what its row says", c.Resolved, c.ResolvedBy, c.ResolvedAt)
	}
	if c.Body != "who owns the rollback now?" || !c.UpdatedAt.Equal(edited) ||
		c.Ask != "bob" {
		t.Errorf("the thread returns the question as %q edited at %v asking %q, "+
			"want its edit's words at %v, asking bob", c.Body, c.UpdatedAt, c.Ask, edited)
	}

	// RESOLVED AGAIN, AND WRITTEN AGAIN: the newest resolution is the one held.
	end := r.logEnd(t)
	again := wednesday.Add(3 * time.Minute)
	resolve("op-resolve-again", again)
	if now := r.logEnd(t); now == end {
		t.Errorf("a resolve of a resolved remark published nothing, want it written " +
			"again as an edit that changes no word is")
	}
	if row := r.resolutionOf("cm-1"); !row.resolvedAt.Equal(again) ||
		row.answeredBy.String != "cm-2" {
		t.Errorf("after the second resolve the question reads resolved at %v and "+
			"answered by %q, want %v and cm-2", row.resolvedAt, row.answeredBy.String, again)
	}
}

// remarkOn reads one remark as the thread returns it.
func (r *roundTrip) remarkOn(task, id string) worklet.Comment {
	r.t.Helper()
	thread, err := r.reader.Task(r.t.Context(), task, worklet.DetailWants{Comment: id},
		statelog.Freshness{Level: statelog.ReadStale})
	if err != nil || len(thread.Comments) != 1 {
		r.t.Fatalf("read comment %s on %s: %v (%d)", id, task, err, len(thread.Comments))
	}
	return thread.Comments[0]
}

// resolvedRow is a remark's columns as the applier left them: its resolution,
// the answer that closed it and its words.
type resolvedRow struct {
	resolved               int
	resolvedBy, answeredBy sql.NullString
	resolvedAt, updatedAt  time.Time
	body                   string
}

// resolutionOf reads one remark's [resolvedRow].
func (r *roundTrip) resolutionOf(id string) resolvedRow {
	r.t.Helper()
	var row resolvedRow
	var resolvedAt, updatedAt sql.NullInt64
	if err := r.db.Read(r.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(r.t.Context(), `
			SELECT resolved, resolved_by, resolved_at, answered_by, updated_at, body
			FROM tracker_comments WHERE id = ?`, id).Scan(&row.resolved,
			&row.resolvedBy, &resolvedAt, &row.answeredBy, &updatedAt, &row.body)
	}); err != nil {
		r.t.Fatalf("read comment %s: %v", id, err)
	}
	if resolvedAt.Valid {
		row.resolvedAt = store.DecodeTime(resolvedAt.Int64)
	}
	if updatedAt.Valid {
		row.updatedAt = store.DecodeTime(updatedAt.Int64)
	}
	return row
}

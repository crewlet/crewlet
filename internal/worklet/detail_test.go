package worklet_test

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// ONE TASK READS BACK WHOLE, and by any name it has ever had.
//
// A board answers "what is there" over many rows and returns what a card
// renders; this is the other question — "tell me everything about this" — and
// every part comes from a different table. The parts a caller did not ask for
// are ABSENT rather than empty, because the costs differ by an order of
// magnitude: a task is one indexed read, a thread can be hundreds of rows, and
// the history grows for the life of the task.
func TestATaskReadsBackWholeAndByEveryNameItHasHad(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	created := r.createTask("Rate limits in the GitLab client")

	if _, err := r.writer.UpdateTask(t.Context(), "op-assign", created.ID, "ENG", worklet.NoIfMatch,
		worklet.TaskPatch{Assignee: strptr("ana")}, worklet.ChangeAssignee, nil); err != nil {
		t.Fatalf("assign: %v", err)
	}
	r.drain()

	detail, err := r.reader.Task(t.Context(), created.Key, worklet.DetailWants{
		History: true,
	}, statelog.Freshness{Level: statelog.ReadSession})
	if err != nil {
		t.Fatalf("read by key: %v", err)
	}
	if detail.Task.ID != created.ID {
		t.Fatalf("reading by key %q gave task %q", created.Key, detail.Task.ID)
	}
	if detail.Task.Title == "" {
		t.Fatal("the task came back with no title, so the document was not decoded")
	}
	if len(detail.History) < 2 {
		t.Fatalf("the feed holds %d change(s) after a create and an assign",
			len(detail.History))
	}
	// NEWEST FIRST, which is what an activity panel renders.
	if detail.History[0].LogSeq < detail.History[len(detail.History)-1].LogSeq {
		t.Fatalf("the feed is oldest-first: %d then %d",
			detail.History[0].LogSeq, detail.History[len(detail.History)-1].LogSeq)
	}
	if !detail.Complete {
		t.Fatalf("a healthy node reports the answer incomplete: %+v", detail.Incomplete)
	}
	if detail.LogSeq == 0 {
		t.Fatal("the answer names no position, so a caller cannot tell how far " +
			"behind it may be")
	}

	// THE PARTS NOT ASKED FOR ARE ABSENT.
	bare, err := r.reader.Task(t.Context(), created.ID, worklet.DetailWants{},
		statelog.Freshness{Level: statelog.ReadSession})
	if err != nil {
		t.Fatalf("read by id: %v", err)
	}
	if len(bare.History) != 0 || len(bare.Comments) != 0 || len(bare.Links) != 0 {
		t.Fatalf("a read that asked for nothing returned %d change(s), %d "+
			"comment(s) and %d link(s)",
			len(bare.History), len(bare.Comments), len(bare.Links))
	}
}

// A TASK NOBODY HAS IS ITS OWN ANSWER.
//
// Its own sentinel, because the caller's answer differs: a tool says "no such
// task" to a model and an API says 404, and neither should say either when
// what actually happened is that this node cannot reach its store.
func TestAMissingTaskIsItsOwnAnswer(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	_, err := r.reader.Task(t.Context(), "ENG-9999", worklet.DetailWants{},
		statelog.Freshness{Level: statelog.ReadSession})
	if err == nil {
		t.Fatal("a task nobody has read back")
	}
	if !isNoTask(err) {
		t.Fatalf("a missing task answers %v, which a caller cannot tell from "+
			"a store it could not reach", err)
	}
}

// A DEFERRED RECORD ABOUT ANOTHER TASK DOES NOT MAKE THIS ONE INCOMPLETE.
//
// The coverage probe is scoped to the OBJECT here and to the container on a
// board, and the difference is the whole reason they are two calls: a company
// holding one undecodable record about one task would otherwise carry a
// permanent warning on every task it has.
func TestAnUnrelatedDeferredRecordDoesNotFlagThisTask(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	mine := r.createTask("mine")
	other := r.createTask("somebody else's")

	r.deferRecordOn(other.ID, other.Project)

	detail, err := r.reader.Task(t.Context(), mine.ID, worklet.DetailWants{},
		statelog.Freshness{Level: statelog.ReadSession})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !detail.Complete {
		t.Fatalf("a deferred record about another task made this one "+
			"incomplete: %+v", detail.Incomplete)
	}

	flagged, err := r.reader.Task(t.Context(), other.ID, worklet.DetailWants{},
		statelog.Freshness{Level: statelog.ReadSession})
	if err != nil {
		t.Fatalf("read the affected task: %v", err)
	}
	if flagged.Complete {
		t.Fatal("the task the deferred record is ABOUT reports complete, so " +
			"the probe cannot report anything at all")
	}
	if flagged.Incomplete == nil || flagged.Incomplete.Records != 1 {
		t.Fatalf("the affected task reports %+v", flagged.Incomplete)
	}
}

// BOTH DIRECTIONS OF A LINK COME BACK, and the mirror says it is the mirror.
//
// A reader sees "blocks" and "blocked by" without a second query and without
// knowing which end authored which — and an editor knows which end to change,
// which is what `derived` is for.
func TestALinkComesBackFromBothEnds(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	blocker := r.createTask("the blocker")
	blocked := r.createTask("the blocked")
	r.relate(blocked.ID, blocker.ID, worklet.RelationWaitingOn)

	from, err := r.reader.Task(t.Context(), blocked.ID,
		worklet.DetailWants{Links: true}, statelog.Freshness{Level: statelog.ReadSession})
	if err != nil {
		t.Fatalf("read the authoring end: %v", err)
	}
	if !slices.ContainsFunc(from.Links, func(l worklet.DetailLink) bool {
		return l.Other == blocker.ID && !l.Derived
	}) {
		t.Fatalf("the authoring end does not own its link: %+v", from.Links)
	}

	to, err := r.reader.Task(t.Context(), blocker.ID,
		worklet.DetailWants{Links: true}, statelog.Freshness{Level: statelog.ReadSession})
	if err != nil {
		t.Fatalf("read the other end: %v", err)
	}
	mirror := slices.IndexFunc(to.Links, func(l worklet.DetailLink) bool {
		return l.Other == blocked.ID
	})
	if mirror < 0 {
		t.Fatalf("the other end sees no link at all: %+v", to.Links)
	}
	if !to.Links[mirror].Derived {
		t.Fatal("the mirror does not say it is the mirror, so an editor would " +
			"change the end that did not author the edge")
	}
	if to.Links[mirror].Key == "" || to.Links[mirror].Title == "" {
		t.Fatalf("the link does not resolve the other end: %+v", to.Links[mirror])
	}
}

// ---- the helpers ------------------------------------------------------ //

// createTask writes one task and drains it into the rows.
func (r *roundTrip) createTask(title string) worklet.Task {
	r.t.Helper()
	task := newTask("t-" + strings.ToLower(strings.ReplaceAll(title, " ", "-")))
	task.Title = title
	task.Key = ""
	if _, err := r.writer.CreateTask(r.t.Context(), "op-"+task.ID, task, nil); err != nil {
		r.t.Fatalf("create %q: %v", title, err)
	}
	r.drain()
	detail, err := r.reader.Task(r.t.Context(), task.ID, worklet.DetailWants{},
		statelog.Freshness{Level: statelog.ReadSession})
	if err != nil {
		r.t.Fatalf("read back %q: %v", title, err)
	}
	return detail.Task
}

// relate writes one authored edge and drains it.
func (r *roundTrip) relate(from, to string, kind worklet.RelationKind) {
	r.t.Helper()
	if _, err := r.writer.UpdateTask(r.t.Context(), "op-rel-"+from+to, from, "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{
			Relations: &[]worklet.Relation{{Kind: kind, Other: to}},
		}, worklet.ChangeRelations, nil); err != nil {
		r.t.Fatalf("relate: %v", err)
	}
	r.drain()
}

// deferRecordOn writes a deferred record naming one task, which is what a
// record from a newer build looks like on this node.
func (r *roundTrip) deferRecordOn(taskID, project string) {
	r.t.Helper()
	r.deferRecordAt(taskID, worklet.ScopeTerm{
		Kind: worklet.TermObject, ID: taskID, Container: project,
	}.Path())
}

// deferRecordAt files one undecodable record about taskID under scope, which
// is how a newer peer's record looks to this build.
func (r *roundTrip) deferRecordAt(taskID, scope string) {
	r.t.Helper()
	if err := r.db.Tx(r.t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(r.t.Context(), `
			INSERT INTO tracker_log_deferred
				(position, subject, subject_kind, subject_id, version, payload, stored_at)
			VALUES (?, ?, 'task', ?, ?, x'00', 0)`,
			int64(1)<<40|9_000_000, "task."+taskID, taskID,
			worklet.RecordVersion+1); err != nil {
			return err
		}
		_, err := tx.ExecContext(r.t.Context(), `
			INSERT INTO tracker_log_deferred_scope (position, path) VALUES (?, ?)`,
			int64(1)<<40|9_000_000, scope)
		return err
	}); err != nil {
		r.t.Fatalf("defer a record: %v", err)
	}
}

func strptr(s string) *string { return &s }

func isNoTask(err error) bool { return errors.Is(err, worklet.ErrNoTask) }

// A COMMENT IS A ROW, and until one was written the whole thread was invisible.
//
// `tracker_comments` was DELETEd on purge, READ by get_task's thread, by
// `has_open_asks`, by `asked_of`, by `asked_by` and by my_work's own block —
// and INSERTed by nothing. Every comment the company had ever written landed
// in its task's document and produced no row, so every one of those answered
// as though nobody had ever said anything.
func TestACommentIsARowAndAnAnswerClosesItsAsk(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	created := r.createTask("who owns the rollback")

	question := "who owns the rollback?"
	if _, err := r.writer.UpdateTask(t.Context(), "op-ask", created.ID, "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Comment: &worklet.Comment{
			ID: "cm-1", Task: created.ID, Author: "ana",
			AuthorKind: worklet.AuthorHuman, Body: question, Ask: "bob",
			CreatedAt: wednesday,
		}}, worklet.ChangeComment, nil); err != nil {
		t.Fatalf("comment: %v", err)
	}
	r.drain()

	detail, err := r.reader.Task(t.Context(), created.ID,
		worklet.DetailWants{Comments: true}, statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		t.Fatalf("read the thread: %v", err)
	}
	if len(detail.Comments) != 1 || detail.Comments[0].Body != question {
		t.Fatalf("the thread holds %+v, want the one comment — a comment that "+
			"produced no row is a comment nobody can read", detail.Comments)
	}

	// AND THE ASK IS OPEN, which is a filter over that same row.
	if got := ids(r.ask(map[string]any{
		"container": "project:ENG", "has_open_asks": "true",
	})); len(got) != 1 || got[0] != created.ID {
		t.Fatalf("has_open_asks answers %v, want the task with the question", got)
	}
	if got := ids(r.ask(map[string]any{
		"container": "project:ENG", "asked_of": "bob",
	})); len(got) != 1 {
		t.Fatalf("asked_of=bob answers %v, want the task", got)
	}

	// AN ANSWER CLOSES IT. The two are separate rows — a reply is its own
	// comment — so without the stamp the ask stayed open on every board
	// and in the answerer's own queue for ever.
	answers := "cm-1"
	if _, err := r.writer.UpdateTask(t.Context(), "op-answer", created.ID, "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Comment: &worklet.Comment{
			ID: "cm-2", Task: created.ID, Author: "bob",
			AuthorKind: worklet.AuthorHuman, Body: "platform does",
			Answers: &answers, CreatedAt: wednesday,
		}}, worklet.ChangeComment, nil); err != nil {
		t.Fatalf("answer: %v", err)
	}
	r.drain()
	if got := ids(r.ask(map[string]any{
		"container": "project:ENG", "has_open_asks": "true",
	})); len(got) != 0 {
		t.Fatalf("has_open_asks still answers %v after the question was "+
			"answered", got)
	}

	// AND AN EDIT REPLACES THE ROW rather than adding one: a comment is
	// edited, resolved and removed in place, and an insert-only write
	// would leave the thread showing the first version for ever.
	if _, err := r.writer.UpdateTask(t.Context(), "op-edit", created.ID, "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Comment: &worklet.Comment{
			ID: "cm-1", Task: created.ID, Author: "ana",
			AuthorKind: worklet.AuthorHuman, Body: "who owns the rollback now?",
			Ask: "bob", CreatedAt: wednesday, UpdatedAt: wednesday,
		}}, worklet.ChangeComment, nil); err != nil {
		t.Fatalf("edit: %v", err)
	}
	r.drain()
	edited, err := r.reader.Task(t.Context(), created.ID,
		worklet.DetailWants{Comments: true}, statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		t.Fatalf("read the thread: %v", err)
	}
	if len(edited.Comments) != 2 {
		t.Fatalf("the thread holds %d comments after an EDIT, want 2 — an "+
			"edit is the same row", len(edited.Comments))
	}
	for _, comment := range edited.Comments {
		if comment.ID == "cm-1" && comment.Body != "who owns the rollback now?" {
			t.Errorf("the edited comment still reads %q", comment.Body)
		}
	}
}

// A CHECKLIST ITEM IS A ROW TOO, and it lives on somebody else's task.
//
// No assignee filter over tasks reaches one, so a seat holding six checklist
// items and no assignment read its queue as empty — and `checklist_assignee=`,
// which the shipped partial index is named for, matched nothing at all.
func TestAChecklistItemIsARow(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	created := r.createTask("the release checklist")

	lists := []worklet.Checklist{{ID: "l-1", Name: "Release", Items: []worklet.ChecklistItem{
		{ID: "i-1", Name: "cut the tag", Assignee: "bob"},
		{ID: "i-2", Name: "publish the notes", Assignee: "ana", Done: true},
	}}}
	if _, err := r.writer.UpdateTask(t.Context(), "op-checklist", created.ID,
		"ENG", worklet.NoIfMatch,
		worklet.TaskPatch{Checklists: &lists}, worklet.ChangeChecklist, nil); err != nil {
		t.Fatalf("checklist: %v", err)
	}
	r.drain()

	if got := ids(r.ask(map[string]any{
		"container": "project:ENG", "checklist_assignee": "bob",
	})); len(got) != 1 || got[0] != created.ID {
		t.Fatalf("checklist_assignee=bob answers %v, want the task carrying "+
			"his item", got)
	}

	// AND A REMOVED ITEM LEAVES THE TABLE. The collection is rebuilt from
	// the document on every apply, so an upsert-only write would keep
	// answering for an item nobody can see any more.
	shorter := []worklet.Checklist{{ID: "l-1", Name: "Release",
		Items: []worklet.ChecklistItem{lists[0].Items[1]}}}
	if _, err := r.writer.UpdateTask(t.Context(), "op-shorter", created.ID,
		"ENG", worklet.NoIfMatch,
		worklet.TaskPatch{Checklists: &shorter}, worklet.ChangeChecklist, nil); err != nil {
		t.Fatalf("checklist: %v", err)
	}
	r.drain()
	if got := ids(r.ask(map[string]any{
		"container": "project:ENG", "checklist_assignee": "bob",
	})); len(got) != 0 {
		t.Fatalf("checklist_assignee=bob still answers %v after his item was "+
			"deleted", got)
	}
}

// EVERY COMMENT ON A PAGE IS WHOLE, AND THE BYTES END A PAGE, NEVER A COMMENT.
//
// The page used to carry each body cut to two kilobytes, so a comment past
// that read as a comment that ended there. Now a page holds whole comments up
// to [worklet.CommentPageBytes] (at least one), and its cursor continues
// exactly where it stopped. This writes a thread of long comments, each with a
// non-ASCII character where the old cut fell, and walks it: every comment must
// arrive exactly once and byte for byte as written.
func TestAThreadPageCarriesWholeCommentsAndPagesByBytes(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	created := r.createTask("the incident write-up")
	const total = 7
	written := map[string]string{}
	for i := range total {
		body := strings.Repeat("a", 2047) + "é" + strings.Repeat(string(rune('b'+i)), 5000)
		id := fmt.Sprintf("cm-long-%d", i)
		written[id] = body
		if _, err := r.writer.UpdateTask(t.Context(), "op-"+id, created.ID, "ENG",
			worklet.NoIfMatch, worklet.TaskPatch{Comment: &worklet.Comment{
				ID: id, Task: created.ID, Author: "ana",
				AuthorKind: worklet.AuthorHuman, Body: body,
				CreatedAt: wednesday.Add(time.Duration(i) * time.Minute),
			}}, worklet.ChangeComment, nil); err != nil {
			t.Fatalf("comment %d: %v", i, err)
		}
		r.drain()
	}

	seen := map[string]bool{}
	cursor, pages := "", 0
	for {
		page, err := r.reader.Task(t.Context(), created.ID, worklet.DetailWants{
			Comments: true, CommentCursor: cursor,
		}, statelog.Freshness{Level: statelog.ReadStale})
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		pages++
		weight := 0
		for _, c := range page.Comments {
			if c.Body != written[c.ID] {
				t.Fatalf("%s came back as %d bytes of the %d written", c.ID, len(c.Body), len(written[c.ID]))
			}
			if seen[c.ID] {
				t.Fatalf("%s came back on two pages", c.ID)
			}
			seen[c.ID] = true
			weight += len(c.Body)
		}
		if len(page.Comments) == 0 {
			t.Fatal("a page of a non-empty thread was empty")
		}
		if len(page.Comments) > 1 && weight > worklet.CommentPageBytes {
			t.Fatalf("page %d holds %d bytes of comments, past its %d", pages, weight, worklet.CommentPageBytes)
		}
		if page.CommentsCursor == "" {
			break
		}
		cursor = page.CommentsCursor
		if pages > total {
			t.Fatal("the cursor never ran out")
		}
	}
	if len(seen) != total {
		t.Fatalf("walked %d of %d comments", len(seen), total)
	}
	if pages < 2 {
		t.Fatal("seven 7 KiB comments fit one page, so the byte bound was not exercised")
	}

	// AND ONE BY ID IS EXACT, with no thread cursor behind it.
	opened, err := r.reader.Task(t.Context(), created.ID,
		worklet.DetailWants{Comment: "cm-long-3"}, statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		t.Fatalf("open one comment: %v", err)
	}
	if len(opened.Comments) != 1 || opened.Comments[0].Body != written["cm-long-3"] || opened.CommentsCursor != "" {
		t.Fatalf("opening one comment answered %d comment(s), cursor %q", len(opened.Comments), opened.CommentsCursor)
	}
}

// EVERY COMMENT IS REACHABLE, PAST THE DETAIL'S TWENTY.
//
// The detail returns the newest [worklet.DetailComments] and a cursor, and a
// thread longer than that was cut there on every screen: nothing followed the
// cursor. This walks a thread of 57 at a page of 25 and must meet every
// comment exactly once, oldest-to-newest inside each page, and end with no
// cursor — and a page asked above the ceiling is held to it.
func TestWorkCommentsPagesPastTheDetailsTwenty(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	created := r.createTask("a long conversation")
	const total = 57
	for i := range total {
		if _, err := r.writer.UpdateTask(t.Context(), fmt.Sprintf("op-c-%d", i),
			created.ID, "ENG", worklet.NoIfMatch, worklet.TaskPatch{Comment: &worklet.Comment{
				ID: fmt.Sprintf("cm-%02d", i), Task: created.ID, Author: "ana",
				AuthorKind: worklet.AuthorHuman, Body: fmt.Sprintf("comment %d", i),
				CreatedAt: wednesday.Add(time.Duration(i) * time.Minute),
			}}, worklet.ChangeComment, nil); err != nil {
			t.Fatalf("comment %d: %v", i, err)
		}
		// APPLIED BEFORE THE NEXT, since each write is decided from the
		// state the one before it left.
		r.drain()
	}

	first, err := r.reader.Task(t.Context(), created.ID, worklet.DetailWants{Comments: true},
		statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if len(first.Comments) != worklet.DetailComments || first.CommentsCursor == "" {
		t.Fatalf("the detail holds %d comments and cursor %q, want its own page of %d and a cursor",
			len(first.Comments), first.CommentsCursor, worklet.DetailComments)
	}

	seen := map[string]bool{}
	cursor, pages := "", 0
	for {
		page, err := r.reader.Task(t.Context(), created.ID, worklet.DetailWants{
			Comments: true, CommentCursor: cursor, CommentLimit: 25,
		}, statelog.Freshness{Level: statelog.ReadStale})
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		pages++
		for i, c := range page.Comments {
			if seen[c.ID] {
				t.Fatalf("%s came back on two pages", c.ID)
			}
			seen[c.ID] = true
			if i > 0 && c.CreatedAt.Before(page.Comments[i-1].CreatedAt) {
				t.Errorf("page %d is not in written order at %s", pages, c.ID)
			}
		}
		if page.CommentsCursor == "" {
			break
		}
		cursor = page.CommentsCursor
		if pages > total {
			t.Fatal("the cursor never ran out")
		}
	}
	if len(seen) != total || pages != 3 {
		t.Fatalf("walked %d of %d comments in %d pages, want all of them in 3", len(seen), total, pages)
	}

	held, err := r.reader.Task(t.Context(), created.ID, worklet.DetailWants{
		Comments: true, CommentLimit: 500,
	}, statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		t.Fatalf("an oversized page: %v", err)
	}
	if len(held.Comments) != worklet.MaxCommentPage {
		t.Errorf("a page asked at 500 held %d, want the ceiling %d", len(held.Comments), worklet.MaxCommentPage)
	}
}

// A TASK FILED AND A REMARK MADE THROUGH A PERSON'S OWN TOKEN NAME THE PERSON.
//
// Bound to a seat, a person writes AS it (iam.ActorFor): the reporter and the
// comment's author ARE the seat, and the token is the operator id the history
// keeps beside the actor. There is no second "the seat behind the token" field
// a screen must prefer over the record.
func TestATaskFiledThroughAPersonsTokenNamesThePerson(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	jane := r.writer.As("jane-founder", worklet.AuthorHuman, worklet.Provenance{
		OperatorID: "pat:jane-laptop",
	})
	filed := newTask("t-filed-by-token")
	filed.Key = ""
	filed.Reporter = "jane-founder"
	if _, err := jane.CreateTask(t.Context(), "op-filed", filed, nil); err != nil {
		t.Fatalf("create through the token: %v", err)
	}
	r.drain()
	remark := worklet.Comment{ID: "c-token", Task: filed.ID, Author: "jane-founder",
		AuthorKind: worklet.AuthorHuman, Body: "hold it", CreatedAt: wednesday}
	if _, err := jane.UpdateTask(t.Context(), "op-remark", filed.ID, "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Comment: &remark}, worklet.ChangeComment,
		nil); err != nil {
		t.Fatalf("comment through the token: %v", err)
	}
	r.drain()

	got, err := r.reader.Task(t.Context(), filed.ID, worklet.DetailWants{
		Comments: true, History: true,
	}, statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Task.Reporter != "jane-founder" {
		t.Errorf("reporter %q, want the person's seat", got.Task.Reporter)
	}
	if len(got.Comments) != 1 || got.Comments[0].Author != "jane-founder" {
		t.Errorf("the thread is %+v, want one remark by jane-founder", got.Comments)
	}
	if len(got.History) == 0 {
		t.Fatal("the task has no history")
	}
	for _, entry := range got.History {
		if entry.Actor != "jane-founder" || entry.ActorKind != worklet.AuthorHuman ||
			entry.OperatorID != "pat:jane-laptop" {
			t.Errorf("history row %s is %q (%s) via %q, want jane as a person "+
				"through her token", entry.Kind, entry.Actor, entry.ActorKind,
				entry.OperatorID)
		}
	}
}

// A COMMENT ID THAT IS NOT ON THIS TASK IS ITS OWN ANSWER.
//
// Its own sentinel beside ErrNoTask, because the caller's answer differs: a
// mistyped id is not an empty thread, and a reader told "no comments" would go
// looking for the wrong thing. Scoped to the task for the same reason — an id
// from another item must not quietly open a thread the caller was not reading.
func TestAnUnknownCommentIsItsOwnAnswer(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	mine := r.createTask("mine")
	theirs := r.createTask("theirs")
	if _, err := r.writer.UpdateTask(t.Context(), "op-comment", theirs.ID, "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Comment: &worklet.Comment{
			ID: "cm-elsewhere", Task: theirs.ID, Author: "ana",
			AuthorKind: worklet.AuthorHuman, Body: "on the other item",
			CreatedAt: wednesday,
		}}, worklet.ChangeComment, nil); err != nil {
		t.Fatalf("comment: %v", err)
	}
	r.drain()

	for _, id := range []string{"cm-nothing", "cm-elsewhere"} {
		_, err := r.reader.Task(t.Context(), mine.ID,
			worklet.DetailWants{Comment: id}, statelog.Freshness{Level: statelog.ReadStale})
		if !errors.Is(err, worklet.ErrNoComment) {
			t.Errorf("opening %q on a task that does not have it answered %v, "+
				"which a caller cannot tell from a store it could not reach",
				id, err)
		}
	}
}

// A THREAD READ REFUSES WHERE ITS TASK IS FILED.
//
// A thread read decides who a wake reaches, so a record this node cannot
// decode covering the task means the comment rows it routes from may already
// be wrong — and it refuses. It used to form its scope from the task id alone,
// under the workspace rather than the project the task is filed in, so it
// probed a path no record is filed under and routed from those rows anyway.
func TestAThreadReadRefusesWhereItsTaskIsFiled(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	affected := r.createTask("the one a newer build wrote to")
	other := r.createTask("somebody else's")
	r.deferRecordOn(affected.ID, affected.Project)

	fresh := statelog.Freshness{Level: statelog.ReadSession}
	_, err := r.reader.Thread(t.Context(), worklet.ThreadQuery{Task: affected.ID}, fresh)
	var refusal *statelog.Refused
	if !errors.As(err, &refusal) || refusal.Code != statelog.RefuseDeferred {
		t.Fatalf("a thread read about a task a deferred record covers = %v, "+
			"want a deferred refusal", err)
	}
	if _, err := r.reader.Thread(t.Context(), worklet.ThreadQuery{Task: other.ID}, fresh); err != nil {
		t.Fatalf("a thread read about an unaffected task = %v, want it served", err)
	}
}

// A TASK READ BY KEY ACCOUNTS FOR THE KEY'S OWN ADDRESS.
//
// A deferred alias claim or re-key is filed under the key rather than under
// the task, so a read that names the task BY that key is about it too. The
// probe used to cover only the task's own path, so a record rewriting what the
// key points at left the answer complete.
func TestATaskReadByKeyAccountsForTheKeysOwnAddress(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	task := r.createTask("named by its key")
	r.deferRecordAt(task.ID, worklet.ScopeTerm{
		Kind: worklet.TermKey, Container: task.Project, ID: task.Key,
	}.Path())

	byKey, err := r.reader.Task(t.Context(), task.Key, worklet.DetailWants{},
		statelog.Freshness{Level: statelog.ReadSession})
	if err != nil {
		t.Fatalf("read by key: %v", err)
	}
	if byKey.Complete || byKey.Incomplete == nil {
		t.Fatalf("a read by a key a deferred record is filed under reports "+
			"complete: %+v", byKey.Incomplete)
	}

	// THE CONTROL: the same task read by id is not about that address, so
	// the case above cannot pass on a probe that flags every read.
	byID, err := r.reader.Task(t.Context(), task.ID, worklet.DetailWants{},
		statelog.Freshness{Level: statelog.ReadSession})
	if err != nil {
		t.Fatalf("read by id: %v", err)
	}
	if !byID.Complete {
		t.Fatalf("a read by id was flagged by a deferral on the key's address: %+v",
			byID.Incomplete)
	}
}

// A TASK NAMES THE TASK IT IS FILED UNDER.
//
// A task carries its parent's id — the record — and a page heading itself
// "Part of LEAD-12 · 2.4 release" needed a second read of the parent to say
// it. The detail answers the parent's key, title and status in its own
// transaction, and a top-level task names none.
func TestTheDetailNamesTheTaskItIsFiledUnder(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	parent := r.createTask("The 2.4 release")
	child := r.createTask("Retry PXE boot")
	if _, err := r.writer.UpdateTask(t.Context(), "op-parent", child.ID, "ENG", worklet.NoIfMatch,
		worklet.TaskPatch{Parent: strptr(parent.ID)}, worklet.ChangeReparented, nil); err != nil {
		t.Fatalf("re-parent: %v", err)
	}
	r.drain()

	detail, err := r.reader.Task(t.Context(), child.Key, worklet.DetailWants{},
		statelog.Freshness{Level: statelog.ReadSession})
	if err != nil {
		t.Fatalf("read the child: %v", err)
	}
	want := worklet.TaskRef{ID: parent.ID, Key: parent.Key, Title: "The 2.4 release",
		Status: parent.Status}
	if detail.Parent == nil || *detail.Parent != want {
		t.Fatalf("the child names its parent as %+v, want %+v", detail.Parent, want)
	}
	top, err := r.reader.Task(t.Context(), parent.Key, worklet.DetailWants{},
		statelog.Freshness{Level: statelog.ReadSession})
	if err != nil {
		t.Fatalf("read the parent: %v", err)
	}
	if top.Parent != nil {
		t.Errorf("a top-level task names a parent %+v", top.Parent)
	}
}

// A THREAD'S CURSOR SAYS WHETHER THAT THREAD GOES BACK, NOT THE TASK.
//
// A pane drawing one question's replies used to page the task's whole thread
// and keep the replies, so a task with more than a page of comments offered
// "earlier" replies to a question nobody had replied to. Narrowed by
// [worklet.CommentThread], an unanswered ask on a busy task is one empty page
// with no cursor, and its replies and answer walk back exactly once each
// with nothing else on the task among them; the root joins only when asked.
func TestACommentThreadPagesOnlyItsOwnConversation(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	created := r.createTask("the brand palette")
	at := func(minute int) time.Time { return wednesday.Add(time.Duration(minute) * time.Minute) }
	comment := func(id string, minute int, replyTo string) {
		t.Helper()
		c := worklet.Comment{ID: id, Task: created.ID, Author: "ana",
			AuthorKind: worklet.AuthorHuman, Body: "about " + id, CreatedAt: at(minute)}
		if replyTo != "" {
			c.ReplyTo = &replyTo
		}
		askOn(t, r, "op-"+id, created.ID, c)
	}
	read := func(thread worklet.CommentThread, cursor string, limit int) worklet.TaskDetail {
		t.Helper()
		page, err := r.reader.Task(t.Context(), created.ID, worklet.DetailWants{
			Comments: true, CommentThread: thread, CommentCursor: cursor, CommentLimit: limit,
		}, statelog.Freshness{Level: statelog.ReadStale})
		if err != nil {
			t.Fatalf("read the thread: %v", err)
		}
		return page
	}
	walk := func(thread worklet.CommentThread) []string {
		t.Helper()
		var ids []string
		cursor := ""
		for pages := 0; ; pages++ {
			if pages > 10 {
				t.Fatal("the cursor never ran out")
			}
			page := read(thread, cursor, 10)
			if len(page.Comments) == 0 {
				t.Fatal("a page of a non-empty thread was empty")
			}
			got := make([]string, 0, len(page.Comments))
			for _, c := range page.Comments {
				got = append(got, c.ID)
			}
			ids = append(got, ids...)
			if cursor = page.CommentsCursor; cursor == "" {
				return ids
			}
		}
	}

	askOn(t, r, "op-ask", created.ID, worklet.Comment{ID: "cm-ask", Task: created.ID,
		Author: "bo", AuthorKind: worklet.AuthorAgent, Body: "Which yellow?", Ask: "ana",
		CreatedAt: at(0)})
	for i := range 25 {
		comment(fmt.Sprintf("cm-other-%02d", i), 1+i, "")
	}

	// THE TASK GOES BACK; THE QUESTION'S CONVERSATION DOES NOT.
	if whole := read(worklet.CommentThread{}, "", 0); whole.CommentsCursor == "" {
		t.Fatal("26 comments fit the detail's page, so the task's cursor was not exercised")
	}
	empty := read(worklet.CommentThread{Root: "cm-ask"}, "", 0)
	if len(empty.Comments) != 0 || empty.CommentsCursor != "" {
		t.Fatalf("an ask with no replies paged %d comment(s) and cursor %q, want none and none",
			len(empty.Comments), empty.CommentsCursor)
	}

	var want []string
	for i := range 21 {
		id := fmt.Sprintf("cm-reply-%02d", i)
		comment(id, 30+2*i, "cm-ask")
		comment(fmt.Sprintf("cm-aside-%02d", i), 31+2*i, "")
		want = append(want, id)
	}
	answerOn(t, r, "op-answer", created.ID, "cm-answer", "ana", "cm-ask", "", "The warm one.", at(80))
	want = append(want, "cm-answer")

	if got := walk(worklet.CommentThread{Root: "cm-ask"}); !slices.Equal(got, want) {
		t.Fatalf("the ask's thread walked\n  %v\nwant its replies and answer in order\n  %v", got, want)
	}
	withRoot := append([]string{"cm-ask"}, want...)
	if got := walk(worklet.CommentThread{Root: "cm-ask", WithRoot: true}); !slices.Equal(got, withRoot) {
		t.Fatalf("the thread with its root walked\n  %v\nwant\n  %v", got, withRoot)
	}
	// A ROOT THE TASK DOES NOT HOLD NARROWS TO NOTHING, never to another
	// task's conversation: the read stays on the task it was asked of.
	other := r.createTask("another task")
	askOn(t, r, "op-elsewhere", other.ID, worklet.Comment{ID: "cm-elsewhere", Task: other.ID,
		Author: "ana", AuthorKind: worklet.AuthorHuman, Body: "elsewhere", CreatedAt: at(90)})
	elsewhere := "cm-elsewhere"
	askOn(t, r, "op-elsewhere-reply", other.ID, worklet.Comment{ID: "cm-elsewhere-reply",
		Task: other.ID, Author: "bo", AuthorKind: worklet.AuthorAgent, Body: "a reply",
		ReplyTo: &elsewhere, CreatedAt: at(91)})
	for _, root := range []string{"cm-nobody-wrote", "cm-elsewhere"} {
		page := read(worklet.CommentThread{Root: root, WithRoot: true}, "", 0)
		if len(page.Comments) != 0 || page.CommentsCursor != "" {
			t.Errorf("root %s paged %d comment(s) and cursor %q on %s, want none",
				root, len(page.Comments), page.CommentsCursor, created.ID)
		}
	}
}

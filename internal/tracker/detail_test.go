package tracker_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
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

	if _, err := r.writer.UpdateTask(t.Context(), "op-assign", created.ID, "ENG", tracker.NoIfMatch,
		tracker.TaskPatch{Assignee: strptr("ana")}, tracker.ChangeAssignee, nil); err != nil {
		t.Fatalf("assign: %v", err)
	}
	r.drain()

	detail, err := r.reader.Task(t.Context(), created.Key, tracker.DetailWants{
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
	if detail.LogSeq == 0 {
		t.Fatal("the answer names no position, so a caller cannot tell how far " +
			"behind it may be")
	}

	// THE PARTS NOT ASKED FOR ARE ABSENT.
	bare, err := r.reader.Task(t.Context(), created.ID, tracker.DetailWants{},
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
	_, err := r.reader.Task(t.Context(), "ENG-9999", tracker.DetailWants{},
		statelog.Freshness{Level: statelog.ReadSession})
	if err == nil {
		t.Fatal("a task nobody has read back")
	}
	if !isNoTask(err) {
		t.Fatalf("a missing task answers %v, which a caller cannot tell from "+
			"a store it could not reach", err)
	}
}

// A DEFERRED RECORD ABOUT ANOTHER TASK DOES NOT TOUCH THIS ONE, AND THE TASK
// IT IS ABOUT IS REFUSED.
//
// The coverage probe is scoped to the OBJECT here and to the container on a
// board, and the difference is the whole reason they are two calls: a company
// holding one undecodable record about one task would otherwise refuse every
// task it has. And a point read REFUSES where a board reports a gap, because
// the answer is about that one task and its rows may already be wrong.
//
// This harness's node reports nothing deferred, so the framework's own probe
// never runs and the refusal is the one the read's transaction makes — the
// probe that covers a task this node did not hold before the read.
//
// Mutation: answer a held task from refuseUnaccounted without refuseCovered,
// and the affected task reads back.
func TestAnUnrelatedDeferredRecordDoesNotFlagThisTask(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	mine := r.createTask("mine")
	other := r.createTask("somebody else's")

	r.deferRecordOn(other.ID, other.Project)

	if _, err := r.reader.Task(t.Context(), mine.ID, tracker.DetailWants{},
		statelog.Freshness{Level: statelog.ReadSession}); err != nil {
		t.Fatalf("a deferred record about another task refused this one: %v", err)
	}

	_, err := r.reader.Task(t.Context(), other.ID, tracker.DetailWants{},
		statelog.Freshness{Level: statelog.ReadSession})
	var refused *statelog.Refused
	if !errors.As(err, &refused) || refused.Code != statelog.RefuseDeferred {
		t.Fatalf("the task the deferred record is ABOUT answered %v, want the "+
			"read refused as deferred", err)
	}
	if !strings.Contains(refused.Detail, other.ID) {
		t.Errorf("the refusal says %q, which does not name the task", refused.Detail)
	}
}

// A RECORD DEFERRED ON A TASK'S PROJECT REFUSES THE TASK BEFORE THE READ WAITS
// — its detail and its thread alike.
//
// A scope path nests an object under its container, so the record filed under
// the project covers every task in it. The framework's own probe runs before
// any wait and meets it only when the read names the task under that project —
// named under the workspace, the read waits out a floor this node never
// reaches and is refused as behind, for a task whose rows no wait can fix. The
// thread a comment routes from is a point read about the same task, so it is
// scoped the same way.
//
// Mutation: form the framework read's scope from the reference alone in
// Reader.pointTerm and both answer `behind`.
func TestARecordDeferredOnTheProjectRefusesTheTaskBeforeItWaits(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	task := r.createTask("in a project somebody's newer build wrote to")
	r.deferRecordAt(task.ID, tracker.ScopeTerm{
		Kind: tracker.TermContainer, ID: task.Project,
	}.Path())

	at := statelog.Position{Stream: tracker.Domain{}.Stream().Name, Generation: 1, Seq: 1}
	var lag, first, floor uint64 = 0, 1, 0
	log, err := statelog.NewReader(statelog.ReaderDeps{
		Domain: tracker.Domain{}, DB: r.db.Replicated(), Waiter: stuckWaiter{at: at},
		Health: func() statelog.Health {
			return statelog.Health{
				Position: at, AppliedThrough: at.Seq, CaughtUp: true,
				Floor: statelog.Floor{State: statelog.FloorOK, ReadAt: time.Now()},
				Lag:   &lag, FirstSeq: &first, TrimFloor: &floor,
				Deferred: 1,
			}
		},
	})
	if err != nil {
		t.Fatalf("build the framework reader: %v", err)
	}
	reader, err := tracker.NewReader(r.db, log)
	if err != nil {
		t.Fatalf("build the tracker reader: %v", err)
	}

	// A FLOOR THIS NODE NEVER REACHES, so a read the probe let through
	// waits and is refused as behind.
	unreached := statelog.Freshness{Level: statelog.ReadSession,
		MinPosition: statelog.Position{
			Stream: at.Stream, Generation: at.Generation, Seq: at.Seq + 1,
		}}
	for name, read := range map[string]func() error{
		"the detail": func() error {
			_, err := reader.Task(t.Context(), task.Key, tracker.DetailWants{}, unreached)
			return err
		},
		"the thread": func() error {
			_, err := reader.Thread(t.Context(), tracker.ThreadQuery{
				Task: task.ID, Author: "ana",
			}, unreached)
			return err
		},
	} {
		var refused *statelog.Refused
		if err := read(); !errors.As(err, &refused) ||
			refused.Code != statelog.RefuseDeferred {
			t.Errorf("%s of a task in a project a deferred record covers "+
				"answered %v, want the read refused as deferred before it "+
				"waited", name, err)
		}
	}
}

// A THREAD ON A TASK A RETAINED RECORD COVERS IS REFUSED, AND ONE ON ITS
// NEIGHBOUR IS NOT.
//
// A comment routes its wake from this read — who is already in the thread,
// whose question it answers — so a thread read over rows a record this node
// cannot decode may have made wrong would wake the wrong people and close the
// wrong ask. It is refused as the detail read is, and scoped as narrowly: a
// record about one task says nothing about the thread on another.
//
// This harness's node reports nothing deferred, so the framework's own probe
// never runs and the refusal is the one the read's transaction makes.
//
// Mutation: drop the refuseUnaccounted call from readThread and the covered
// thread answers.
func TestAThreadOnATaskARetainedRecordCoversIsRefused(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	mine := r.createTask("mine")
	other := r.createTask("somebody else's")
	r.deferRecordOn(other.ID, other.Project)

	if _, err := r.reader.Thread(t.Context(), tracker.ThreadQuery{
		Task: mine.ID, Author: "ana",
	}, statelog.Freshness{Level: statelog.ReadSession}); err != nil {
		t.Fatalf("a deferred record about another task refused this one's "+
			"thread: %v", err)
	}
	_, err := r.reader.Thread(t.Context(), tracker.ThreadQuery{
		Task: other.ID, Author: "ana",
	}, statelog.Freshness{Level: statelog.ReadSession})
	var refused *statelog.Refused
	if !errors.As(err, &refused) || refused.Code != statelog.RefuseDeferred {
		t.Fatalf("the thread on the task the deferred record is ABOUT "+
			"answered %v, want the read refused as deferred", err)
	}
}

// AN ABSENT TASK A RETAINED RECORD COULD CREATE IS REFUSED, NOT ANSWERED ABSENT.
//
// "There is no such task" is an answer a caller acts on: a seat files the
// duplicate, a screen shows a dead link. A node retaining the record that
// creates ENG-77 holds no row for it, so the answer it can give is that it
// cannot say — by the key, whose project names where that create is filed, and
// by the id, which names no project and is probed across all of them.
//
// Scoped by the key's project, so a record retained in ENG says nothing about
// OPS-77. And a PURGED task is absent whatever is retained, because the
// deletion gate drops every later record about it on every node.
//
// Mutations: return ErrNoTask from refuseAbsent without probing and all three
// references answer absent; scope a key across every project and OPS-77 is
// refused; drop the purge check and the purged key and id are refused.
func TestAnAbsentTaskARetainedRecordCouldCreateIsRefused(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	gone := r.createTask("filed twice")
	if _, err := r.writer.PurgeTask(t.Context(), "op-purge", gone.ID, gone.Project,
		"filed twice"); err != nil {
		t.Fatalf("purge %s: %v", gone.Key, err)
	}
	r.drain()
	r.deferRecordOn("t-unapplied", "ENG")

	session := statelog.Freshness{Level: statelog.ReadSession}
	for _, ref := range []string{"ENG-77", "eng-77", "t-unapplied"} {
		_, err := r.reader.Task(t.Context(), ref, tracker.DetailWants{}, session)
		var refused *statelog.Refused
		if !errors.As(err, &refused) || refused.Code != statelog.RefuseDeferred {
			t.Errorf("%s, which a retained record could create, answered %v — "+
				"want the read refused as deferred rather than told there is "+
				"no such task", ref, err)
			continue
		}
		if isNoTask(err) {
			t.Errorf("%s was refused AND answered absent: %v", ref, err)
		}
	}
	for _, ref := range []string{"OPS-77", gone.Key, gone.ID} {
		if _, err := r.reader.Task(t.Context(), ref, tracker.DetailWants{},
			session); !isNoTask(err) {
			t.Errorf("%s answered %v, want no such task — nothing retained "+
				"can create it", ref, err)
		}
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
	r.relate(blocked.ID, blocker.ID, tracker.RelationWaitingOn)

	from, err := r.reader.Task(t.Context(), blocked.ID,
		tracker.DetailWants{Links: true}, statelog.Freshness{Level: statelog.ReadSession})
	if err != nil {
		t.Fatalf("read the authoring end: %v", err)
	}
	if !slices.ContainsFunc(from.Links, func(l tracker.DetailLink) bool {
		return l.Other == blocker.ID && !l.Derived
	}) {
		t.Fatalf("the authoring end does not own its link: %+v", from.Links)
	}

	to, err := r.reader.Task(t.Context(), blocker.ID,
		tracker.DetailWants{Links: true}, statelog.Freshness{Level: statelog.ReadSession})
	if err != nil {
		t.Fatalf("read the other end: %v", err)
	}
	mirror := slices.IndexFunc(to.Links, func(l tracker.DetailLink) bool {
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
func (r *roundTrip) createTask(title string) tracker.Task {
	r.t.Helper()
	task := newTask("t-" + strings.ToLower(strings.ReplaceAll(title, " ", "-")))
	task.Title = title
	task.Key = ""
	if _, err := r.writer.CreateTask(r.t.Context(), "op-"+task.ID, task, nil); err != nil {
		r.t.Fatalf("create %q: %v", title, err)
	}
	r.drain()
	detail, err := r.reader.Task(r.t.Context(), task.ID, tracker.DetailWants{},
		statelog.Freshness{Level: statelog.ReadSession})
	if err != nil {
		r.t.Fatalf("read back %q: %v", title, err)
	}
	return detail.Task
}

// relate writes one authored edge and drains it.
func (r *roundTrip) relate(from, to string, kind tracker.RelationKind) {
	r.t.Helper()
	if _, err := r.writer.UpdateTask(r.t.Context(), "op-rel-"+from+to, from, "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{
			Relations: &[]tracker.Relation{{Kind: kind, Other: to}},
		}, tracker.ChangeRelations, nil); err != nil {
		r.t.Fatalf("relate: %v", err)
	}
	r.drain()
}

// deferRecordOn writes a deferred record naming one task, which is what a
// record from a newer build looks like on this node.
func (r *roundTrip) deferRecordOn(taskID, project string) {
	r.t.Helper()
	r.deferRecordAt(taskID, tracker.ScopeTerm{
		Kind: tracker.TermObject, ID: taskID, Container: project,
	}.Path())
}

// deferRecordAt writes a deferred record on one task whose declared scope is
// one path.
func (r *roundTrip) deferRecordAt(taskID, scope string) {
	r.t.Helper()
	if err := r.db.Replicated().Tx(r.t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(r.t.Context(), `
			INSERT INTO tracker_log_deferred
				(position, subject, subject_kind, subject_id, version, payload, stored_at)
			VALUES (?, ?, 'task', ?, ?, x'00', 0)`,
			int64(1)<<40|9_000_000, "task."+taskID, taskID,
			tracker.ReadableRecordVersion+1); err != nil {
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

// stuckWaiter is a node's applier that never moves past where it is.
type stuckWaiter struct{ at statelog.Position }

func (w stuckWaiter) Committed() statelog.Position { return w.at }

func (w stuckWaiter) WaitCommitted(_ context.Context, p statelog.Position) error {
	if p.Packed() <= w.at.Packed() {
		return nil
	}
	return errors.New("this applier does not move")
}

func (w stuckWaiter) WaitApplied(ctx context.Context, _ statelog.ScopeSet,
	p statelog.Position) error {
	return w.WaitCommitted(ctx, p)
}

func strptr(s string) *string { return &s }

func isNoTask(err error) bool { return errors.Is(err, tracker.ErrNoTask) }

// A COMMENT IS A ROW.
//
// `tracker_comments` is deleted on purge and read by the detail read's thread,
// by `has_open_asks`, by `asked_of`, by `asked_by` and by my_work's own block —
// so a comment that landed in its task's document and produced no row would
// leave every one of those answering as though nobody had ever said anything.
func TestACommentIsARowAndAnAnswerClosesItsAsk(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	created := r.createTask("who owns the rollback")

	question := "who owns the rollback?"
	if _, err := r.writer.UpdateTask(t.Context(), "op-ask", created.ID, "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Comment: &tracker.Comment{
			ID: "cm-1", Task: created.ID, Author: "ana",
			AuthorKind: tracker.AuthorHuman, Body: question, Ask: "bob",
			CreatedAt: wednesday,
		}}, tracker.ChangeComment, nil); err != nil {
		t.Fatalf("comment: %v", err)
	}
	r.drain()

	detail, err := r.reader.Task(t.Context(), created.ID,
		tracker.DetailWants{Comments: true}, statelog.Freshness{Level: statelog.ReadStale})
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
	// comment — so without the stamp the ask would stay open on every
	// board and in the answerer's own queue for ever.
	answers := "cm-1"
	if _, err := r.writer.UpdateTask(t.Context(), "op-answer", created.ID, "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Comment: &tracker.Comment{
			ID: "cm-2", Task: created.ID, Author: "bob",
			AuthorKind: tracker.AuthorHuman, Body: "platform does",
			Answers: &answers, CreatedAt: wednesday,
		}}, tracker.ChangeComment, nil); err != nil {
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
		tracker.NoIfMatch, tracker.TaskPatch{Comment: &tracker.Comment{
			ID: "cm-1", Task: created.ID, Author: "ana",
			AuthorKind: tracker.AuthorHuman, Body: "who owns the rollback now?",
			Ask: "bob", CreatedAt: wednesday, UpdatedAt: wednesday,
		}}, tracker.ChangeComment, nil); err != nil {
		t.Fatalf("edit: %v", err)
	}
	r.drain()
	edited, err := r.reader.Task(t.Context(), created.ID,
		tracker.DetailWants{Comments: true}, statelog.Freshness{Level: statelog.ReadStale})
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
// No assignee filter over tasks reaches one, so without its own row a seat
// holding six checklist items and no assignment would read its queue as empty,
// and `checklist_assignee=`, which the partial index is named for, would match
// nothing at all.
func TestAChecklistItemIsARow(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	created := r.createTask("the release checklist")

	lists := []tracker.Checklist{{ID: "l-1", Name: "Release", Items: []tracker.ChecklistItem{
		{ID: "i-1", Name: "cut the tag", Assignee: "bob"},
		{ID: "i-2", Name: "publish the notes", Assignee: "ana", Done: true},
	}}}
	if _, err := r.writer.UpdateTask(t.Context(), "op-checklist", created.ID,
		"ENG", tracker.NoIfMatch,
		tracker.TaskPatch{Checklists: &lists}, tracker.ChangeChecklist, nil); err != nil {
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
	shorter := []tracker.Checklist{{ID: "l-1", Name: "Release",
		Items: []tracker.ChecklistItem{lists[0].Items[1]}}}
	if _, err := r.writer.UpdateTask(t.Context(), "op-shorter", created.ID,
		"ENG", tracker.NoIfMatch,
		tracker.TaskPatch{Checklists: &shorter}, tracker.ChangeChecklist, nil); err != nil {
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

// A LONG COMMENT BODY IS AN EXCERPT IN THE PAGE AND WHOLE WHEN OPENED.
//
// The page is excerpted because twenty bodies at [tracker.MaxCommentBody] is
// ten times the ceiling on one tool answer. That is only legitimate if the
// rest is reachable: an excerpt pointing at a read the engine does not have
// leaves anything a person wrote past 2 KiB unrecoverable by any seat through
// any tool. This is that read, and the assertion that the two halves disagree
// — one cut and marked, one exactly what was written — is the whole point.
func TestALongCommentBodyIsAnExcerptWithAWayBackToTheWhole(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	created := r.createTask("the incident write-up")

	// Past the excerpt and well inside what a write accepts, with a
	// non-ASCII character ON the boundary: a byte slice there yields
	// invalid UTF-8, which is the other half of what the cut has to get
	// right.
	body := strings.Repeat("a", tracker.CommentBodyShown-1) + "é" +
		strings.Repeat("b", 500)
	if _, err := r.writer.UpdateTask(t.Context(), "op-comment", created.ID, "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Comment: &tracker.Comment{
			ID: "cm-long", Task: created.ID, Author: "ana",
			AuthorKind: tracker.AuthorHuman, Body: body, CreatedAt: wednesday,
		}}, tracker.ChangeComment, nil); err != nil {
		t.Fatalf("comment: %v", err)
	}
	r.drain()

	page, err := r.reader.Task(t.Context(), created.ID,
		tracker.DetailWants{Comments: true}, statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		t.Fatalf("read the thread: %v", err)
	}
	if len(page.Comments) != 1 {
		t.Fatalf("the thread holds %d comment(s), want 1", len(page.Comments))
	}
	excerpt := page.Comments[0].Body
	switch {
	case excerpt == body:
		t.Fatal("a body past the excerpt came back whole in the PAGE — " +
			"twenty of these is ten times what one tool answer may weigh")
	case !strings.HasSuffix(excerpt, "…"):
		t.Errorf("the excerpt is unmarked: %q — a body cut at exactly the cap "+
			"and handed over unmarked reads as a comment that ENDED there",
			excerpt[max(0, len(excerpt)-8):])
	case !utf8.ValidString(excerpt):
		t.Error("the excerpt is not valid UTF-8, so the cut went through a rune")
	}

	// AND THE WHOLE THING IS ONE READ AWAY. Without this the excerpt is
	// not a pointer, it is a loss.
	opened, err := r.reader.Task(t.Context(), created.ID,
		tracker.DetailWants{Comment: "cm-long"}, statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		t.Fatalf("open one comment: %v", err)
	}
	if len(opened.Comments) != 1 {
		t.Fatalf("opening one comment answered %d of them", len(opened.Comments))
	}
	if opened.Comments[0].Body != body {
		t.Fatalf("the opened comment is %d bytes and %d were written — opening "+
			"one is the read that has to be exact",
			len(opened.Comments[0].Body), len(body))
	}
	// IT REPLACES THE PAGE, so there is no cursor inviting a caller to walk
	// a thread it did not ask for.
	if opened.CommentsCursor != "" {
		t.Errorf("opening one comment carried a thread cursor %q",
			opened.CommentsCursor)
	}
	// AND IT IS READ WITHOUT `comments`: naming one IS asking for it, and a
	// caller that had to pass both would meet a silently empty thread.
	if len(opened.Comments) == 0 {
		t.Error("a read naming a comment but not `comments` came back empty")
	}
}

// A THREAD PAGED TO ITS END RETURNS EVERY COMMENT EXACTLY ONCE.
//
// The page reads one row past its size as evidence that a next page exists,
// and the cursor has to name the last row RETURNED rather than that probe:
// the next page is strictly older than the cursor, so a cursor naming the
// probe row puts it on neither page and loses one comment at every boundary.
// The instants tie in threes so the boundary falls inside a tie, which is
// what the id half of the cursor is for.
func TestAThreadPagedToItsEndReturnsEveryCommentOnce(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	created := r.createTask("a long thread")

	const total = tracker.DetailComments + 5
	written := make([]string, 0, total)
	for i := range total {
		id := fmt.Sprintf("cm-%03d", i)
		if _, err := r.writer.UpdateTask(t.Context(), "op-"+id, created.ID, "ENG",
			tracker.NoIfMatch, tracker.TaskPatch{Comment: &tracker.Comment{
				ID: id, Task: created.ID, Author: "ana",
				AuthorKind: tracker.AuthorHuman, Body: "comment " + id,
				CreatedAt: wednesday.Add(time.Duration(i/3) * time.Minute),
			}}, tracker.ChangeComment, nil); err != nil {
			t.Fatalf("comment %s: %v", id, err)
		}
		r.drain()
		written = append(written, id)
	}

	seen := map[string]int{}
	cursor := ""
	for page := 0; ; page++ {
		if page > total {
			t.Fatalf("the thread did not end after %d pages — the cursor is "+
				"not advancing", page)
		}
		got, err := r.reader.Task(t.Context(), created.ID,
			tracker.DetailWants{Comments: true, CommentCursor: cursor},
			statelog.Freshness{Level: statelog.ReadStale})
		if err != nil {
			t.Fatalf("read page %d: %v", page, err)
		}
		for _, comment := range got.Comments {
			seen[comment.ID]++
		}
		if got.CommentsCursor == "" {
			break
		}
		cursor = got.CommentsCursor
	}
	for _, id := range written {
		switch seen[id] {
		case 1:
		case 0:
			t.Errorf("%s was on no page — the thread lost it at a page "+
				"boundary, and nothing told the reader it existed", id)
		default:
			t.Errorf("%s was on %d pages", id, seen[id])
		}
	}
}

// A MALFORMED THREAD CURSOR IS REFUSED, NOT READ AS THE START OF TIME.
//
// The page predicate is "strictly older than the cursor", so a cursor whose
// instant decoded as zero would match nothing and answer an empty page — the
// same answer a thread with no older comments gives.
func TestAMalformedThreadCursorIsRefused(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	created := r.createTask("a thread with one comment")
	if _, err := r.writer.UpdateTask(t.Context(), "op-comment", created.ID, "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Comment: &tracker.Comment{
			ID: "cm-one", Task: created.ID, Author: "ana",
			AuthorKind: tracker.AuthorHuman, Body: "hello", CreatedAt: wednesday,
		}}, tracker.ChangeComment, nil); err != nil {
		t.Fatalf("comment: %v", err)
	}
	r.drain()

	for _, cursor := range []string{"not-a-cursor", "yesterday:cm-one", "12345:"} {
		got, err := r.reader.Task(t.Context(), created.ID,
			tracker.DetailWants{Comments: true, CommentCursor: cursor},
			statelog.Freshness{Level: statelog.ReadStale})
		if err == nil {
			t.Errorf("cursor %q was accepted and answered %d comment(s) — a "+
				"cursor this read did not mint must be refused by name",
				cursor, len(got.Comments))
			continue
		}
		if !strings.Contains(err.Error(), "comments_cursor") {
			t.Errorf("cursor %q was refused with %q, which does not name the "+
				"argument to fix", cursor, err)
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
		tracker.NoIfMatch, tracker.TaskPatch{Comment: &tracker.Comment{
			ID: "cm-elsewhere", Task: theirs.ID, Author: "ana",
			AuthorKind: tracker.AuthorHuman, Body: "on the other item",
			CreatedAt: wednesday,
		}}, tracker.ChangeComment, nil); err != nil {
		t.Fatalf("comment: %v", err)
	}
	r.drain()

	for _, id := range []string{"cm-nothing", "cm-elsewhere"} {
		_, err := r.reader.Task(t.Context(), mine.ID,
			tracker.DetailWants{Comment: id}, statelog.Freshness{Level: statelog.ReadStale})
		if !errors.Is(err, tracker.ErrNoComment) {
			t.Errorf("opening %q on a task that does not have it answered %v, "+
				"which a caller cannot tell from a store it could not reach",
				id, err)
		}
	}
}

// A CAPPED HISTORY FEED SAYS IT WAS CAPPED.
//
// A silent cut would answer a task with five hundred changes and a task with
// fifty identically, so a reader deciding "has anybody touched this" would be
// told the whole story either way and could not tell which it had. That is the
// failure the board reader refuses one file over — an overflow is counted and
// said, never silently cut — and the escape hatch it points at, `task_activity`,
// is only reachable by a caller who knows there is something to reach for.
func TestACappedHistoryFeedSaysSo(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	created := r.createTask("Rate limits in the GitLab client")

	// Four changes beyond the create, so a limit of 2 leaves more behind and
	// a limit past the whole feed does not.
	for _, who := range []string{"ana", "bo", "cy", "di"} {
		if _, err := r.writer.UpdateTask(t.Context(), "op-assign-"+who, created.ID,
			"ENG", tracker.NoIfMatch, tracker.TaskPatch{Assignee: strptr(who)},
			tracker.ChangeAssignee, nil); err != nil {
			t.Fatalf("assign %s: %v", who, err)
		}
		r.drain()
	}

	read := func(limit int) tracker.TaskDetail {
		t.Helper()
		detail, err := r.reader.Task(t.Context(), created.ID, tracker.DetailWants{
			History: true, HistoryLimit: limit,
		}, statelog.Freshness{Level: statelog.ReadSession})
		if err != nil {
			t.Fatalf("read at limit %d: %v", limit, err)
		}
		return detail
	}

	cut := read(2)
	if len(cut.History) != 2 {
		t.Fatalf("a limit of 2 returned %d change(s)", len(cut.History))
	}
	if !cut.HistoryTruncated {
		t.Error("the feed was cut and the answer does not say so, so a reader " +
			"cannot tell this task from one with two changes in its whole life")
	}

	// THE EXTRA ROW IS EVIDENCE, NEVER AN ANSWER: the page stays at the
	// bound. A limit+1 read that forgot to drop the probe row would return
	// three here and the flag would be right for the wrong reason.
	whole := read(50)
	if whole.HistoryTruncated {
		t.Errorf("a feed of %d change(s) under a limit of 50 reports itself cut",
			len(whole.History))
	}
	if len(whole.History) != 5 {
		t.Fatalf("the whole feed is %d change(s) after a create and four assigns",
			len(whole.History))
	}
	// EXACTLY AT THE BOUND IS NOT CUT, which is the off-by-one that would
	// make every full page claim there is more behind it.
	if exact := read(5); exact.HistoryTruncated || len(exact.History) != 5 {
		t.Errorf("a limit equal to the feed returned %d change(s), truncated=%v",
			len(exact.History), exact.HistoryTruncated)
	}
}

// AN AMBIGUOUS-ANSWER REFUSAL NEVER STATES A COUNT IT DID NOT MAKE.
//
// The read takes one row more than it will name so it can tell "five" from "at
// least five" — and the extra row was left on the candidate list and counted,
// so a seat with nine open asks was told "6 open questions are addressed to
// you". That is neither the five the refusal then lists nor the nine that
// exist. A model has no way to check the number and chooses its next move
// against it.
func TestAnAmbiguousAnswerCountsOnlyWhatItCounted(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	created := r.createTask("the rollout")

	ask := func(n int) {
		t.Helper()
		if _, err := r.writer.UpdateTask(t.Context(), fmt.Sprintf("op-ask%d", n),
			created.ID, "ENG", tracker.NoIfMatch,
			tracker.TaskPatch{Comment: &tracker.Comment{
				ID: fmt.Sprintf("cm-%d", n), Task: created.ID, Author: "ana",
				AuthorKind: tracker.AuthorHuman,
				Body:       fmt.Sprintf("question number %d about the rollout", n),
				Ask:        "bob", CreatedAt: wednesday, UpdatedAt: wednesday,
			}}, tracker.ChangeComment, nil); err != nil {
			t.Fatalf("ask %d: %v", n, err)
		}
		r.drain()
	}
	// longAsk is a question whose first AskExcerptShown bytes are shared
	// boilerplate, so only a MARKED excerpt tells it from its neighbours.
	longAsk := func(n int) {
		t.Helper()
		body := strings.Repeat("quick question about the rollout plan. ", 6) +
			"which region do we start in?"
		if _, err := r.writer.UpdateTask(t.Context(), fmt.Sprintf("op-ask%d", n),
			created.ID, "ENG", tracker.NoIfMatch,
			tracker.TaskPatch{Comment: &tracker.Comment{
				ID: fmt.Sprintf("cm-%d", n), Task: created.ID, Author: "ana",
				AuthorKind: tracker.AuthorHuman, Body: body,
				Ask: "bob", CreatedAt: wednesday, UpdatedAt: wednesday,
			}}, tracker.ChangeComment, nil); err != nil {
			t.Fatalf("long ask %d: %v", n, err)
		}
		r.drain()
	}
	resolve := func() error {
		t.Helper()
		_, err := r.reader.Thread(t.Context(), tracker.ThreadQuery{
			Task: created.ID, Author: "bob",
		}, statelog.Freshness{Level: statelog.ReadStale})
		return err
	}

	// Two asks: ambiguous, and both are named, so the count is EXACT.
	ask(1)
	ask(2)
	// A THIRD WHOSE OPENING IS THE SAME, so an unmarked excerpt that
	// stopped at the boilerplate would render it identically to one of
	// them — which is the whole reason a candidate list exists.
	longAsk(3)
	var ambiguous *tracker.ErrAmbiguousAnswer
	if err := resolve(); !errors.As(err, &ambiguous) {
		t.Fatalf("a reply with two open asks gave %v, want an ambiguous refusal", err)
	}
	if ambiguous.More || len(ambiguous.Asks) != 3 {
		t.Fatalf("three asks reported as more=%v over %d candidates",
			ambiguous.More, len(ambiguous.Asks))
	}
	if !strings.Contains(ambiguous.Error(), "3 open questions") {
		t.Errorf("the refusal reads %q", ambiguous.Error())
	}
	// THE EXCERPT IS MARKED WHERE IT CUT. Unmarked it reads as a question
	// that really ended there, and two sharing an opening clause render
	// identically — the reader picks one and `askAuthor` stamps it
	// answered on somebody else's behalf.
	var cut string
	for _, candidate := range ambiguous.Asks {
		if candidate.Comment == "cm-3" {
			cut = candidate.Excerpt
		}
	}
	if cut == "" {
		t.Fatal("the long ask is not among the candidates")
	}
	if !strings.HasSuffix(cut, "…") {
		t.Errorf("the excerpt %q stops without saying it was cut", cut)
	}
	if len(cut) > tracker.AskExcerptShown {
		t.Errorf("the excerpt is %d bytes, past the %d it declares",
			len(cut), tracker.AskExcerptShown)
	}

	// NOW PAST THE BOUND. The candidate list stops at MaxOpenAsksNamed and
	// the count says "at least" — never the probe row's number.
	for n := 4; n <= tracker.MaxOpenAsksNamed+4; n++ {
		ask(n)
	}
	if err := resolve(); !errors.As(err, &ambiguous) {
		t.Fatalf("a reply past the bound gave %v, want an ambiguous refusal", err)
	}
	if !ambiguous.More {
		t.Error("nine open asks are not reported as more than were named")
	}
	// THE PROBE ROW IS DROPPED: it is evidence, never a candidate.
	if len(ambiguous.Asks) != tracker.MaxOpenAsksNamed {
		t.Errorf("the refusal carries %d candidates, want the bound of %d",
			len(ambiguous.Asks), tracker.MaxOpenAsksNamed)
	}
	text := ambiguous.Error()
	if !strings.Contains(text, "at least 5 open questions") {
		t.Errorf("the refusal reads %q, want an \"at least\" count", text)
	}
	if strings.Contains(text, "6 open questions") {
		t.Errorf("the refusal states the probe row as a total: %q", text)
	}
}

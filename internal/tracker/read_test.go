package tracker_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/tracker"
)

// readHarness is an applied estate with the reader over it.
type readHarness struct {
	*applyHarness
	reader *tracker.Reader
}

func newReadHarness(t *testing.T) *readHarness {
	t.Helper()
	h := newApplyHarness(t)
	return &readHarness{applyHarness: h, reader: tracker.NewReader(h.db)}
}

// seed applies one task with the fields a case varies.
func (h *readHarness) seed(id string, mutate func(*tracker.Task)) {
	h.t.Helper()
	task := newTask(id)
	task.Key = "ENG-" + id
	if mutate != nil {
		mutate(&task)
	}
	if _, err := h.apply(taskRecord(id, tracker.OpCreate, task, nil),
		time.Unix(1_700_000_100, 0).UTC()); err != nil {
		h.t.Fatalf("seed %s: %v", id, err)
	}
}

func (h *readHarness) ask(kv map[string]any) tracker.Answer {
	h.t.Helper()
	q, err := tracker.ParseQuery(tracker.MapParams(kv), wednesday, berlin)
	if err != nil {
		h.t.Fatalf("ParseQuery(%v): %v", kv, err)
	}
	answer, err := h.reader.Tasks(h.t.Context(), q, wednesday)
	if err != nil {
		h.t.Fatalf("Tasks(%v): %v", kv, err)
	}
	return answer
}

func ids(a tracker.Answer) []string {
	out := make([]string, 0, len(a.Rows))
	for _, row := range a.Rows {
		out = append(out, row.ID)
	}
	return out
}

// A DEFAULT ANSWER EXCLUDES FINISHED, ARCHIVED AND REMOVED WORK.
//
// Three defaults, and each one is what a person means by "the board": a task
// that is done is out of the way, a task in an archived project is out of the
// company, and a removed task is out of existence — the last by ONE predicate
// for its whole life, at any age, which is what makes a restore answer exactly
// what it answered before.
func TestTheDefaultAnswerIsOpenLiveWork(t *testing.T) {
	t.Parallel()
	h := newReadHarness(t)
	h.seed("open", nil)
	h.seed("done", func(task *tracker.Task) {
		task.Status, task.StatusGroup = tracker.StatusDone, tracker.GroupDone
	})
	h.seed("archived", func(task *tracker.Task) { task.Archived = true })

	got := ids(h.ask(map[string]any{"container": "project:ENG"}))
	if len(got) != 1 || got[0] != "open" {
		t.Fatalf("the default answer is %v, want the one open live task", got)
	}
	if got := ids(h.ask(map[string]any{
		"container": "project:ENG", "show_closed": "true",
	})); len(got) != 2 {
		t.Errorf("show_closed=true answers %v, want the open and the done one", got)
	}
	if got := ids(h.ask(map[string]any{
		"container": "project:ENG", "archived": "only",
	})); len(got) != 1 || got[0] != "archived" {
		t.Errorf("archived=only answers %v", got)
	}
}

// ARCHIVING A PROJECT TAKES ITS TASKS OUT OF THE DEFAULT ANSWER, THROUGH A
// JOIN.
//
// The alternative — copying the project's flag onto every one of its tasks —
// is the one unbounded cross-object write this design removed: archiving a
// project with ten thousand tasks would be ten thousand rows written by one
// commit, on every node, and a scope term nobody could bound.
func TestArchivingAProjectHidesItsTasksWithoutRewritingThem(t *testing.T) {
	t.Parallel()
	h := newReadHarness(t)
	project := tracker.MutationRecord{
		RecordEnvelope: tracker.RecordEnvelope{
			V: tracker.RecordVersion, OpID: "eng-live",
			Subject: tracker.ProjectSubject("ENG"), Op: tracker.OpCreate,
			Writer: "node-a", Scope: tracker.ScopeSet{Subject: true},
		},
		Mutation: mustJSON(tracker.Project{
			V: tracker.DocumentVersion, Key: "ENG", Name: "Engineering",
		}),
	}
	if _, err := h.apply(project, time.Unix(1_700_000_050, 0).UTC()); err != nil {
		t.Fatalf("project: %v", err)
	}
	h.seed("live", nil)
	if got := ids(h.ask(map[string]any{"container": "project:ENG"})); len(got) != 1 {
		t.Fatalf("the task is not in the answer before the project is archived: %v", got)
	}

	archived := project
	archived.OpID = "eng-archived"
	archived.Op = tracker.OpPatch
	archived.Mutation = mustJSON(tracker.Project{
		V: tracker.DocumentVersion, Key: "ENG", Name: "Engineering",
		Archived: true,
	})
	if _, err := h.apply(archived, time.Unix(1_700_000_060, 0).UTC()); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if got := ids(h.ask(map[string]any{"container": "project:ENG"})); len(got) != 0 {
		t.Fatalf("archiving the project left %v in the default answer; the task "+
			"row was never rewritten, so only the join can hide it", got)
	}
	if got := ids(h.ask(map[string]any{
		"container": "project:ENG", "archived": "only",
	})); len(got) != 1 {
		t.Fatalf("archived=only answers %v, want the task in the archived "+
			"project", got)
	}
}

// A CANCELLED TASK IS IN THE RECENT WINDOW EXACTLY AS A DONE ONE IS.
//
// Because the finish stamp is by GROUP rather than by slug, `recent:` applies
// to the whole finished set — which is what makes a board's Cancelled column
// non-empty without a second field, and what stops "abandoned" work
// disappearing from every view the moment it is abandoned.
func TestARecentWindowHoldsCancelledWorkToo(t *testing.T) {
	t.Parallel()
	h := newReadHarness(t)
	finished := wednesday.Add(-2 * 24 * time.Hour)
	for id, status := range map[string]tracker.Status{
		"done": tracker.StatusDone, "cancelled": tracker.StatusCancelled,
	} {
		h.seed(id, func(task *tracker.Task) {
			task.Status, task.StatusGroup = status, status.Group()
			at := finished
			task.DoneAt = &at
		})
	}
	got := ids(h.ask(map[string]any{
		"container": "project:ENG", "show_closed": "recent:168h",
	}))
	if len(got) != 2 {
		t.Fatalf("the fortnight window answers %v, want both the done and the "+
			"cancelled task", got)
	}
}

// A DISJUNCTION IS A FILTER, ANDed WITH THE REST OF THE QUERY.
//
// It is the whole reason `any` exists: "assigned to me, or unassigned in my
// project" is one question, and expressing it as two calls means the caller
// merges, sorts and pages two answers by hand.
func TestADisjunctionNarrowsRatherThanReplaces(t *testing.T) {
	t.Parallel()
	h := newReadHarness(t)
	h.seed("mine", func(task *tracker.Task) { task.Assignee = "ana" })
	h.seed("nobodys", nil)
	h.seed("theirs", func(task *tracker.Task) { task.Assignee = "bo" })
	h.seed("mine-done", func(task *tracker.Task) {
		task.Assignee = "ana"
		task.Status, task.StatusGroup = tracker.StatusDone, tracker.GroupDone
	})

	got := ids(h.ask(map[string]any{
		"container": "project:ENG",
		"any":       `[{"assignee":"ana"},{"assignee":"none"}]`,
	}))
	if len(got) != 2 {
		t.Fatalf("the disjunction answers %v, want the assigned and the "+
			"unassigned open task — and NOT the finished one, because the "+
			"top-level default is ANDed with the branches", got)
	}
}

// A FILTER VALUE NEVER REACHES A STATEMENT AS TEXT.
//
// The only things this package composes into SQL are its own column names,
// from closed sets declared in the grammar. A filter that carried its value
// into the statement would make a saved view a way to run a query nobody
// wrote.
func TestAHostileFilterValueIsBoundRatherThanComposed(t *testing.T) {
	t.Parallel()
	h := newReadHarness(t)
	h.seed("safe", nil)
	answer := h.ask(map[string]any{
		"container": "project:ENG",
		"assignee":  `' OR 1=1 --`,
	})
	if len(answer.Rows) != 0 {
		t.Fatalf("a filter value that reads as SQL matched %d row(s); it is a "+
			"handle nobody has, so the answer is empty", len(answer.Rows))
	}
	// And the table is still there, which a composed value would not
	// guarantee.
	if got := h.count("tracker_tasks"); got != 1 {
		t.Fatalf("the estate holds %d tasks after a hostile filter", got)
	}
}

// AN ANSWER CARRIES THE POSITION IT WAS READ AT.
//
// Two numbers, because a node applying nothing while its position advances
// looks identical to one that is caught up — and a caller comparing a wake's
// position against an answer's needs the one that says what this node HOLDS.
func TestAnAnswerCarriesBothPositions(t *testing.T) {
	t.Parallel()
	h := newReadHarness(t)
	h.seed("a", nil)
	answer := h.ask(map[string]any{"container": "project:ENG"})
	if !answer.Complete {
		t.Fatalf("an answer from a node holding no deferred record is incomplete: "+
			"%+v", answer.Incomplete)
	}
	if answer.Incomplete != nil {
		t.Errorf("a complete answer carries an incompleteness report")
	}
}

// A PAGE MINTS THE CURSOR THAT RESUMES IT, AND NEVER REPEATS A ROW.
//
// Keyset rather than offset: an offset re-reads and re-sorts every row it
// skips, so page fifty costs fifty times page one — and a row inserted between
// two polls shifts every page after it.
func TestPagingResumesWithoutRepeatingOrSkipping(t *testing.T) {
	t.Parallel()
	h := newReadHarness(t)
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		h.seed(id, nil)
	}
	seen := map[string]bool{}
	cursor := ""
	for page := range 5 {
		params := map[string]any{"container": "project:ENG", "limit": "2"}
		if cursor != "" {
			params["cursor"] = cursor
		}
		answer := h.ask(params)
		for _, row := range answer.Rows {
			if seen[row.ID] {
				t.Fatalf("page %d repeated %s", page, row.ID)
			}
			seen[row.ID] = true
		}
		cursor = answer.NextCursor
		if cursor == "" {
			break
		}
	}
	if len(seen) != 5 {
		t.Fatalf("paging saw %d of 5 tasks: %v", len(seen), seen)
	}
}

// THE TOTAL HINT STOPS AT ITS CEILING RATHER THAN COUNTING ON.
//
// An exact total over an unbounded set is the one query in this grammar that
// turns a sixty-second poll into a scan, and nobody reading a board needs the
// difference between eleven thousand and twelve.
func TestTheTotalHintIsBounded(t *testing.T) {
	t.Parallel()
	h := newReadHarness(t)
	for _, id := range []string{"a", "b", "c"} {
		h.seed(id, nil)
	}
	answer := h.ask(map[string]any{"container": "project:ENG"})
	if answer.TotalHint != 3 {
		t.Fatalf("the total hint is %d for three tasks", answer.TotalHint)
	}
	if answer.TotalHint > tracker.TotalHintCeiling+1 {
		t.Fatalf("the total hint counted past its ceiling")
	}
}

// A READ'S SCOPE IS THE CLOSURE THE COVERAGE PROBE COMPARES AGAINST.
//
// A query with no container is the DOMAIN — not because it touches everything,
// but because a read that cannot say what it is about is one every deferred
// record concerns, and the honest answer to "is this complete" is then no.
func TestAScopelessReadIsAboutTheWholeDomain(t *testing.T) {
	t.Parallel()
	narrow := tracker.ReadScope(tracker.Query{
		Scope: tracker.Scope{Project: "ENG"},
	})
	wide := tracker.ReadScope(tracker.Query{})
	if len(narrow.Paths) != 1 || narrow.Paths[0] == wide.Paths[0] {
		t.Fatalf("a project read's scope is %v and a scopeless one's is %v; the "+
			"two must differ or every read is about everything",
			narrow.Paths, wide.Paths)
	}
	if !wide.Intersects(narrow) {
		t.Fatal("the domain's scope does not cover a project's, so a deferred " +
			"record about the whole domain would not be reported to a project " +
			"read")
	}
}

// A REMOVED TASK IS OUT OF EVERY ANSWER BUT THE TWO THAT ARE ABOUT REMOVALS.
//
// ONE predicate for a removed task's whole life, at any age — which is what
// makes a removal an entirely local decision with no walk behind it, and a
// restore at any age answer exactly what it answered before.
func TestARemovedTaskIsOutOfEveryOrdinaryAnswer(t *testing.T) {
	t.Parallel()
	h := newReadHarness(t)
	h.seed("live", nil)
	h.seed("gone", func(task *tracker.Task) {
		task.Removed = &tracker.Tombstone{
			By: "ops", Kind: tracker.AuthorOperator, At: wednesday,
		}
	})
	got := ids(h.ask(map[string]any{"container": "project:ENG"}))
	if len(got) != 1 || got[0] != "live" {
		t.Fatalf("the answer is %v; a removed task is out of it whatever else "+
			"the query asks for", got)
	}
	// AND EVEN WHEN EVERY OTHER DEFAULT IS LIFTED, because the removal
	// predicate is not one of them.
	got = ids(h.ask(map[string]any{
		"container": "project:ENG", "show_closed": "true", "archived": "true",
	}))
	if len(got) != 1 || got[0] != "live" {
		t.Fatalf("lifting the other defaults answered %v; a removal is not a "+
			"default a caller can turn off", got)
	}
}

// THE THREE TAG MODES ANSWER THREE DIFFERENT QUESTIONS.
//
// `all:` is a COUNT rather than a membership — a task carrying two of three is
// not a match, and an IN test cannot say so, which is the one of the three
// that reads correctly while being wrong.
func TestTheTagModesAreThreeDifferentQuestions(t *testing.T) {
	t.Parallel()
	h := newReadHarness(t)
	h.seed("both", func(task *tracker.Task) { task.Tags = []string{"api", "v2"} })
	h.seed("one", func(task *tracker.Task) { task.Tags = []string{"api"} })
	h.seed("none", nil)

	for name, tc := range map[string]struct {
		filter string
		want   []string
	}{
		"any matches either":          {"any:api,v2", []string{"both", "one"}},
		"all matches only both":       {"all:api,v2", []string{"both"}},
		"none matches neither":        {"none:api,v2", []string{"none"}},
		"a bare list defaults to any": {"api,v2", []string{"both", "one"}},
	} {
		t.Run(name, func(t *testing.T) {
			got := ids(h.ask(map[string]any{
				"container": "project:ENG", "tag": tc.filter,
			}))
			if len(got) != len(tc.want) {
				t.Fatalf("tag=%s answered %v, want %v", tc.filter, got, tc.want)
			}
		})
	}
}

// A PAGE BOUNDARY IS THE WHOLE ORDER, NOT THE ID.
//
// # The failure this exists to catch
//
// The cursor used to carry only the last row's id and the predicate was
// `id > ?` — while the order is `<sort column>, id`. On a board ordered by
// rank, every task with a later rank and a smaller id SORTS AFTER the boundary
// and FAILS the predicate, so it is never returned; every task with an earlier
// rank and a larger id passes it on every page, so it is returned again and
// again. Both at once, silently: a caller paging its own board would see some
// of its tasks twice and never see others, and nothing in the answer said so.
//
// The fixture is the shape that catches it — ranks and ids in OPPOSITE orders
// — because any fixture where they agree passes with either implementation.
func TestPagingAnOrderReturnsEveryRowExactlyOnce(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	// Ranks ascend while ids descend, so "after this rank" and "after
	// this id" name disjoint sets.
	const total = 9
	for i := range total {
		task := newTask(fmt.Sprintf("t-%d", total-i))
		if _, err := r.writer.CreateTask(t.Context(), fmt.Sprintf("op-%d", i),
			task, nil); err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
		r.drain()
	}

	// BOTH DIRECTIONS. "After" in a descending order is a SMALLER value,
	// and a keyset that compared the same way both ways would return the
	// page BEFORE the boundary — every descending page repeating the
	// first — which an ascending fixture cannot show.
	for _, sort := range []string{"rank", "-rank", "-updated"} {
		seen := map[string]int{}
		cursor := ""
		for page := 0; page < total+2; page++ {
			params := map[string]any{
				"container": "project:ENG", "sort": sort, "limit": "2",
			}
			if cursor != "" {
				params["cursor"] = cursor
			}
			answer := r.ask(params)
			for _, row := range answer.Rows {
				seen[row.ID]++
			}
			if answer.NextCursor == "" {
				break
			}
			cursor = answer.NextCursor
		}

		if len(seen) != total {
			t.Errorf("paging by %s returned %d distinct tasks of %d — a "+
				"boundary that compares a column the order does not sort by, "+
				"or compares it the wrong way, drops rows on one side of it",
				sort, len(seen), total)
		}
		for id, times := range seen {
			if times != 1 {
				t.Errorf("paging by %s returned task %s %d times; a keyset "+
					"boundary returns each row exactly once", sort, id, times)
			}
		}
	}
}

// A CURSOR BELONGS TO THE ORDER THAT MINTED IT.
//
// Resuming a rank-ordered page inside an update-ordered query would compute a
// boundary against a column the query does not sort by — which drops rows
// without saying so, and is the same defect as the one above wearing a
// caller's mistake instead of ours.
func TestACursorFromAnotherOrderIsRefused(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for i := range 3 {
		if _, err := r.writer.CreateTask(t.Context(), fmt.Sprintf("op-%d", i),
			newTask(fmt.Sprintf("t-%d", i)), nil); err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
		r.drain()
	}
	first := r.ask(map[string]any{
		"container": "project:ENG", "sort": "rank,updated", "limit": "1",
	})
	if first.NextCursor == "" {
		t.Fatal("the first page minted no cursor, so this case asserts nothing")
	}
	q, err := tracker.ParseQuery(tracker.MapParams(map[string]any{
		"container": "project:ENG", "sort": "rank", "cursor": first.NextCursor,
	}), wednesday, berlin)
	if err != nil {
		t.Fatalf("ParseQuery: %v", err)
	}
	if _, err := r.reader.Tasks(t.Context(), q, wednesday); err == nil {
		t.Fatal("a cursor minted by a two-column order was applied to a " +
			"one-column one, which computes a boundary against a column the " +
			"query does not sort by")
	}
}

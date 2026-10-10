package tracker_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

func (r *roundTrip) companyFeed(q tracker.FeedQuery) tracker.FeedPage {
	r.t.Helper()
	if q.Level == "" {
		q.Level = statelog.ReadStale
	}
	if q.Limit == 0 {
		q.Limit = tracker.MaxFeedPage
	}
	page, err := r.reader.CompanyFeed(r.t.Context(), q)
	if err != nil {
		r.t.Fatalf("CompanyFeed: %v", err)
	}
	return page
}

func feedKinds(page tracker.FeedPage) []string {
	out := make([]string, 0, len(page.Rows))
	for _, row := range page.Rows {
		out = append(out, string(row.Kind)+":"+row.Task)
	}
	return out
}

// ONE ROW PER COMMIT, UNDER ONE KIND, NEWEST FIRST.
//
// A create, a hand-off and a delivery, each at its own instant; a comment and
// a title edit, which the feed does not carry. A create that is also a
// delivery is filed as the create, and asking for completions alone finds it.
func TestTheFeedFilesEachCommitUnderOneKindNewestFirst(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	clock := &flowClock{r: r}
	base := time.Date(2031, 4, 14, 9, 0, 0, 0, time.UTC)
	assign(t, r, "a", "ana")
	clock.at(base)
	handed := "bo"
	if _, err := r.writer.UpdateTask(t.Context(), "op-hand", "a", "ENG", tracker.NoIfMatch,
		tracker.TaskPatch{Assignee: &handed}, tracker.ChangeAssignee, nil); err != nil {
		t.Fatalf("hand a on: %v", err)
	}
	clock.at(base.Add(time.Hour))
	title := "renamed"
	if _, err := r.writer.UpdateTask(t.Context(), "op-title", "a", "ENG", tracker.NoIfMatch,
		tracker.TaskPatch{Title: &title}, tracker.ChangeFields, nil); err != nil {
		t.Fatalf("rename a: %v", err)
	}
	clock.at(base.Add(2 * time.Hour))
	r.setStatus("a", tracker.StatusDone)
	clock.at(base.Add(3 * time.Hour))
	done := newTask("d")
	done.Status = tracker.StatusDone
	if _, err := r.writer.CreateTask(t.Context(), "op-d", done, nil); err != nil {
		t.Fatalf("file d done: %v", err)
	}
	clock.at(base.Add(4 * time.Hour))

	got := fmt.Sprint(feedKinds(r.companyFeed(tracker.FeedQuery{})))
	want := fmt.Sprint([]string{"created:d", "completed:a", "handoff:a", "created:a"})
	if got != want {
		t.Errorf("the feed is %s, want %s", got, want)
	}
	got = fmt.Sprint(feedKinds(r.companyFeed(tracker.FeedQuery{
		Kinds: []tracker.FeedKind{tracker.FeedCompleted}})))
	if want = fmt.Sprint([]string{"completed:d", "completed:a"}); got != want {
		t.Errorf("completions alone are %s, want %s — a create that delivered "+
			"is still a completion to a reader asking only for those", got, want)
	}
	page := r.companyFeed(tracker.FeedQuery{Kinds: []tracker.FeedKind{tracker.FeedHandoff}})
	if len(page.Rows) != 1 {
		t.Fatalf("hand-offs are %v, want one", feedKinds(page))
	}
	if row := page.Rows[0]; row.From != "ana" || row.To != "bo" ||
		row.Reassignments == nil || row.ReassignmentBudget != tracker.ReassignmentBudget {
		t.Errorf("the hand-off is %+v, want ana to bo with its counter and budget", row)
	}
}

// A SCROLL NEITHER REPEATS NOR SKIPS, including across rows at one instant.
func TestTheFeedCursorContinuesWithoutRepeatOrGap(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	clock := &flowClock{r: r}
	base := time.Date(2031, 4, 14, 9, 0, 0, 0, time.UTC)
	for i := range 7 {
		r.file(fmt.Sprintf("t%d", i))
		// TWO ROWS PER INSTANT, so the position has to break the tie.
		clock.at(base.Add(time.Duration(i/2) * time.Minute))
	}
	seen := map[string]bool{}
	var before *tracker.FeedCursor
	pages := 0
	for {
		page := r.companyFeed(tracker.FeedQuery{Limit: 3, Before: before})
		pages++
		for _, row := range page.Rows {
			if seen[row.Task] {
				t.Errorf("page %d repeats %s", pages, row.Task)
			}
			seen[row.Task] = true
		}
		if !page.More {
			break
		}
		cursor, err := tracker.ParseFeedCursor(page.Rows[len(page.Rows)-1].Cursor)
		if err != nil {
			t.Fatalf("the cursor does not parse: %v", err)
		}
		before = &cursor
		if pages > 5 {
			t.Fatal("the scroll does not end")
		}
	}
	if len(seen) != 7 || pages != 3 {
		t.Errorf("three pages of three saw %d tasks in %d pages, want 7 in 3", len(seen), pages)
	}
}

// A TASK FILED FROM A CHAT THREAD SAYS WHERE; one filed from nowhere says
// nothing.
func TestACreateFromAChatThreadSaysWhere(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seat := r.writer.As("pm", tracker.AuthorAgent, tracker.Provenance{
		Origin: &tracker.Origin{Surface: "slack", Conversation: "slack:C1:171.2"},
	})
	task := newTask("from-slack")
	if _, err := seat.CreateTask(t.Context(), "op-slack", task, nil); err != nil {
		t.Fatalf("file from slack: %v", err)
	}
	r.drain()
	r.file("from-nowhere")

	origins := map[string]*tracker.Origin{}
	for _, row := range r.companyFeed(tracker.FeedQuery{}).Rows {
		origins[row.Task] = row.Origin
	}
	if got := origins["from-slack"]; got == nil || got.Surface != "slack" ||
		got.Conversation != "slack:C1:171.2" {
		t.Errorf("the Slack-woken create's origin is %+v", got)
	}
	if got := origins["from-nowhere"]; got != nil {
		t.Errorf("a create nothing on a chat surface caused carries origin %+v", got)
	}
}

// A PERSON'S FEED IS THE COMPANY'S ROWS ABOUT THE TASKS THEY ARE IN: one they
// filed (under either of their names), own, were handed or handed on, asked or
// were asked something on, or watch — and not one they muted, nor one they
// have nothing to do with. Each task below is in it through exactly one arm,
// so a dropped arm is a missing task rather than a passing case.
func TestAPersonsFeedIsTheTasksTheyAreIn(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	ana := tracker.Party{Handle: "ana", OperatorID: "ops-1"}
	create := func(id string, edit func(*tracker.Task)) {
		t.Helper()
		task := newTask(id)
		task.Key = "ENG-" + id
		edit(&task)
		if _, err := r.writer.CreateTask(t.Context(), "op-"+id, task, nil); err != nil {
			t.Fatalf("file %s: %v", id, err)
		}
		r.drain()
	}
	watch := func(id, handle string, on bool) {
		t.Helper()
		if _, err := r.writer.UpdateTask(t.Context(), fmt.Sprintf("op-watch-%s-%v", id, on), id,
			"ENG", tracker.NoIfMatch, tracker.TaskPatch{Watch: &tracker.WatchIntent{
				Handle: handle, Watch: on}}, tracker.ChangeWatchers, nil); err != nil {
			t.Fatalf("watch %s: %v", id, err)
		}
		r.drain()
	}
	create("filed", func(task *tracker.Task) { task.Reporter = "ops-1"; task.Assignee = "bo" })
	create("owned", func(task *tracker.Task) { task.Assignee = "ana" })
	// A CHANGE THAT MOVES NO ASSIGNEE, on a task she owns: hers by ownership alone.
	r.setStatus("owned", tracker.StatusDone)
	create("handed", func(task *tracker.Task) { task.Assignee = "bo" })
	handed := "ana"
	if _, err := r.writer.UpdateTask(t.Context(), "op-hand-in", "handed", "ENG", tracker.NoIfMatch,
		tracker.TaskPatch{Assignee: &handed}, tracker.ChangeAssignee, nil); err != nil {
		t.Fatalf("hand handed to ana: %v", err)
	}
	r.drain()
	onward := "cy"
	if _, err := r.writer.UpdateTask(t.Context(), "op-hand-on", "handed", "ENG", tracker.NoIfMatch,
		tracker.TaskPatch{Assignee: &onward}, tracker.ChangeAssignee, nil); err != nil {
		t.Fatalf("hand handed on to cy: %v", err)
	}
	r.drain()
	// ASKED, then muted: the ask is what keeps her in it, not the watch.
	create("asked", func(task *tracker.Task) { task.Assignee = "bo" })
	askOn(t, r, "op-asked", "asked", tracker.Comment{ID: "c-asked", Task: "asked", Author: "bo",
		AuthorKind: tracker.AuthorAgent, Body: "which region?", Ask: "ana", CreatedAt: wednesday})
	watch("asked", "ana", false)
	// SHE ASKED, then muted.
	create("asker", func(task *tracker.Task) { task.Assignee = "bo" })
	askOn(t, r, "op-asker", "asker", tracker.Comment{ID: "c-asker", Task: "asker", Author: "ana",
		AuthorKind: tracker.AuthorHuman, Body: "ship it?", Ask: "bo", CreatedAt: wednesday})
	watch("asker", "ana", false)
	create("watched", func(task *tracker.Task) { task.Assignee = "bo" })
	watch("watched", "ana", true)
	create("muted", func(task *tracker.Task) { task.Assignee = "bo" })
	watch("muted", "ana", true)
	watch("muted", "ana", false)
	// ON THE WATCHERS AND MUTED TOO — watching, but not for her — which is the
	// row the tracker keeps as `muted = 1` rather than as no row at all.
	create("silenced", func(task *tracker.Task) {
		task.Assignee = "bo"
		task.Watchers, task.Muted = []string{"ana"}, []string{"ana"}
	})
	create("theirs", func(task *tracker.Task) { task.Assignee = "bo" })

	got := map[string][]string{}
	for _, row := range r.companyFeed(tracker.FeedQuery{Involving: ana}).Rows {
		got[row.Task] = append(got[row.Task], string(row.Kind))
	}
	want := map[string][]string{
		"filed": {"created"},
		"owned": {"completed", "created"},
		// THE HAND-OFFS TO HER AND ON FROM HER, and not the create: it was
		// filed to bo, and she neither owns nor watches the task now.
		"handed":  {"handoff", "handoff"},
		"asked":   {"created"},
		"asker":   {"created"},
		"watched": {"created"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("ana's feed is %v, want %v", got, want)
	}
	// THE WHOLE COMPANY WITHOUT IT — the control, so a predicate that matched
	// nothing would not pass as one that matched only ana's.
	all := map[string]bool{}
	for _, row := range r.companyFeed(tracker.FeedQuery{}).Rows {
		all[row.Task] = true
	}
	if !all["muted"] || !all["silenced"] || !all["theirs"] {
		t.Errorf("the company's feed is %v, and it lacks the tasks ana is not in", all)
	}
}

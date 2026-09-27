package tracker_test

import (
	"encoding/json"
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
// nothing, and a create written before the field decodes the same way.
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

// A CREATE CARRYING AN ORIGIN IS STAMPED AT THE VERSION THAT READS IT, and one
// without stays below it — so a node that cannot read the field holds back
// only the records that carry it.
func TestAnOriginIsStampedAtTheVersionThatReadsIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		origin *tracker.Origin
		want   func(int) bool
	}{
		{&tracker.Origin{Surface: "slack"}, func(v int) bool { return v == 9 }},
		{nil, func(v int) bool { return v < 9 }},
	} {
		body, err := json.Marshal(tracker.TaskCreate{Task: newTask("t"), Origin: tc.origin})
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := tracker.MutationRecord{
			RecordEnvelope: tracker.RecordEnvelope{
				OpID: "op", Subject: tracker.TaskSubject("t"), Op: tracker.OpCreate,
				CreatedAt: wednesday, Writer: "n",
				Scope: tracker.ScopeSet{Subject: true, Container: "ENG"},
			},
			Kind: tracker.ChangeCreated, Mutation: body,
			Actor: "pm", ActorKind: tracker.AuthorAgent,
		}.Encode()
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		env, err := tracker.DecodeEnvelope(encoded)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !tc.want(env.V) {
			t.Errorf("a create with origin %+v is stamped at version %d", tc.origin, env.V)
		}
	}
}

package tracker_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

func (r *roundTrip) myWork(handle string) tracker.MyWork {
	r.t.Helper()
	out, err := r.reader.MyWork(r.t.Context(), tracker.MyWorkQuery{
		Handle: handle, Level: statelog.ReadStale,
	}, wednesday)
	if err != nil {
		r.t.Fatalf("MyWork(%s): %v", handle, err)
	}
	return out
}

// askOn writes one comment onto a task, which is how a comment is written at
// all: a comment is a MUTATION of its task, arbitrated by the task's own
// version, so it rides the task's write rather than having a verb of its own.
func askOn(t *testing.T, r *roundTrip, opID, task string, comment tracker.Comment) {
	t.Helper()
	if _, err := r.writer.UpdateTask(t.Context(), opID, task, "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Comment: &comment}, tracker.ChangeComment, nil); err != nil {
		t.Fatalf("comment on %s: %v", task, err)
	}
	r.drain()
}

// assign files a task held by one handle.
func assign(t *testing.T, r *roundTrip, id, handle string) {
	t.Helper()
	task := newTask(id)
	task.Assignee = handle
	if _, err := r.writer.CreateTask(t.Context(), "op-"+id, task, nil); err != nil {
		t.Fatalf("CreateTask %s: %v", id, err)
	}
	r.drain()
}

// PRIORITIES COME BACK IN THEIR STORED ORDER, because the order IS the
// content: it is what somebody decided, and re-sorting it by anything at all
// discards the decision.
func TestMyWorkKeepsThePriorityOrder(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for _, id := range []string{"a", "b", "c"} {
		assign(t, r, id, "ana")
	}
	// DELIBERATELY NOT ALPHABETICAL and not creation order, so an answer
	// sorted by either is visibly wrong.
	if _, err := r.writer.WritePriorities(t.Context(), "op-prio", "ana",
		[]string{"c", "a", "b"}, tracker.PersonAuthority{}); err != nil {
		t.Fatalf("WritePriorities: %v", err)
	}
	r.drain()

	got := r.myWork("ana")
	var order []string
	for _, row := range got.Priorities {
		order = append(order, row.ID)
	}
	if len(order) != 3 || order[0] != "c" || order[1] != "a" || order[2] != "b" {
		t.Fatalf("priorities came back as %v, want [c a b] — the stored order "+
			"is what somebody decided, and sorting it discards the decision",
			order)
	}

	// A FINISHED TASK DROPS OUT OF THE LIST WITHOUT THE LIST BEING
	// REWRITTEN. A read must not write to somebody's own object, so it
	// filters — and the next write to the list drops it for good.
	done := tracker.StatusDone
	if _, err := r.writer.UpdateTask(t.Context(), "op-done", "a", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Status: &done}, tracker.ChangeStatus, nil); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	r.drain()
	after := r.myWork("ana")
	if len(after.Priorities) != 2 || after.Priorities[0].ID != "c" ||
		after.Priorities[1].ID != "b" {
		t.Fatalf("after finishing one, priorities are %+v, want c then b",
			after.Priorities)
	}
	if held := r.person("ana"); len(held.Priorities) != 3 {
		t.Errorf("the stored list is now %v — a READ rewrote somebody's own "+
			"object", held.Priorities)
	}
}

// A PRIORITY LIST IS FILTERED BEFORE IT IS CUT, AND CUT IN ITS OWN ORDER.
//
// Two bugs in one read, and both made the highest-signal block in the answer
// silently wrong. MaxPriorities is 32 and MyWorkRows is 20, so a list longer
// than twenty is ordinary.
//
//   - The cut ran BEFORE the open/removed filter, so a list whose first twenty
//     entries were all finished rendered EMPTY while live work sat at 21-32.
//     "You have nothing prioritised" is the one thing this block must not say
//     falsely.
//   - The SQL LIMIT was twenty over rows ordered by `t.id`, and the stored
//     order is re-applied in Go afterwards — so it kept the twenty lowest ids
//     and threw away whatever the person had actually put first.
func TestAPriorityListIsFilteredBeforeItIsCut(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	// Thirty tasks, ids chosen so the stored order is the REVERSE of id
	// order: the id-limited read would keep p00..p19 and the honest one
	// keeps p29..p10.
	ids := make([]string, 0, 30)
	for i := range 30 {
		id := fmt.Sprintf("p%02d", i)
		assign(t, r, id, "ana")
		ids = append(ids, id)
	}
	stored := make([]string, len(ids))
	for i, id := range ids {
		stored[len(ids)-1-i] = id
	}
	if _, err := r.writer.WritePriorities(t.Context(), "op-prio", "ana",
		stored, tracker.PersonAuthority{}); err != nil {
		t.Fatalf("WritePriorities: %v", err)
	}
	r.drain()

	got := r.myWork("ana")
	if len(got.Priorities) != tracker.MyWorkRows {
		t.Fatalf("the block holds %d rows, want %d", len(got.Priorities),
			tracker.MyWorkRows)
	}
	// THE TOP OF THE PERSON'S OWN ORDER, not the top of an id order.
	for i := range tracker.MyWorkRows {
		if want := stored[i]; got.Priorities[i].ID != want {
			t.Fatalf("row %d is %q, want %q — the cut is taking the lowest ids "+
				"rather than the front of somebody's own list",
				i, got.Priorities[i].ID, want)
		}
	}

	// NOW FINISH EVERY ONE OF THE FIRST TWENTY. What is left is live work
	// the person deliberately ranked, and it must still be there.
	done := tracker.StatusDone
	for i := range tracker.MyWorkRows {
		if _, err := r.writer.UpdateTask(t.Context(), "op-done-"+stored[i],
			stored[i], "ENG", tracker.NoIfMatch,
			tracker.TaskPatch{Status: &done}, tracker.ChangeStatus, nil); err != nil {
			t.Fatalf("finish %s: %v", stored[i], err)
		}
		r.drain()
	}

	after := r.myWork("ana")
	if len(after.Priorities) == 0 {
		t.Fatal("the block came back EMPTY while ten ranked, open tasks are " +
			"below the cut — the filter is running on the wrong set")
	}
	if after.Priorities[0].ID != stored[tracker.MyWorkRows] {
		t.Errorf("the first live row is %q, want %q",
			after.Priorities[0].ID, stored[tracker.MyWorkRows])
	}
}

// THE SEVEN BLOCKS ARE SEVEN DIFFERENT CLAIMS, and a seat that saw only its
// assignments would miss six of them.
func TestMyWorkSeparatesTheSevenClaims(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	assign(t, r, "mine", "ana")
	assign(t, r, "theirs", "bob")

	// COLLABORATING: brought on without owning. It is a separate block
	// precisely because it is not an assignment.
	if _, err := r.writer.UpdateTask(t.Context(), "op-collab", "theirs", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{
			Collaborators: &[]string{"ana"},
		}, tracker.ChangeCollaborators, nil); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}
	r.drain()

	got := r.myWork("ana")
	if len(got.Assigned) != 1 || got.Assigned[0].ID != "mine" {
		t.Fatalf("assigned is %+v, want the one task ana holds", got.Assigned)
	}
	if len(got.Collaborating) != 1 || got.Collaborating[0].ID != "theirs" {
		t.Fatalf("collaborating is %+v, want the task she was brought onto",
			got.Collaborating)
	}
	// AND THE TWO NEVER OVERLAP: a task already in `assigned` listed again
	// under `collaborating` is one row spending two of the seven blocks.
	for _, row := range got.Collaborating {
		if row.Assignee == "ana" {
			t.Errorf("%s is in collaborating and ana holds it — the block is "+
				"for work she was brought onto WITHOUT owning", row.ID)
		}
	}
	if got.Handle != "ana" {
		t.Errorf("the answer names %q", got.Handle)
	}
}

// AN ASK CARRIES THE CALL THAT ANSWERS IT.
//
// A model handed a comment id still has to compose the answer, and every one
// it composes differently is a round spent being refused.
func TestMyWorkCarriesTheAnswerForm(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	assign(t, r, "asked", "ana")
	askOn(t, r, "op-ask", "asked", tracker.Comment{
		ID: "c-1", Task: "asked", Author: "ana", AuthorKind: tracker.AuthorHuman,
		Body: "which region?", Ask: "ana", CreatedAt: wednesday,
	})

	got := r.myWork("ana")
	if len(got.AskedOfMe) != 1 {
		t.Fatalf("asked_of_me is %+v, want the one open ask", got.AskedOfMe)
	}
	ask := got.AskedOfMe[0]
	if ask.Comment == "" {
		t.Error("the ask names no comment, so nothing can answer it")
	}
	if !strings.Contains(ask.Answer, tracker.CommentOnWorkTool) ||
		!strings.Contains(ask.Answer, ask.Comment) {
		t.Errorf("the answer form is %q and does not carry the tool and the "+
			"comment — a model composing it itself spends a round being "+
			"refused", ask.Answer)
	}
	if ask.Body == "" || ask.AskedBy != "ana" {
		t.Errorf("the ask is %+v, want the question and who asked it", ask)
	}
	if ask.Key == "" {
		t.Error("the ask carries no task key, so it cannot be opened")
	}

	// AND AN ANSWERED ASK LEAVES THE BLOCK, because the block is what is
	// still waiting.
	answers := ask.Comment
	askOn(t, r, "op-answer", "asked", tracker.Comment{
		ID: "c-2", Task: "asked", Author: "ana", AuthorKind: tracker.AuthorHuman,
		Body: "eu-west-1", Answers: &answers, CreatedAt: wednesday,
	})
	if after := r.myWork("ana"); len(after.AskedOfMe) != 0 {
		t.Errorf("asked_of_me still holds %d after the ask was answered",
			len(after.AskedOfMe))
	}
}

// A LONG ASK IS AN OPENING WITH A WAY BACK TO THE WHOLE.
//
// A row is for choosing which ask to answer, so it carries the opening — at
// most [tracker.AskBodyShown] bytes, marked where it was cut, on a whole rune.
// That is only honest if the rest is one read away, and the row's own comment
// id is that read: the detail's `comment` answers the ask exactly as written.
func TestALongAskIsAnOpeningWithAWayBackToTheWhole(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	assign(t, r, "asked", "ana")
	// A two-byte rune ON the boundary, which a byte slice would split.
	body := strings.Repeat("q", tracker.AskBodyShown-2) + "é" +
		strings.Repeat("z", 400)
	askOn(t, r, "op-ask", "asked", tracker.Comment{
		ID: "c-long", Task: "asked", Author: "bob", AuthorKind: tracker.AuthorHuman,
		Body: body, Ask: "ana", CreatedAt: wednesday,
	})

	got := r.myWork("ana")
	if len(got.AskedOfMe) != 1 {
		t.Fatalf("asked_of_me is %+v, want the one open ask", got.AskedOfMe)
	}
	ask := got.AskedOfMe[0]
	switch {
	case len(ask.Body) > tracker.AskBodyShown:
		t.Errorf("the row carries %d bytes of the ask against a bound of %d",
			len(ask.Body), tracker.AskBodyShown)
	case !strings.HasSuffix(ask.Body, "…"):
		t.Errorf("the opening is unmarked — an ask cut at the bound and handed "+
			"over bare reads as a question that ENDED there: %q",
			ask.Body[max(0, len(ask.Body)-8):])
	case !utf8.ValidString(ask.Body):
		t.Error("the opening is not valid UTF-8, so the cut went through a rune")
	}

	whole, err := r.reader.Task(t.Context(), ask.Key,
		tracker.DetailWants{Comment: ask.Comment},
		statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		t.Fatalf("open the ask the row names: %v", err)
	}
	if len(whole.Comments) != 1 || whole.Comments[0].Body != body {
		t.Fatal("the row's key and comment id do not open the whole ask — the " +
			"opening is then a loss rather than a pointer")
	}
}

// MY_WORK NAMES SOMEBODY, always. A day with nobody's name on it is
// everybody's, and a reader that defaulted the viewer would answer about
// whoever it happened to pick.
func TestMyWorkNamesSomebody(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if _, err := r.reader.MyWork(t.Context(), tracker.MyWorkQuery{
		Level: statelog.ReadStale,
	}, wednesday); err == nil {
		t.Error("my_work with no handle answered")
	}
	if _, err := r.reader.MyWork(t.Context(), tracker.MyWorkQuery{
		Handle: "ana",
	}, wednesday); err == nil {
		t.Error("my_work with no read level answered — a level a surface did " +
			"not resolve is a label rather than a guarantee")
	}
}

// A COLLECTION IS EMPTY OR ABSENT — NEVER NULL, and this is the one place in
// the tree where that is a wire contract rather than a preference.
//
// A nil Go slice marshals to `null`. Every client type declares these keys as
// arrays, so a screen reads `mine.priorities.length` — which throws on a null
// and takes the whole page down rather than drawing "nothing here". That is
// precisely what a person with nothing assigned saw: seven blocks of nothing
// is the ORDINARY state of this answer, so the empty case is the case it has
// to be right about.
//
// The three-valued nulls elsewhere in this tree are pointers and maps for
// exactly this reason: a nil SLICE here means one thing and nothing else.
func TestAnEmptyDayCarriesEmptyCollectionsRatherThanNulls(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	// Nobody by this handle holds anything at all, which is the case.
	got := r.myWork("nobody")

	for _, block := range []struct {
		key  string
		rows any
	}{
		{"priorities", got.Priorities},
		{"assigned", got.Assigned},
		{"asked_of_me", got.AskedOfMe},
		{"checklist_items", got.ChecklistItems},
		{"collaborating", got.Collaborating},
		{"watching_recent", got.WatchingRecent},
		{"unblocked_recent", got.UnblockedRecent},
	} {
		if reflect.ValueOf(block.rows).IsNil() {
			t.Errorf("%s is nil, which marshals to null — a client doing "+
				".length on it throws", block.key)
		}
	}

	// The wire is what the claim is about, so assert on the wire.
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), ":null") {
		t.Errorf("an empty day marshals a null: %s", raw)
	}
}

// A CUT BLOCK SAYS SO, on every one of the seven.
//
// Each block is bounded at MyWorkRows and none of them said when it filled, so
// a seat with two hundred assignments rendered exactly like a seat with twenty
// — and "what is on my plate", the one question this answer exists to settle,
// came back as a number the reader could neither check nor doubt. Every other
// bounded read in this package already refuses that: the board counts its
// dropped columns, the history feed flags its cut, routing, workload and the
// project listing all carry a marker.
//
// The evidence was already being computed and thrown away: readTasks fetches
// one row past its limit and mints a cursor when it finds one, and four of the
// seven blocks discarded it.
func TestEveryMyWorkBlockSaysWhenItWasCut(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	// One task more than a block holds, in every claim at once: assigned to
	// ana, collaborated on by her, watched by her, with an open ask to her
	// and a checklist item she holds — and a cleared blocker, so it lands
	// in `unblocked` too.
	over := tracker.MyWorkRows + 1
	for i := range over {
		// HERS: assigned, asked of her, with a checklist item she holds.
		id := fmt.Sprintf("m%02d", i)
		task := newTask(id)
		task.Assignee = "ana"
		task.Checklists = []tracker.Checklist{{ID: "l-1", Name: "steps",
			Items: []tracker.ChecklistItem{{ID: "i-1", Name: "step", Assignee: "ana"}}}}
		if _, err := r.writer.CreateTask(t.Context(), "op-"+id, task, nil); err != nil {
			t.Fatalf("CreateTask %s: %v", id, err)
		}
		r.drain()
		askOn(t, r, "op-ask-"+id, id, tracker.Comment{
			ID: "cm-" + id, Task: id, Author: "bo", AuthorKind: tracker.AuthorHuman,
			Body: "which region?", Ask: "ana", CreatedAt: wednesday, UpdatedAt: wednesday,
		})

		// SOMEBODY ELSE'S, with her on it. Both blocks exclude what this
		// seat OWNS — "brought on without owning" is the distinction they
		// exist for — so they cannot be filled by the tasks above.
		other := fmt.Sprintf("o%02d", i)
		theirs := newTask(other)
		theirs.Assignee = "bo"
		theirs.Collaborators = []string{"ana"}
		theirs.Watchers = []string{"ana"}
		if _, err := r.writer.CreateTask(t.Context(), "op-"+other, theirs, nil); err != nil {
			t.Fatalf("CreateTask %s: %v", other, err)
		}
		r.drain()
	}
	// And a priority list longer than the block, over the same tasks.
	stored := make([]string, 0, tracker.MaxPriorities)
	for i := range min(over, tracker.MaxPriorities) {
		stored = append(stored, fmt.Sprintf("m%02d", i))
	}
	if _, err := r.writer.WritePriorities(t.Context(), "op-prio", "ana",
		stored, tracker.PersonAuthority{}); err != nil {
		t.Fatalf("WritePriorities: %v", err)
	}
	r.drain()

	got := r.myWork("ana")
	for _, block := range []struct {
		name string
		rows int
		cut  bool
	}{
		{"priorities", len(got.Priorities), got.Truncated.Priorities},
		{"assigned", len(got.Assigned), got.Truncated.Assigned},
		{"asked_of_me", len(got.AskedOfMe), got.Truncated.AskedOfMe},
		{"checklist_items", len(got.ChecklistItems), got.Truncated.ChecklistItems},
		{"collaborating", len(got.Collaborating), got.Truncated.Collaborating},
		{"watching_recent", len(got.WatchingRecent), got.Truncated.WatchingRecent},
	} {
		if block.rows != tracker.MyWorkRows {
			t.Errorf("%s holds %d rows, want the bound of %d — the case is not "+
				"exercising this block", block.name, block.rows, tracker.MyWorkRows)
			continue
		}
		if !block.cut {
			t.Errorf("%s is full and does not say it was cut, so it reads as "+
				"a seat with exactly %d", block.name, tracker.MyWorkRows)
		}
	}
	if !got.Truncated.Any() {
		t.Error("Any() says nothing was cut while six blocks were")
	}

	// AND A QUIET DAY CLAIMS NOTHING. A flag that were always on would pass
	// every assertion above and tell an operator their whole company is
	// behind on everything.
	quiet := newRoundTrip(t)
	assign(t, quiet, "one", "ana")
	day := quiet.myWork("ana")
	if day.Truncated.Any() {
		t.Errorf("a one-task day reports blocks cut: %+v", day.Truncated)
	}
}

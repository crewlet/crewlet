package tracker_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

func (r *roundTrip) myWork(handle string) tracker.MyWork {
	r.t.Helper()
	out, err := r.reader.MyWork(r.t.Context(), tracker.MyWorkQuery{
		Who: tracker.PartyOf(handle), Level: statelog.ReadStale,
	}, wednesday, time.UTC)
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
		[]string{"c", "a", "b"}, nil, tracker.PersonAuthority{}); err != nil {
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

// MY_WORK NAMES SOMEBODY, always. A day with nobody's name on it is
// everybody's, and a reader that defaulted the viewer would answer about
// whoever it happened to pick.
func TestMyWorkNamesSomebody(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if _, err := r.reader.MyWork(t.Context(), tracker.MyWorkQuery{
		Level: statelog.ReadStale,
	}, wednesday, time.UTC); err == nil {
		t.Error("my_work with no handle answered")
	}
	if _, err := r.reader.MyWork(t.Context(), tracker.MyWorkQuery{
		Who: tracker.PartyOf("ana"),
	}, wednesday, time.UTC); err == nil {
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

// A BLOCK IS A PAGE AND ITS TOTAL IS THE CLAIM.
//
// Every block carries at most [tracker.MyWorkRows] rows, so a count drawn from
// a block's length says twenty for the person holding twenty-five — the page
// size reported as a fact about their day. The total is counted by the block's
// own predicate, in the same transaction as the page.
func TestMyWorkTotalsExceedThePageCap(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	const held = tracker.MyWorkRows + 5
	ids := make([]string, 0, held)
	for i := range held {
		id := fmt.Sprintf("t-%02d", i)
		ids = append(ids, id)
		assign(t, r, id, "ana")
	}
	// A PRIORITY LIST LONGER THAN THE PAGE whose head is finished work: the
	// page is cut AFTER the finished entries are dropped, so the first open
	// twenty are what the block carries and the total is every open entry.
	if _, err := r.writer.WritePriorities(t.Context(), "op-prio", "ana", ids,
		nil, tracker.PersonAuthority{}); err != nil {
		t.Fatalf("WritePriorities: %v", err)
	}
	r.drain()
	done := tracker.StatusDone
	for _, id := range ids[:3] {
		if _, err := r.writer.UpdateTask(t.Context(), "op-done-"+id, id, "ENG",
			tracker.NoIfMatch, tracker.TaskPatch{Status: &done},
			tracker.ChangeStatus, nil); err != nil {
			t.Fatalf("finish %s: %v", id, err)
		}
		r.drain()
	}
	const asks = tracker.MyWorkRows + 2
	for i := range asks {
		askOn(t, r, fmt.Sprintf("op-ask-%02d", i), ids[3], tracker.Comment{
			ID: fmt.Sprintf("c-%02d", i), Task: ids[3], Author: "bob",
			AuthorKind: tracker.AuthorHuman, Body: "which region?", Ask: "ana",
			CreatedAt: wednesday,
		})
	}

	got := r.myWork("ana")
	open := held - 3
	if len(got.Assigned) != tracker.MyWorkRows {
		t.Fatalf("assigned carries %d rows, want the page of %d",
			len(got.Assigned), tracker.MyWorkRows)
	}
	if want := (tracker.ClaimTotal{Total: open}); got.Totals.Assigned != want {
		t.Errorf("assigned's total is %+v, want %+v — every open task ana "+
			"holds, not the page", got.Totals.Assigned, want)
	}
	if len(got.Priorities) != tracker.MyWorkRows {
		t.Errorf("priorities carries %d rows, want the page of %d — the list "+
			"was cut before its finished head was dropped", len(got.Priorities),
			tracker.MyWorkRows)
	}
	if len(got.Priorities) > 0 && got.Priorities[0].ID != ids[3] {
		t.Errorf("priorities opens on %s, want %s — the first OPEN entry in "+
			"the stored order", got.Priorities[0].ID, ids[3])
	}
	if want := (tracker.ClaimTotal{Total: open}); got.Totals.Priorities != want {
		t.Errorf("priorities' total is %+v, want %+v", got.Totals.Priorities, want)
	}
	if len(got.AskedOfMe) != tracker.MyWorkRows {
		t.Errorf("asked_of_me carries %d rows, want the page of %d",
			len(got.AskedOfMe), tracker.MyWorkRows)
	}
	if want := (tracker.ClaimTotal{Total: asks}); got.Totals.AskedOfMe != want {
		t.Errorf("asked_of_me's total is %+v, want %+v", got.Totals.AskedOfMe, want)
	}
	// AND A BLOCK WITH NOTHING IN IT COUNTS ZERO, not "not counted".
	if got.Totals.Collaborating != (tracker.ClaimTotal{}) {
		t.Errorf("collaborating's total is %+v over an empty block",
			got.Totals.Collaborating)
	}
}

// EVERY BLOCK HAS A TOTAL, under the block's own name. A block added to the
// answer without one is the block whose heading falls back to a length.
func TestEveryMyWorkBlockHasATotal(t *testing.T) {
	t.Parallel()
	totals := map[string]bool{}
	tt := reflect.TypeFor[tracker.MyWorkTotals]()
	for i := range tt.NumField() {
		totals[strings.Split(tt.Field(i).Tag.Get("json"), ",")[0]] = true
	}
	mw := reflect.TypeFor[tracker.MyWork]()
	blocks := 0
	for i := range mw.NumField() {
		field := mw.Field(i)
		if field.Type.Kind() != reflect.Slice {
			continue
		}
		blocks++
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if !totals[name] {
			t.Errorf("block %q has no total in MyWorkTotals", name)
		}
		delete(totals, name)
	}
	for name := range totals {
		t.Errorf("MyWorkTotals carries %q, which is no block of MyWork", name)
	}
	if blocks != 7 {
		t.Errorf("MyWork has %d blocks; the package doc names seven", blocks)
	}
}

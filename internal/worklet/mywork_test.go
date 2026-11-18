package worklet_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

func (r *roundTrip) myWork(handle string) worklet.MyWork {
	r.t.Helper()
	out, err := r.reader.MyWork(r.t.Context(), worklet.MyWorkQuery{
		Handle: handle, Level: statelog.ReadStale,
	}, wednesday, time.UTC)
	if err != nil {
		r.t.Fatalf("MyWork(%s): %v", handle, err)
	}
	return out
}

// askOn writes one comment onto a task: a comment is a mutation of its task,
// arbitrated by the task's version, with no verb of its own.
func askOn(t *testing.T, r *roundTrip, opID, task string, comment worklet.Comment) {
	t.Helper()
	if _, err := r.writer.UpdateTask(t.Context(), opID, task, "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Comment: &comment}, worklet.ChangeComment, nil); err != nil {
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

// PRIORITIES COME BACK IN THEIR STORED ORDER: the order is what somebody
// decided, and any re-sort discards it.
func TestMyWorkKeepsThePriorityOrder(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for _, id := range []string{"a", "b", "c"} {
		assign(t, r, id, "ana")
	}
	// Neither alphabetical nor creation order, so a sort by either shows.
	if _, err := r.writer.WritePriorities(t.Context(), "op-prio", "ana",
		[]string{"c", "a", "b"}, nil, worklet.PersonAuthority{}); err != nil {
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

	// A FINISHED TASK DROPS OUT WITHOUT THE LIST BEING REWRITTEN: a read
	// must not write somebody's own object.
	done := worklet.StatusDone
	if _, err := r.writer.UpdateTask(t.Context(), "op-done", "a", "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Status: &done}, worklet.ChangeStatus, nil); err != nil {
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

// ASSIGNED AND COLLABORATING ARE DIFFERENT CLAIMS, and never overlap.
func TestMyWorkSeparatesTheSevenClaims(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	assign(t, r, "mine", "ana")
	assign(t, r, "theirs", "bob")

	// COLLABORATING: brought on without owning.
	if _, err := r.writer.UpdateTask(t.Context(), "op-collab", "theirs", "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{
			Collaborators: &[]string{"ana"},
		}, worklet.ChangeCollaborators, nil); err != nil {
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
	// AND THE TWO NEVER OVERLAP: an owned task is in `assigned` only.
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

// WATCHING IS WHAT A PERSON WATCHES AND DOES NOT HOLD, MINUS WHAT THEY MUTED.
//
// Each term keeps a different task out: one only somebody else watches, one
// they hold (it is in `assigned`), one they muted. The plan gate holds the
// read to the person's index; this holds it to the answer.
func TestMyWorkWatchingIsWhatAPersonWatchesUnmuted(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for _, seed := range []struct {
		id, assignee    string
		watchers, muted []string
	}{
		{"watched", "bob", []string{"ana", "bob"}, nil},
		{"held", "ana", []string{"ana"}, nil},
		{"muted", "bob", []string{"ana", "bob"}, []string{"ana"}},
		{"others", "bob", []string{"bob"}, nil},
	} {
		task := newTask(seed.id)
		task.Assignee, task.Watchers, task.Muted = seed.assignee, seed.watchers, seed.muted
		if _, err := r.writer.CreateTask(t.Context(), "op-"+seed.id, task, nil); err != nil {
			t.Fatalf("CreateTask %s: %v", seed.id, err)
		}
		r.drain()
	}
	got := r.myWork("ana")
	watching := make([]string, 0, len(got.WatchingRecent))
	for _, row := range got.WatchingRecent {
		watching = append(watching, row.ID)
	}
	if len(watching) != 1 || watching[0] != "watched" {
		t.Errorf("watching is %v, want only the task ana watches, does not "+
			"hold and has not muted", watching)
	}
	if got.Totals.WatchingRecent.Total != 1 {
		t.Errorf("the watching total is %+v, want the one row the block lists",
			got.Totals.WatchingRecent)
	}
}

// AN ASK CARRIES THE CALL THAT ANSWERS IT, so a model spends no round composing
// one and being refused; an answered ask leaves the block.
func TestMyWorkCarriesTheAnswerForm(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	assign(t, r, "asked", "ana")
	askOn(t, r, "op-ask", "asked", worklet.Comment{
		ID: "c-1", Task: "asked", Author: "ana", AuthorKind: worklet.AuthorHuman,
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
	if !strings.Contains(ask.Answer, worklet.CommentOnWorkTool) ||
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

	// AND AN ANSWERED ASK LEAVES THE BLOCK.
	answers := ask.Comment
	askOn(t, r, "op-answer", "asked", worklet.Comment{
		ID: "c-2", Task: "asked", Author: "ana", AuthorKind: worklet.AuthorHuman,
		Body: "eu-west-1", Answers: &answers, CreatedAt: wednesday,
	})
	if after := r.myWork("ana"); len(after.AskedOfMe) != 0 {
		t.Errorf("asked_of_me still holds %d after the ask was answered",
			len(after.AskedOfMe))
	}
}

// MY_WORK NAMES SOMEBODY, AND A LEVEL: a reader that defaulted the viewer
// would answer about whoever it picked.
func TestMyWorkNamesSomebody(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if _, err := r.reader.MyWork(t.Context(), worklet.MyWorkQuery{
		Level: statelog.ReadStale,
	}, wednesday, time.UTC); err == nil {
		t.Error("my_work with no handle answered")
	}
	if _, err := r.reader.MyWork(t.Context(), worklet.MyWorkQuery{
		Handle: "ana",
	}, wednesday, time.UTC); err == nil {
		t.Error("my_work with no read level answered — a level a surface did " +
			"not resolve is a label rather than a guarantee")
	}
}

// A COLLECTION IS EMPTY, NEVER NULL: a wire contract. A nil slice marshals to
// `null`, clients declare these keys as arrays, and `.length` on a null throws
// and takes the page down. Empty blocks are this answer's ORDINARY state.
func TestAnEmptyDayCarriesEmptyCollectionsRatherThanNulls(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	// Nobody by this handle holds anything.
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

	// The claim is about the wire, so assert on it.
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), ":null") {
		t.Errorf("an empty day marshals a null: %s", raw)
	}
}

// A BLOCK IS A PAGE AND ITS TOTAL IS THE CLAIM: a block's length says
// [worklet.MyWorkRows] for the person holding more, so the total counts every
// row by the block's own predicate.
func TestMyWorkTotalsExceedThePageCap(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	const held = worklet.MyWorkRows + 5
	ids := make([]string, 0, held)
	for i := range held {
		id := fmt.Sprintf("t-%02d", i)
		ids = append(ids, id)
		assign(t, r, id, "ana")
	}
	// A priority list longer than the page with a finished head: the page
	// is cut AFTER the filter, and the total is every open entry.
	if _, err := r.writer.WritePriorities(t.Context(), "op-prio", "ana", ids,
		nil, worklet.PersonAuthority{}); err != nil {
		t.Fatalf("WritePriorities: %v", err)
	}
	r.drain()
	done := worklet.StatusDone
	for _, id := range ids[:3] {
		if _, err := r.writer.UpdateTask(t.Context(), "op-done-"+id, id, "ENG",
			worklet.NoIfMatch, worklet.TaskPatch{Status: &done},
			worklet.ChangeStatus, nil); err != nil {
			t.Fatalf("finish %s: %v", id, err)
		}
		r.drain()
	}
	const asks = worklet.MyWorkRows + 2
	for i := range asks {
		askOn(t, r, fmt.Sprintf("op-ask-%02d", i), ids[3], worklet.Comment{
			ID: fmt.Sprintf("c-%02d", i), Task: ids[3], Author: fmt.Sprintf("bob-%02d", i),
			AuthorKind: worklet.AuthorHuman, Body: "which region?", Ask: "ana",
			CreatedAt: wednesday,
		})
	}

	got := r.myWork("ana")
	open := held - 3
	if len(got.Assigned) != worklet.MyWorkRows {
		t.Fatalf("assigned carries %d rows, want the page of %d",
			len(got.Assigned), worklet.MyWorkRows)
	}
	if want := (worklet.ClaimTotal{Total: open}); got.Totals.Assigned != want {
		t.Errorf("assigned's total is %+v, want %+v — every open task ana "+
			"holds, not the page", got.Totals.Assigned, want)
	}
	if len(got.Priorities) != worklet.MyWorkRows {
		t.Errorf("priorities carries %d rows, want the page of %d — the list "+
			"was cut before its finished head was dropped", len(got.Priorities),
			worklet.MyWorkRows)
	}
	if len(got.Priorities) > 0 && got.Priorities[0].ID != ids[3] {
		t.Errorf("priorities opens on %s, want %s — the first OPEN entry in "+
			"the stored order", got.Priorities[0].ID, ids[3])
	}
	if want := (worklet.ClaimTotal{Total: open}); got.Totals.Priorities != want {
		t.Errorf("priorities' total is %+v, want %+v", got.Totals.Priorities, want)
	}
	if len(got.AskedOfMe) != worklet.MyWorkRows {
		t.Errorf("asked_of_me carries %d rows, want the page of %d",
			len(got.AskedOfMe), worklet.MyWorkRows)
	}
	if want := (worklet.ClaimTotal{Total: asks}); got.Totals.AskedOfMe != want {
		t.Errorf("asked_of_me's total is %+v, want %+v", got.Totals.AskedOfMe, want)
	}
	// AN EMPTY BLOCK COUNTS ZERO.
	if got.Totals.Collaborating != (worklet.ClaimTotal{}) {
		t.Errorf("collaborating's total is %+v over an empty block",
			got.Totals.Collaborating)
	}
}

// EVERY BLOCK HAS A TOTAL, under its own name, or its heading falls back to a
// length.
func TestEveryMyWorkBlockHasATotal(t *testing.T) {
	t.Parallel()
	totals := map[string]bool{}
	tt := reflect.TypeFor[worklet.MyWorkTotals]()
	for i := range tt.NumField() {
		totals[strings.Split(tt.Field(i).Tag.Get("json"), ",")[0]] = true
	}
	mw := reflect.TypeFor[worklet.MyWork]()
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
	if blocks != 8 {
		t.Errorf("MyWork has %d blocks; the package doc names eight", blocks)
	}
}

// AN ASK IS WHOLE OR BY REFERENCE, NEVER CUT: whole bodies up to the budget,
// the first always, and the rest with their size and the read that returns
// them.
func TestMyWorkCarriesAsksWholeOrByReference(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	const asks = 4
	bodies := map[string]string{}
	for i := range asks {
		task := fmt.Sprintf("asked-%d", i)
		assign(t, r, task, "ana")
		body := strings.Repeat(fmt.Sprintf("context %d. ", i), 600) + "WHICH REGION?"
		id := fmt.Sprintf("c-%d", i)
		bodies[id] = body
		askOn(t, r, "op-ask-"+task, task, worklet.Comment{
			ID: id, Task: task, Author: "ana", AuthorKind: worklet.AuthorHuman,
			Body: body, Ask: "ana", CreatedAt: wednesday.Add(time.Duration(i) * time.Minute),
		})
	}
	got := r.myWork("ana")
	if len(got.AskedOfMe) != asks {
		t.Fatalf("asked_of_me holds %d of %d", len(got.AskedOfMe), asks)
	}
	whole, referenced, weight := 0, 0, 0
	for i, ask := range got.AskedOfMe {
		switch {
		case ask.Body != "":
			if ask.Body != bodies[ask.Comment] {
				t.Fatalf("ask %s came back as %d of %d bytes", ask.Comment, len(ask.Body), len(bodies[ask.Comment]))
			}
			whole++
			weight += len(ask.Body)
		case ask.BodyNotIncluded != "":
			if i == 0 {
				t.Fatal("the first ask was left out; a block always carries one whole")
			}
			if !strings.Contains(ask.BodyNotIncluded, ask.Comment) ||
				!strings.Contains(ask.BodyNotIncluded, strconv.Itoa(len(bodies[ask.Comment]))+" bytes") {
				t.Errorf("a left-out ask does not say how to read it: %q", ask.BodyNotIncluded)
			}
			referenced++
		default:
			t.Fatalf("ask %s carries neither its body nor a reference to it", ask.Comment)
		}
	}
	if whole == 0 || referenced == 0 {
		t.Fatalf("whole %d, referenced %d — the fixture must exercise both", whole, referenced)
	}
	if whole > 1 && weight > worklet.MyWorkAskBytes {
		t.Fatalf("the block carries %d bytes of whole asks, past its %d", weight, worklet.MyWorkAskBytes)
	}
}

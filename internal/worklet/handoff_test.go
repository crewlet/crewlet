package worklet_test

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// inProgress moves a task's status to one more step of work, within the
// active group, so a case about another move never also crosses the finished
// edge.
func inProgress(task *worklet.Task) {
	task.Status, task.StatusGroup = worklet.StatusInProgress, worklet.GroupActive
}

// A HAND-OFF IS TOLD WHATEVER KIND ITS RECORD ENDS WITH (#953). A record
// carries one kind, and `update_work_item` files "assign it to bo and set it
// in progress" as a status change: decided by that kind, ana, who held the
// task, and pm, who filed it, heard nothing, and bo was woken as a status move
// he could absorb rather than as work he was handed. The first case is that
// record. The tags and watch cases are records no writer files a hand-off
// under today, which pin the rule for any that does: whatever kind carries
// it, a hand-off reaches everybody it concerns as the hand-off written alone,
// the last case, does.
//
// Mutation: tell the old holder only on kind `assignee` — ana hears nothing of
// the first three; tell the reporter of a reassignment only on that kind — pm
// hears nothing of them; ask the new holder only on that kind — bo may absorb
// them.
func TestAHandOffIsToldWhateverKindItsRecordEndsWith(t *testing.T) {
	t.Parallel()
	held := newTask("t-1")
	held.Assignee, held.Reporter = "ana", "pm"
	for _, c := range []struct {
		name string
		kind worklet.ChangeKind
		also func(*worklet.Task)
	}{
		{"written with a status", worklet.ChangeStatus, inProgress},
		{"written with tags", worklet.ChangeTags, func(task *worklet.Task) {
			task.Tags = []string{"urgent"}
		}},
		{"written with a watch", worklet.ChangeWatchers, func(task *worklet.Task) {
			task.Watchers = []string{"cy"}
		}},
		{"written alone", worklet.ChangeAssignee, func(*worklet.Task) {}},
	} {
		handed := held
		handed.Assignee = "bo"
		c.also(&handed)
		n := worklet.Wake{Kind: c.kind, Before: held, After: handed}.Notify(nil)
		for handle, want := range map[string]worklet.Reason{
			"ana": worklet.ReasonUnassigned,
			"pm":  worklet.ReasonReporter,
			"bo":  worklet.ReasonAssignee,
		} {
			if heard, ok := worklet.CandidateFor(n, handle); !ok || heard.Reason != want {
				t.Errorf("a hand-off %s, a %s record: %s hears %+v (told: %v), want %s",
					c.name, c.kind, handle, heard, ok, want)
			}
		}
		if heard, _ := worklet.CandidateFor(n, "bo"); !heard.Wakes || !heard.Addressed {
			t.Errorf("a hand-off %s, a %s record, reaches bo as %+v, want woken and "+
				"asked to answer", c.name, c.kind, heard)
		}
	}
}

// A REASSERTION WRITTEN WITH ANOTHER CHANGE HANDS NOTHING OVER (#918, #953):
// "assign it to ana", of a task ana already holds, written beside a status
// move or a watch, moves no assignee. So it tells nobody the task went
// anywhere, its reporter is not told of a reassignment, and ana hears the
// change the write did make, without being asked to answer for it.
//
// A reassertion written beside a checklist item handed to somebody moves no
// delta at all, and still reaches the item's holders: nothing else is
// everything the record says the write did.
//
// Mutation: decide the hand-off by the record's kind as well as its delta — the
// reassertion written with a watch tells pm and asks ana; read the reassertion
// off the deltas alone — the item's holders hear nothing.
func TestAReassertionWrittenWithAnotherChangeHandsNothingOver(t *testing.T) {
	t.Parallel()
	item := func(holder string) []worklet.Checklist {
		return []worklet.Checklist{{ID: "cl-1", Name: "launch", Items: []worklet.ChecklistItem{
			{ID: "i-1", Name: "docs", Assignee: holder},
		}}}
	}
	held := newTask("t-1")
	held.Assignee, held.Reporter, held.Checklists = "ana", "pm", item("dee")
	for _, c := range []struct {
		name string
		kind worklet.ChangeKind
		also func(*worklet.Task)
		told []string
	}{
		{"written with a status", worklet.ChangeStatus, inProgress, []string{"ana"}},
		{"written with a watch", worklet.ChangeAssignee, func(task *worklet.Task) {
			task.Watchers = []string{"cy"}
		}, []string{"ana", "cy"}},
		{"written with a checklist item handed to somebody", worklet.ChangeAssignee,
			func(task *worklet.Task) { task.Checklists = item("fa") },
			[]string{"ana", "dee", "fa"}},
	} {
		after := held
		c.also(&after)
		n := worklet.Wake{Kind: c.kind, Before: held, After: after}.Notify(nil)
		if got := handles(worklet.Candidates(n)); !slices.Equal(got, c.told) {
			t.Errorf("a reassertion %s tells %v, want only %v: nobody hears that the "+
				"task went anywhere", c.name, got, c.told)
		}
		if heard, ok := worklet.CandidateFor(n, "ana"); !ok ||
			heard.Reason != worklet.ReasonAssignee || heard.Addressed {
			t.Errorf("a reassertion %s reaches its holder as %+v (told: %v), want "+
				"told as its assignee and not asked", c.name, heard, ok)
		}
	}
}

// A WRITE'S TRANSITIONS ARE TOLD WHATEVER KIND ITS RECORD ENDS WITH (#953):
// `update_work_item` files a hand-off written beside a re-route or a checklist
// gesture as an `assignee` record, and a hand-off that also drops a watcher,
// routes the task to another team and ticks a checklist item still reaches
// whom each transition concerns, as a record of that transition's own kind
// does: the dropped watcher's inbox, the new team's lead and the item's
// holder. The parent's assignee is the write's own to resolve
// (TestAnUpdatesWakeIsWhatItsWriteMoved).
//
// Mutation: fill or read any one of the three on its own kind alone — that
// one's handle is told nothing.
func TestAWritesTransitionsAreToldWhateverKindItsRecordEndsWith(t *testing.T) {
	t.Parallel()
	item := func(done bool) []worklet.Checklist {
		return []worklet.Checklist{{ID: "cl-1", Name: "launch", Items: []worklet.ChecklistItem{
			{ID: "i-1", Name: "docs", Done: done, Assignee: "dee"},
		}}}
	}
	held := newTask("t-1")
	held.Assignee, held.Watchers, held.Checklists = "ana", []string{"cy"}, item(false)
	after := held
	after.Assignee, after.Watchers, after.RoutingUnit = "bo", nil, "platform"
	after.Checklists = item(true)
	n := worklet.Wake{Kind: worklet.ChangeAssignee, Before: held, After: after}.
		Notify(fixedLeads{project: "eng-lead", unit: "platform-lead"})
	for handle, want := range map[string]worklet.Reason{
		"cy":            worklet.ReasonUnwatched,
		"platform-lead": worklet.ReasonRoutedTo,
		"dee":           worklet.ReasonChecklist,
	} {
		if heard, ok := worklet.CandidateFor(n, handle); !ok || heard.Reason != want {
			t.Errorf("a hand-off that also moved what %s answers for: %s hears %+v "+
				"(told: %v), want %s", handle, handle, heard, ok, want)
		}
	}
}

// A CREATION CARRIES NO TRANSITION: its before is nothing ([worklet.Wake]), so
// a task filed into a team, closed already, with a checklist item somebody
// holds, names no new team's lead and no item holder as moved, its reporter
// hears of no hand-off from nobody and no edge it never crossed, and its own
// arms route it.
//
// Mutation: fill the checklist's holders on a creation — dee is told of a task
// filed with her item; read a creation's deltas as moves, or the finished edge
// without a status move — pm is told.
func TestACreationCarriesNoTransition(t *testing.T) {
	t.Parallel()
	filed := newTask("t-1")
	filed.Assignee, filed.Reporter, filed.RoutingUnit = "ana", "pm", "platform"
	filed.Status, filed.StatusGroup = worklet.StatusDone, worklet.GroupDone
	filed.Checklists = []worklet.Checklist{{ID: "cl-1", Name: "launch",
		Items: []worklet.ChecklistItem{{ID: "i-1", Name: "docs", Assignee: "dee"}}}}
	n := worklet.Wake{Kind: worklet.ChangeCreated, After: filed}.
		Notify(fixedLeads{project: "eng-lead", unit: "platform-lead"})
	if s := n.Snapshot; s.RoutedTo != "" || s.ChecklistAssignees != nil {
		t.Errorf("a creation names transitions: routed to %q, checklist %v; want none",
			s.RoutedTo, s.ChecklistAssignees)
	}
	for _, handle := range []string{"dee", "pm"} {
		if heard, ok := worklet.CandidateFor(n, handle); ok {
			t.Errorf("a creation tells %s %+v, want nothing", handle, heard)
		}
	}
}

// A WATCH WRITTEN WITH ANOTHER CHANGE IS THAT CHANGE (#953): `update_work_item`
// files a watch written beside a priority or a checklist gesture under
// `watchers` wherever nothing else named a kind, and the seat that answers for
// the task is woken by it as by the change written alone. A watch alone asks
// it nothing. The checklist item handed to somebody moves no list's done
// count, so no delta says it moved: the holders it touched do. A custom value
// written with a watch is the decide's to name
// (TestAnUpdatesWakeIsWhatItsWriteMoved).
//
// Mutation: decide a watch by its kind (`ownerAsked`'s `!= ChangeWatchers`) —
// ana sleeps through the priority and the item; let watchOnly read the deltas
// alone — she sleeps through the item.
func TestAWatchWrittenWithAnotherChangeWakesTheOwners(t *testing.T) {
	t.Parallel()
	item := func(holder string) []worklet.Checklist {
		return []worklet.Checklist{{ID: "cl-1", Name: "launch", Items: []worklet.ChecklistItem{
			{ID: "i-1", Name: "docs", Assignee: holder},
		}}}
	}
	held := newTask("t-1")
	held.Assignee, held.Checklists = "ana", item("dee")
	watched := held
	watched.Watchers = []string{"cy"}
	prioritised := watched
	prioritised.Priority = worklet.PriorityHigh
	handed := watched
	handed.Checklists = item("fa")
	for _, c := range []struct {
		name  string
		after worklet.Task
		wakes bool
	}{
		{"a watch alone", watched, false},
		{"a watch with a priority", prioritised, true},
		{"a watch with a checklist item handed to somebody", handed, true},
	} {
		n := worklet.Wake{Kind: worklet.ChangeWatchers, Before: held, After: c.after}.Notify(nil)
		heard, ok := worklet.CandidateFor(n, "ana")
		if !ok || heard.Reason != worklet.ReasonAssignee || heard.Wakes != c.wakes {
			t.Errorf("%s reaches ana as %+v (told: %v), want told as its assignee, "+
				"woken: %v", c.name, heard, ok, c.wakes)
		}
	}
}

// A TURN THAT WATCHED A TASK BESIDE ANOTHER CHANGE MOVED WORK (#953), on both
// sides a turn's outcome is read from: the change its writer reports the
// record committed, and the history rows a schedule's fires are read by
// ([worklet.Reader.TurnsMoved]). A turn that only watched moved nothing on
// either. All are `watchers` records. The link is a gesture the caller's wake
// cannot apply, which the decide resolves and its restated wake records, so
// the two sides agree on it too.
//
// Mutation: read a `watchers` record by its kind in committedChange — the
// priority's and the link's turns committed nothing; in TurnsMoved — they are
// missing from the answer; restate no update's wake in its decide — the
// link's turn committed nothing.
func TestATurnThatWatchedBesideAnotherChangeMovedWork(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	watch := func(turn string, also func(*worklet.Task, *worklet.TaskPatch)) []types.CommittedChange {
		t.Helper()
		task := r.createTask("the health check, for " + turn)
		log := &writeLog{}
		provenance := aTurnNobodyWoke()
		provenance.TurnID, provenance.Written = turn, log
		w := r.writer.As("dev", worklet.AuthorAgent, provenance)
		after := task
		after.Watchers = append(slices.Clone(task.Watchers), "dev")
		watchers := slices.Clone(after.Watchers)
		patch := worklet.TaskPatch{Watchers: &watchers}
		also(&after, &patch)
		if _, err := w.UpdateTask(t.Context(), "op-"+turn, task.ID, task.Project,
			worklet.NoIfMatch, patch, worklet.ChangeWatchers,
			worklet.Wake{Kind: worklet.ChangeWatchers, Before: task, After: after}.Notify(nil)); err != nil {
			t.Fatalf("%s: %v", turn, err)
		}
		r.drain()
		return log.committed()
	}
	if got := watch("turn-watch", func(*worklet.Task, *worklet.TaskPatch) {}); len(got) != 0 {
		t.Errorf("a turn that only watched committed %v, want nothing of the work", got)
	}
	high := worklet.PriorityHigh
	got := watch("turn-priority", func(after *worklet.Task, patch *worklet.TaskPatch) {
		after.Priority, patch.Priority = high, &high
	})
	if !slices.Equal(got, []types.CommittedChange{types.CommittedItem}) {
		t.Errorf("a turn that watched and raised the priority committed %v, want the item", got)
	}
	other := r.createTask("the dashboard it feeds")
	got = watch("turn-link", func(_ *worklet.Task, patch *worklet.TaskPatch) {
		patch.Relate = &worklet.RelationIntent{Add: []worklet.Relation{
			{Kind: worklet.RelationLinked, Other: other.ID}}}
	})
	if !slices.Equal(got, []types.CommittedChange{types.CommittedItem}) {
		t.Errorf("a turn that watched and linked the task committed %v, want the item", got)
	}
	moved, err := r.reader.TurnsMoved(t.Context(), worklet.TurnsMovedQuery{
		Turns: []string{"turn-watch", "turn-priority", "turn-link"}, Level: statelog.ReadStale})
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]bool{"turn-priority": true, "turn-link": true}; !reflect.DeepEqual(moved, want) {
		t.Errorf("the turns that moved work are %v, want the ones that raised the "+
			"priority and linked the task", moved)
	}
}

// A HAND-OFF WRITTEN WITH A STATUS TELLS ITS NEW HOLDER THE TASK WAS ASSIGNED TO
// THEM (#953), and its change line names both moves, through the parser as the
// router decides: a `status` record whose wake names the assignee it moved. A
// reassertion written with a status hands nothing over, and its line says only
// what moved.
//
// Mutation: open by the record's kind — cy reads "A task you are assigned to
// changed."; lead by the record's kind alone — the hand-off's line never says
// the assignee changed.
func TestAHandOffWrittenWithAStatusTellsItsNewHolderItWasAssigned(t *testing.T) {
	t.Parallel()
	held := newTask("t-1")
	held.Assignee = "ana"
	handed := held
	handed.Assignee = "cy"
	inProgress(&handed)
	body := promptFor(t, parseRecord(
		worklet.Wake{Kind: worklet.ChangeStatus, Before: held, After: handed}.Notify(nil)))
	for _, want := range []string{
		"A task was assigned to you.",
		"The status changed by ana. The assignee changed too.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the new holder of a hand-off written with a status is not told %q:\n%s",
				want, body)
		}
	}
	kept := held
	kept.Assignee = "cy"
	moving := kept
	inProgress(&moving)
	body = promptFor(t, parseRecord(
		worklet.Wake{Kind: worklet.ChangeStatus, Before: kept, After: moving}.Notify(nil)))
	if !strings.Contains(body, "A task you are assigned to changed.") ||
		strings.Contains(body, "assigned to you") || strings.Contains(body, "assignee changed") {
		t.Errorf("a status move on cy's own task tells cy it was handed the task:\n%s", body)
	}
}

// A COMMENT THAT TOOK A WATCH SAYS ONLY THAT IT WAS ADDED: the one field a
// comment moves is the watch its writer picks up by commenting, which is the
// comment's own, so the change line names no other move.
//
// Mutation: name the moves a conversation's record carries — the line goes on
// to say the watchers changed too.
func TestACommentThatTookAWatchSaysOnlyThatItWasAdded(t *testing.T) {
	t.Parallel()
	task := newTask("t-1")
	task.Assignee = "cy"
	watched := task
	watched.Watchers = []string{"ana"}
	n := worklet.Wake{Kind: worklet.ChangeComment, Before: task, After: watched,
		Comment: &worklet.Comment{ID: "cm-1", Task: "t-1", Author: "ana",
			AuthorKind: worklet.AuthorHuman, Body: "is this still blocked?"}}.Notify(nil)
	if _, moved := n.Fields["watchers"]; !moved {
		t.Fatalf("the comment's record carries no watch, so the case asserts nothing: %+v",
			n.Fields)
	}
	body := promptFor(t, parseRecord(n))
	if !strings.Contains(body, "A comment was added by ana.") || strings.Contains(body, "changed too") {
		t.Errorf("a comment that took a watch says more than that it was added:\n%s", body)
	}
}

// AN UPDATE'S WAKE IS WHAT ITS WRITE MOVED (#953): its decide restates the
// card its caller built outside the write, from the row it stores and the row
// it leaves, because the router decides who hears the change by that card.
// A subtask closed the way a merged change closes its item, its status alone,
// with no parent named and its old group kept, still tells the parent's
// assignee and the reporter. A custom value written with a watch is named by
// its slug, which the caller cannot do without the catalogue, and wakes the
// owner. And a link written with a watch, an edge only the decide's gesture
// resolves, wakes the owner too.
//
// Mutation: restate no update's wake in its decide — eve and pm are never
// told, and the custom value and the link wake nobody; read no parent in the
// decide — eve is never told.
func TestAnUpdatesWakeIsWhatItsWriteMoved(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if _, err := r.writer.WriteFields(t.Context(), "op-fields", []worklet.FieldDef{
		{ID: "f-sev", Slug: "severity", Name: "Severity", Type: worklet.FieldDropdown,
			Config: worklet.FieldConfig{Options: []worklet.Option{
				{ID: "o-low", Slug: "low", Name: "Low"},
				{ID: "o-high", Slug: "high", Name: "High"},
			}}},
	}); err != nil {
		t.Fatalf("declare a field: %v", err)
	}
	r.drain()
	file := func(id, key, assignee string, parent *string) worklet.Task {
		t.Helper()
		task := newTask(id)
		task.Key, task.Assignee, task.Reporter, task.Parent = key, assignee, "pm", parent
		if _, err := r.writer.CreateTask(t.Context(), "op-"+id, task, nil); err != nil {
			t.Fatalf("file %s: %v", id, err)
		}
		r.drain()
		return task
	}
	update := func(op string, before, after worklet.Task, patch worklet.TaskPatch,
		kind worklet.ChangeKind) *worklet.Notify {

		t.Helper()
		if _, err := r.writer.UpdateTask(t.Context(), op, before.ID, before.Project,
			worklet.NoIfMatch, patch, kind,
			worklet.Wake{Kind: kind, Before: before, After: after}.Notify(nil)); err != nil {
			t.Fatalf("%s: %v", op, err)
		}
		r.drain()
		return r.lastWake()
	}

	parent := "t-parent"
	file(parent, "ENG-10", "eve", nil)
	child := file("t-child", "ENG-11", "ana", &parent)
	closed := child
	closed.Status = worklet.StatusDone
	done := worklet.StatusDone
	n := update("op-close", child, closed, worklet.TaskPatch{Status: &done}, worklet.ChangeStatus)
	for handle, want := range map[string]worklet.Reason{
		"eve": worklet.ReasonParentAssignee,
		"pm":  worklet.ReasonReporter,
	} {
		if heard, ok := worklet.CandidateFor(n, handle); !ok || heard.Reason != want {
			t.Errorf("a subtask closed with no parent named: %s hears %+v (told: %v), "+
				"want %s", handle, heard, ok, want)
		}
	}

	valued := file("t-valued", "ENG-12", "ana", nil)
	watchers := []string{"dev"}
	set := map[string]json.RawMessage{"severity": json.RawMessage(`"low"`)}
	after := valued
	after.Watchers, after.Fields = watchers, set
	n = update("op-value", valued, after, worklet.TaskPatch{Watchers: &watchers, Fields: &set},
		worklet.ChangeWatchers)
	if got := n.Fields["fields"]; got.To != "severity=low" {
		t.Errorf("a custom value written with a watch is carded as %+v, want it named "+
			"severity=low", got)
	}
	if heard, _ := worklet.CandidateFor(n, "ana"); !heard.Wakes {
		t.Errorf("a custom value written with a watch reaches its owner as %+v, want woken",
			heard)
	}

	other := file("t-other", "ENG-13", "", nil)
	linked := file("t-linked", "ENG-14", "ana", nil)
	watched := linked
	watched.Watchers = watchers
	n = update("op-link", linked, watched, worklet.TaskPatch{Watchers: &watchers,
		Relate: &worklet.RelationIntent{Add: []worklet.Relation{
			{Kind: worklet.RelationLinked, Other: other.ID}}}}, worklet.ChangeWatchers)
	if _, moved := n.Fields[string(worklet.RelationLinked)]; !moved {
		t.Errorf("a link written with a watch is not on its card: %+v", n.Fields)
	}
	if heard, _ := worklet.CandidateFor(n, "ana"); !heard.Wakes {
		t.Errorf("a link written with a watch reaches its owner as %+v, want woken", heard)
	}
}

// A REASSERTION WRITTEN WITH AN EDIT NO KIND NAMES SAYS THE FIELDS WERE EDITED:
// "assign it to cy", of a task cy holds, written beside a priority, is an
// `assignee` record that moved only the priority, and its change line says
// what happened rather than that the assignee changed.
//
// Mutation: keep a field kind's own line where its field did not move — the
// line says the assignee changed.
func TestAReassertionWithAnEditSaysTheFieldsWereEdited(t *testing.T) {
	t.Parallel()
	held := newTask("t-1")
	held.Assignee = "cy"
	raised := held
	raised.Priority = worklet.PriorityHigh
	body := promptFor(t, parseRecord(
		worklet.Wake{Kind: worklet.ChangeAssignee, Before: held, After: raised}.Notify(nil)))
	if !strings.Contains(body, "Fields were edited by ana.") ||
		strings.Contains(body, "The assignee changed") {
		t.Errorf("a reassertion written with a priority does not say the fields were "+
			"edited:\n%s", body)
	}
}

// A RESTATED ASSIGNMENT WITH ITS STATUS STILL REACHES ITS HOLDER, until #978:
// "assign it to ana and set it to todo", of a task ana holds at todo, moves
// nothing, yet as a `status` record it is not the reassertion the router
// drops, which stays on kind `assignee`. On every kind that rule would also
// drop an unassigned checklist item's rename written beside a restated status,
// which records no delta at all (#978). Pinned so the rule is widened with
// #978's fix, and never by accident.
//
// Mutation: decide the reassertion on any kind — ana is told nothing.
func TestARestatedAssignmentWithItsStatusStillReachesItsHolder(t *testing.T) {
	t.Parallel()
	held := newTask("t-1")
	held.Assignee = "ana"
	n := worklet.Wake{Kind: worklet.ChangeStatus, Before: held, After: held}.Notify(nil)
	if heard, ok := worklet.CandidateFor(n, "ana"); !ok || heard.Reason != worklet.ReasonAssignee {
		t.Errorf("an assignment and a status both restated reach ana as %+v (told: %v), "+
			"want told as its assignee until #978", heard, ok)
	}
}

// A LEAD IS OFFERED UNOWNED WORK BY WHAT ITS WRITE MOVED (#953). lena leads
// platform and re-routes a stray she filed and alone watches, writing labels
// with it, so `update_work_item` files a `tags` record: the new team's lead is
// lena, the actor, whom the route drops, and the project's lead must hear of
// the unowned work instead. A status written as it already was beside a
// priority is a `status` record that moved the priority alone, and pages
// nobody.
//
// Mutation: offer the fallback by the record's kind — the re-route reaches
// nobody, and the priority pages the project's lead.
func TestALeadIsOfferedUnownedWorkByWhatItsWriteMoved(t *testing.T) {
	t.Parallel()
	leads := fixedLeads{project: "eng-lead", unit: "lena"}
	stray := newTask("t-1")
	stray.Reporter, stray.Watchers = "lena", []string{"lena"}
	rerouted := stray
	rerouted.RoutingUnit, rerouted.Tags = "platform", []string{"triage"}
	n := worklet.Wake{Kind: worklet.ChangeTags, Before: stray, After: rerouted}.Notify(leads)
	got := handles(worklet.Route(worklet.Candidates(n), everyone, "lena"))
	if !slices.Equal(got, []string{"eng-lead"}) {
		t.Errorf("a stray re-routed beside labels reaches %v, want the project's lead", got)
	}
	unowned := newTask("t-2")
	raised := unowned
	raised.Priority = worklet.PriorityHigh
	n = worklet.Wake{Kind: worklet.ChangeStatus, Before: unowned, After: raised}.Notify(leads)
	if got = handles(worklet.Route(worklet.Candidates(n), everyone, "ana")); len(got) != 0 {
		t.Errorf("a priority written beside its status as it was reaches %v, want nobody", got)
	}
}

package builtin_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/colleague"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A BARE LIST IS REFUSED NAMING BOTH SHAPES, which is the whole reason these
// arguments are objects.
//
// The two readings of `waiting_on: ["ENG-7"]` are opposite: as a delta it adds
// one edge, and as a set it silently drops every edge not repeated. A tool
// that guessed would be right half the time and lose somebody's dependencies
// the other half — which is exactly what `watchers: [me]` did to the watcher
// set before the watch gesture existed.
func TestABareListIsRefusedForASetValuedArgument(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{
		Reader: trk, Writer: trk.as, Dependencies: trk.depends,
	})
	got := callWork(t, reg, builtin.UpdateWorkItemTool, map[string]any{
		"item": "ENG-1", "waiting_on": []any{"ENG-2"},
	})
	if !got.Failed {
		t.Fatal("a bare list was accepted, so the model cannot know which of " +
			"the two opposite readings it got")
	}
	for _, want := range []string{"set", "add", "remove"} {
		if !strings.Contains(got.Output, want) {
			t.Errorf("the refusal is %q and does not name %q — a refusal that "+
				"names one shape is one a caller satisfies without learning "+
				"the other exists", got.Output, want)
		}
	}
	if len(trk.depended) != 0 {
		t.Errorf("the refused call still wrote %+v", trk.depended)
	}
}

// A `set` GESTURE REACHES THE WRITER AS A DELTA, because the two ends of a
// dependency are two commits and the sequence has to know which edges ARRIVED
// and which LEFT to write the right mirror on the right counterparty.
func TestAWholeSetOfDependenciesBecomesADelta(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	// THE ITEM ALREADY WAITS ON TWO, so a `set` naming one of them and a
	// new one is exactly one add and one remove.
	detail := trk.tasks["ENG-1"]
	detail.Task.Relations = []tracker.Relation{
		{Kind: tracker.RelationWaitingOn, Other: "id-2"},
		{Kind: tracker.RelationWaitingOn, Other: "id-3"},
	}
	trk.tasks["ENG-1"] = detail
	reg := workRegistry(t, builtin.WorkDeps{
		Reader: trk, Writer: trk.as, Dependencies: trk.depends,
	})
	got := callWork(t, reg, builtin.UpdateWorkItemTool, map[string]any{
		"item":       "ENG-1",
		"waiting_on": map[string]any{"set": []any{"ENG-2", "ENG-4"}},
	})
	if got.Failed {
		t.Fatalf("the set gesture failed: %s", got.Output)
	}
	if len(trk.depended) != 1 {
		t.Fatalf("the call made %d dependency changes, want one", len(trk.depended))
	}
	change := trk.depended[0]
	if !slices.Equal(change.WaitingOnAdd, []string{"id-4"}) {
		t.Errorf("adds = %v, want the one edge the set introduced — every id "+
			"resolved, because a relation stores an id and a key stored in "+
			"one resolves to nothing on every node", change.WaitingOnAdd)
	}
	if !slices.Equal(change.WaitingOnRemove, []string{"id-3"}) {
		t.Errorf("removes = %v, want the one edge the set dropped",
			change.WaitingOnRemove)
	}
}

// A DEPENDENCY-ONLY CALL WRITES NO PATCH, because an empty patch is a real
// write: it stamps a version and puts a `fields` commit in the feed that
// changed no fields.
func TestADependencyOnlyCallWritesNoPatch(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{
		Reader: trk, Writer: trk.as, Dependencies: trk.depends,
	})
	got := callWork(t, reg, builtin.UpdateWorkItemTool, map[string]any{
		"item":     "ENG-1",
		"blocking": map[string]any{"add": []any{"ENG-2"}},
	})
	if got.Failed {
		t.Fatalf("the dependency call failed: %s", got.Output)
	}
	if len(trk.patched) != 0 {
		t.Errorf("a call that changed no field of the item still wrote %d "+
			"patches: %+v", len(trk.patched), trk.patched)
	}
	if len(trk.depended) != 1 {
		t.Fatalf("the dependency change did not reach the sequence")
	}
	// AND THE ANSWER STILL CARRIES AN OUTCOME, which a caller reads to
	// know its write landed.
	var answer map[string]any
	if err := json.Unmarshal([]byte(got.Output), &answer); err != nil {
		t.Fatalf("the answer is not json: %v", err)
	}
	if answer["outcome"] == nil {
		t.Errorf("the answer carries no outcome: %v", answer)
	}
}

// A RE-ROUTE IS THE PROJECT LEAD'S, and the refusal says whose.
func TestARerouteIsRefusedToASeatThatDoesNotLead(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})
	got := callWork(t, reg, builtin.UpdateWorkItemTool, map[string]any{
		"item": "ENG-1", "routing_unit": "backend",
	})
	if !got.Failed {
		t.Fatal("an ungated seat pointed somebody else's work at another team")
	}
	if !strings.Contains(got.Output, "lead") {
		t.Errorf("the refusal is %q and does not say whose decision it is",
			got.Output)
	}
	if len(trk.patched) != 0 {
		t.Errorf("the refused re-route still wrote %+v", trk.patched)
	}
}

// AN ASK RESOLVES ITS HANDLE AND TRAVELS ON THE WAKE.
//
// `Comment.Ask` had no writer at all, so the whole open-ask mechanism —
// `my_work.asked_of_me`, the `has_open_asks` filter, the `asked` routing arm —
// read back columns nothing ever wrote.
func TestAnAskIsResolvedAndRoutes(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{
		Reader: trk, Writer: trk.as, Seats: askable,
	})
	got := callWork(t, reg, builtin.CommentOnWorkTool, map[string]any{
		"item": "ENG-1", "body": "which of these do you want?", "ask": "pm",
	})
	if got.Failed {
		t.Fatalf("the ask failed: %s", got.Output)
	}
	if len(trk.patched) != 1 || trk.patched[0].Comment == nil {
		t.Fatalf("the comment did not ride the task's own write")
	}
	if trk.patched[0].Comment.Ask != "pm" {
		t.Errorf("the comment asks %q — with nothing writing this column the "+
			"whole open-ask mechanism reads back rows that never existed",
			trk.patched[0].Comment.Ask)
	}
	if trk.threadQuery.Ask != "pm" {
		t.Errorf("the thread read was asked about %q", trk.threadQuery.Ask)
	}
	wake := trk.notified[0]
	if wake == nil || wake.Snapshot.CommentAsk != "pm" {
		t.Fatalf("the wake carries no ask, so `asked` routes to nobody: %+v", wake)
	}
	if !slices.ContainsFunc(tracker.Candidates(wake, false),
		func(c tracker.Candidate) bool {
			return c.Handle == "pm" && c.Reason == tracker.ReasonAsked && c.Addressed
		}) {
		t.Errorf("the asked party is not an ADDRESSED candidate: %+v",
			tracker.Candidates(wake, false))
	}
	// AND AN UNKNOWN HANDLE IS REFUSED, because an ask addressed to a
	// spelling wakes nobody and leaves a question open on the board for
	// ever.
	if got := callWork(t, reg, builtin.CommentOnWorkTool, map[string]any{
		"item": "ENG-1", "body": "?", "ask": "nobody",
	}); !got.Failed {
		t.Error("an ask to a handle the company does not have was accepted")
	}
}

// THE WARNING NAMES WHAT THE CALLER CANNOT SEE: a comment that asks nobody
// still wakes the assignee, unaddressed, which a turn may absorb in silence.
func TestACommentThatAsksNobodyWarnsAboutTheAssignee(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	detail := trk.tasks["ENG-1"]
	detail.Task.Assignee = "pm"
	trk.tasks["ENG-1"] = detail
	reg := workRegistry(t, builtin.WorkDeps{
		Reader: trk, Writer: trk.as, Seats: askable,
	})
	got := callWork(t, reg, builtin.CommentOnWorkTool, map[string]any{
		"item": "ENG-1", "body": "noting this for later",
	})
	if got.Failed {
		t.Fatalf("the comment failed: %s", got.Output)
	}
	var answer map[string]any
	if err := json.Unmarshal([]byte(got.Output), &answer); err != nil {
		t.Fatalf("the answer is not json: %v", err)
	}
	warnings, _ := answer["warnings"].([]any)
	if len(warnings) == 0 {
		t.Fatal("a comment that asks nobody wakes the assignee unaddressed " +
			"and says nothing about it — a commenter expecting an answer gets " +
			"silence with nothing to explain it")
	}
	if text, _ := warnings[0].(string); !strings.Contains(text, "ask") {
		t.Errorf("the warning is %q and does not name the remedy", warnings[0])
	}
	// AND AN ASK SILENCES IT, because then somebody IS being asked.
	trk.patched = nil
	if got := callWork(t, reg, builtin.CommentOnWorkTool, map[string]any{
		"item": "ENG-1", "body": "?", "ask": "pm",
	}); strings.Contains(got.Output, "warnings") {
		t.Errorf("a comment that asks the assignee still warns: %s", got.Output)
	}
}

// askable is the roster an `ask` resolves against: a tool refuses a handle the
// company does not have, because an ask addressed to a spelling wakes nobody
// and leaves a question open on the board for ever.
func askable() []colleague.Seat {
	return []colleague.Seat{
		{Handle: "pm", Name: "Pat Manager", Kind: "agent"},
		{Handle: "eng", Name: "Evan Engineer", Kind: "agent"},
	}
}

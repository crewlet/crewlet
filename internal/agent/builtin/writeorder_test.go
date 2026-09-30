package builtin_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A CALL IS DECIDED WHOLE BEFORE ITS FIRST WRITE.
//
// Declaring a write's missing labels is that call's first append, on the
// project's own subject. It ran ahead of the re-route's authority, the
// relations' and the blockers' resolution — so a call refused for any of those
// had already put its labels on the project's tag set, under a refusal telling
// the caller nothing had been written.
func TestARefusedCallDeclaresNoLabelFirst(t *testing.T) {
	t.Parallel()
	labels := map[string]any{"labels": []any{"regression"}, "labels_create_missing": true}
	with := func(args map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range labels {
			out[k] = v
		}
		for k, v := range args {
			out[k] = v
		}
		return out
	}
	for name, c := range map[string]struct {
		tool string
		args map[string]any
	}{
		// THE ONE THAT IS AN AUTHORITY: a colleague leading nothing.
		"an update re-routing work it may not": {builtin.UpdateWorkItemTool,
			with(map[string]any{"item": "ENG-1", "routing_unit": "plat"})},
		"an update linking an item nobody filed": {builtin.UpdateWorkItemTool,
			with(map[string]any{"item": "ENG-1",
				"linked": map[string]any{"add": []any{"ENG-404"}}})},
		"an update waiting on an item nobody filed": {builtin.UpdateWorkItemTool,
			with(map[string]any{"item": "ENG-1",
				"waiting_on": map[string]any{"add": []any{"ENG-404"}}})},
		"a create waiting on an item nobody filed": {builtin.CreateWorkItemTool,
			with(map[string]any{"title": "x", "project": "ENG",
				"waiting_on": []any{"ENG-404"}})},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			trk := newFakeTracker()
			got := callWorkAs(colleagueCaller(), t,
				projectRegistry(t, trk, chartRefuses), c.tool, c.args)
			if !got.Failed {
				t.Fatalf("the call was not refused: %s", got.Output)
			}
			if len(trk.ensured) != 0 || len(trk.opIDs) != 0 {
				t.Errorf("a refused call declared %v first (writes %v): %s",
					trk.ensured, trk.opIDs, got.Output)
			}
		})
	}

	// THE CONTROL: the same labels on a call nothing refuses are declared,
	// and before the write that uses them.
	trk := newFakeTracker()
	got := callWorkAs(colleagueCaller(), t, projectRegistry(t, trk, chartRefuses),
		builtin.UpdateWorkItemTool, with(map[string]any{"item": "ENG-1"}))
	if got.Failed || len(trk.ensured) != 1 || len(trk.patched) != 1 {
		t.Errorf("an admissible update declared %v and patched %d times: %s",
			trk.ensured, len(trk.patched), got.Output)
	}
}

// AN UPDATE WHOSE FIELDS LANDED SAYS SO when its dependencies did not.
//
// The call is two writes, and a refusal of the second was answered with the
// default failure — "the change was NOT made" — about fields the first had
// already set. A create has always answered this case with the item it filed;
// an update answers with the change it made.
func TestAnUpdateWhoseDependenciesFailedReportsTheChangeItMade(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	trk.dependErr = errors.New("tracker: ENG-2 already waits on the most items one may")
	reg := workRegistry(t, builtin.WorkDeps{
		Reader: trk, Writer: trk.as, Dependencies: trk.depends,
	})
	got := callWork(t, reg, builtin.UpdateWorkItemTool, map[string]any{
		"item": "ENG-1", "priority": "urgent",
		"waiting_on": map[string]any{"add": []any{"ENG-2"}},
	})
	if got.Failed {
		t.Fatalf("an update whose fields landed was reported as failed: %s",
			got.Output)
	}
	answer := answerOf(t, got)
	if answer["outcome"] != string(statelog.OutcomeApplied) {
		t.Errorf("the answer's outcome is %v, want the patch's own", answer["outcome"])
	}
	failure, _ := answer["dependencies_failed"].(string)
	for _, want := range []string{"WAS made", "ENG-1", "waiting_on"} {
		if !strings.Contains(failure, want) {
			t.Errorf("dependencies_failed lacks %q: %q", want, failure)
		}
	}
	if strings.Contains(failure, "NOT made") {
		t.Errorf("dependencies_failed says the change was not made: %q", failure)
	}

	// AND A CALL THAT WAS ONLY THE DEPENDENCY STILL FAILS: nothing else
	// landed, so the failure is the whole of the answer.
	only := newFakeTracker()
	only.dependErr = trk.dependErr
	got = callWork(t, workRegistry(t, builtin.WorkDeps{
		Reader: only, Writer: only.as, Dependencies: only.depends,
	}), builtin.UpdateWorkItemTool, map[string]any{
		"item": "ENG-1", "waiting_on": map[string]any{"add": []any{"ENG-2"}},
	})
	if !got.Failed || len(only.patched) != 0 {
		t.Errorf("a dependency-only update that failed answered %q", got.Output)
	}
}

// A DEPENDENCY THIS BUILD CANNOT RECORD IS REFUSED BEFORE THE PATCH, not after
// it: refused after, the fields had landed and the answer said the tracker was
// not configured.
func TestAnUnrecordableDependencyRefusesBeforeAnyWrite(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	got := callWork(t, workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as}),
		builtin.UpdateWorkItemTool, map[string]any{
			"item": "ENG-1", "priority": "urgent",
			"waiting_on": map[string]any{"add": []any{"ENG-2"}},
		})
	if !got.Failed || len(trk.patched) != 0 {
		t.Errorf("an update with no dependency writer patched %d times and "+
			"answered %q", len(trk.patched), got.Output)
	}
}

// A REFERENCE THIS NODE COULD NOT READ IS NOT A REFERENCE TO NOTHING.
//
// Every argument naming another item — a parent, a blocker, a link, the
// question a comment answers — is read before anything is written, and a read
// that failed came back as a sentence alone. The HTTP write surface reads its
// status from a result's CAUSE, so a node that could not read its own tracker
// answered 422 "refused" rather than 503, and a client never came back. The
// read's error is the cause now; and an item that plainly does not exist
// still carries none of the tracker's not-found errors, because it is not the
// object the call is about — the call is what is refused.
func TestAnUnreadableReferenceCarriesTheReadsError(t *testing.T) {
	t.Parallel()
	down := fmt.Errorf("the tracker could not be read: %w", statelog.ErrUnavailable)
	for name, c := range map[string]struct {
		tool  string
		args  map[string]any
		fault func(*fakeTracker)
	}{
		"a create's parent": {builtin.CreateWorkItemTool,
			map[string]any{"title": "x", "project": "ENG", "parent": "ENG-2"},
			func(f *fakeTracker) { f.readErr = down }},
		"a create's blocker": {builtin.CreateWorkItemTool,
			map[string]any{"title": "x", "project": "ENG", "waiting_on": []any{"ENG-2"}},
			func(f *fakeTracker) { f.readErr = down }},
		"the question a comment answers": {builtin.CommentOnWorkTool,
			map[string]any{"item": "ENG-1", "body": "yes", "answers": "c1"},
			func(f *fakeTracker) { f.threadErr = down }},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			trk := newFakeTracker()
			c.fault(trk)
			got := callWork(t, workRegistry(t, builtin.WorkDeps{
				Reader: trk, Writer: trk.as, Dependencies: trk.depends,
			}), c.tool, c.args)
			if !got.Failed || !errors.Is(got.Cause, statelog.ErrUnavailable) {
				t.Errorf("an unreadable reference answered %q with cause %v, "+
					"want the read's own error", got.Output, got.Cause)
			}
			if len(trk.created)+len(trk.patched) != 0 {
				t.Errorf("an unreadable reference still wrote")
			}
		})
	}

	// THE CONTROL: a reference to nothing is refused, and its cause is not
	// the tracker's not-found — which a caller answering in status codes
	// reads as the route's own object being missing.
	trk := newFakeTracker()
	got := callWork(t, workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as}),
		builtin.CreateWorkItemTool,
		map[string]any{"title": "x", "project": "ENG", "parent": "ENG-404"})
	if !got.Failed || errors.Is(got.Cause, tracker.ErrNoTask) {
		t.Errorf("a parent nobody filed answered %q with cause %v", got.Output,
			got.Cause)
	}
}

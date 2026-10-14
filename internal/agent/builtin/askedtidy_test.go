package builtin_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// ---- answering what you were asked, without the tracker ----------------- //

// askedTracker is the tracker's read side with the one fact these cases turn
// on made real: an OPEN ask on a task is addressed to one seat, and a thread
// read authored by that seat INFERS it, exactly as the reader's own inference
// does — while an `answers` naming an ask somebody else was asked is refused
// as the reader refuses it. Everything else is the shared fake's.
type askedTracker struct {
	*fakeTracker

	// open is each task's open asks, by the seat each is addressed to.
	open map[string]map[string]string

	// down, when set, is what every thread read fails with — a node that
	// cannot read the item's asks right now.
	down error
}

func (a *askedTracker) Thread(_ context.Context, q tracker.ThreadQuery,
	_ statelog.Freshness) (tracker.ResolvedThread, error) {

	a.threadQuery = q
	if a.down != nil {
		return tracker.ResolvedThread{}, a.down
	}
	out := tracker.ResolvedThread{ThreadParties: tracker.ThreadParties{Asked: q.Ask},
		Answers: q.Answers}
	asks := a.open[q.Task]
	if out.Answers == "" && q.Author != "" {
		out.Answers = asks[q.Author]
	}
	if out.Answers != "" && asks[q.Author] != out.Answers {
		return tracker.ResolvedThread{}, fmt.Errorf("%w: comment %s asked "+
			"somebody else", tracker.ErrForbidden, out.Answers)
	}
	return out, nil
}

// askedRig is the operator surface over a tracker on which ENG-1 carries an
// open ask addressed to the seat `legal` and ENG-2 carries none, decided by the
// real table over a chart in which nobody leads anybody — so whatever admits a
// caller here is the app or the ask.
func askedRig(t *testing.T) (map[string]tools.Callable, *fakeTracker) {
	t.Helper()
	return askedRigDown(t, nil)
}

// askedRigDown is [askedRig] over a tracker whose thread reads fail with down.
func askedRigDown(t *testing.T, down error) (map[string]tools.Callable, *fakeTracker) {
	t.Helper()
	trk := newFakeTracker()
	reader := &askedTracker{fakeTracker: trk, down: down,
		open: map[string]map[string]string{"i1": {"legal": "c-ask"}}}
	out := map[string]tools.Callable{}
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
		Work: builtin.WorkDeps{
			Reader: reader, Writer: trk.as, Actor: builtin.PrincipalActor,
		},
		Authorize: builtin.Decide(chartRefuses),
	}) {
		out[tool.Name()] = tool
	}
	return out, trk
}

// callAs calls one of the rig's tools as a principal, outside any turn.
func callAs(t *testing.T, tool tools.Callable, p iam.Principal,
	args map[string]any) tools.Result {

	t.Helper()
	got, err := tool.Call(iam.WithPrincipal(context.Background(), p), args)
	if err != nil {
		t.Fatalf("%s: %v", tool.Name(), err)
	}
	return got
}

// refusedForTracker reports whether a result is the table's own refusal naming
// the tracker as what would admit — the one refusal a caller kept off the board
// is ever told.
func refusedForTracker(got tools.Result) bool {
	var refused *builtin.DecisionRefused
	return got.Failed && errors.As(got.Cause, &refused) &&
		slices.Equal(refused.Decision.Grants, []iam.Grant{iam.GrantTracker})
}

// SOMEBODY WITHOUT THE TRACKER READS THE ITEM THEY WERE ASKED ON, AND ANSWERS.
//
// A company that keeps somebody off its board still asks them things, and the
// question is filed on a work item: refused the item they could not read what
// they were asked, and refused its thread they could not answer — so the ask
// sat open for ever. The asked arm admits exactly the person an OPEN ask names,
// to the read and to the answer; their comment closes the ask it answers, and
// it does not make them follow a board they do not use. The control is the
// same person on an item nobody asked them on.
func TestAnAskedPersonReadsAndAnswersWithoutTracker(t *testing.T) {
	t.Parallel()
	surface, trk := askedRig(t)
	legal := boundTo("legal")

	read := callAs(t, surface[tracker.GetWorkItemTool], legal, map[string]any{"item": "ENG-1"})
	if read.Failed || !strings.Contains(read.Output, `"ENG-1"`) {
		t.Fatalf("the person asked on ENG-1 could not read it: %s", read.Output)
	}
	answered := callAs(t, surface[tracker.CommentOnWorkTool], legal,
		map[string]any{"item": "ENG-1", "body": "Yes — the clause stands."})
	if answered.Failed {
		t.Fatalf("the person asked on ENG-1 could not answer: %s", answered.Output)
	}
	if len(trk.patched) != 1 {
		t.Fatalf("the answer wrote %d patches, want one", len(trk.patched))
	}
	posted := trk.patched[0]
	if posted.Comment == nil || posted.Comment.Answers == nil ||
		*posted.Comment.Answers != "c-ask" {
		t.Errorf("the comment answers %v, want the open ask c-ask", posted.Comment)
	}
	if posted.Watch != nil {
		t.Errorf("answering made the person follow the item (%+v), which would "+
			"wake them on a board they do not use", posted.Watch)
	}

	// THE CONTROL: nobody asked them anything on ENG-2.
	if got := callAs(t, surface[tracker.GetWorkItemTool], legal,
		map[string]any{"item": "ENG-2"}); !refusedForTracker(got) {
		t.Errorf("the person read ENG-2, which no ask names them on: %+v", got)
	}
}

// ADMITTED ONLY TO ANSWER, AND ONLY THE ANSWER.
//
// The arm admits the answer to an open ask and nothing else: a question of
// their own (`ask`, or a structured `decision`) is the tracker's to put, and a
// remark on an item nobody asked them on is a comment like any other. Each is
// refused with the table's own refusal naming the tracker, before anything is
// written — and a tracker holder making the same call is not refused, so the
// rule is about the app and not about the call.
func TestAnAskedPersonMayOnlyAnswer(t *testing.T) {
	t.Parallel()
	for name, args := range map[string]map[string]any{
		"a question of their own": {"item": "ENG-1", "body": "Who signs off?",
			"ask": "agent-cto"},
		"a decision of their own": {"item": "ENG-1", "body": "Pick one",
			"ask": "agent-cto", "decision": map[string]any{
				"question": "Which?", "options": []any{
					map[string]any{"id": "a", "label": "A"},
					map[string]any{"id": "b", "label": "B"},
				}}},
		"a remark nobody asked for": {"item": "ENG-2", "body": "fyi"},
		"an answer to somebody else's ask": {"item": "ENG-1", "body": "no",
			"answers": "c-other"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			surface, trk := askedRig(t)
			got := callAs(t, surface[tracker.CommentOnWorkTool], boundTo("legal"), args)
			if !got.Failed {
				t.Fatalf("admitted: %s", got.Output)
			}
			if name != "an answer to somebody else's ask" && !refusedForTracker(got) {
				t.Errorf("refused with %v, want the table's refusal naming the "+
					"tracker: %s", got.Cause, got.Output)
			}
			if len(trk.patched) != 0 {
				t.Errorf("a refused comment wrote %d patches", len(trk.patched))
			}
		})
	}

	// THE CONTROL: a tracker holder asks a question on the same item.
	surface, trk := askedRig(t)
	if got := callAs(t, surface[tracker.CommentOnWorkTool], boundTo("pm", iam.GrantTracker),
		map[string]any{"item": "ENG-1", "body": "Who signs off?", "ask": "agent-cto"}); got.Failed {
		t.Fatalf("a tracker holder was refused a question: %s", got.Output)
	}
	if len(trk.patched) != 1 || trk.patched[0].Watch == nil {
		t.Errorf("a tracker holder's comment did not follow the item as ever: %+v",
			trk.patched)
	}
}

// AN ITEM THAT IS NOT THERE AND ONE YOU WERE NOT ASKED ON ARE ONE REFUSAL.
//
// Told apart — "there is no ENG-404" against "you may not read ENG-2" — a
// caller kept off the board could learn which keys exist one guess at a time.
// So both are the table's refusal naming the tracker, in the same words, on
// the read and on the thread alike. A tracker holder is told the truth about
// a missing item, because nothing on the board is hidden from them.
func TestAMissingItemAndAnItemYouWereNotAskedOnAreOneRefusal(t *testing.T) {
	t.Parallel()
	surface, trk := askedRig(t)
	legal := boundTo("legal")
	for tool, extra := range map[string]map[string]any{
		tracker.GetWorkItemTool: {}, tracker.CommentOnWorkTool: {"body": "x"},
	} {
		args := func(item string) map[string]any {
			out := map[string]any{"item": item}
			for k, v := range extra {
				out[k] = v
			}
			return out
		}
		missing := callAs(t, surface[tool], legal, args("ENG-404"))
		notAsked := callAs(t, surface[tool], legal, args("ENG-2"))
		if !refusedForTracker(missing) || !refusedForTracker(notAsked) {
			t.Fatalf("%s: missing = %+v, not asked = %+v — want the table's refusal "+
				"naming the tracker for both", tool, missing, notAsked)
		}
		if missing.Output != notAsked.Output {
			t.Errorf("%s told a missing item from one nobody asked them on:\n%s\n%s",
				tool, missing.Output, notAsked.Output)
		}
	}
	if len(trk.patched) != 0 {
		t.Errorf("a refused caller wrote %d patches", len(trk.patched))
	}
	holder := callAs(t, surface[tracker.GetWorkItemTool], boundTo("pm", iam.GrantTracker),
		map[string]any{"item": "ENG-404"})
	if !holder.Failed || tools.RefusalOf(holder) != tools.RefusalNotFound {
		t.Errorf("a tracker holder asking for a missing item = %+v, want not found", holder)
	}
}

// A PROBE THAT CANNOT READ HAS FOUND NOTHING EITHER WAY.
//
// Whether an open ask names the caller is a read, and a node that cannot make
// it has not learned that nobody asked them anything: answered with the
// table's refusal naming the tracker, the person who WAS asked would be sent
// to go and get access they do not need, for a question still waiting on
// them. So the read and the answer alike say the tracker could not be read —
// the condition, which a retry clears — and nothing is written; a tracker
// holder, who needs no probe, is not stopped by it at all.
func TestAProbeThatCannotReadHasFoundNothingEitherWay(t *testing.T) {
	t.Parallel()
	down := fmt.Errorf("the item's thread could not be read: %w", statelog.ErrUnavailable)
	surface, trk := askedRigDown(t, down)
	for tool, args := range map[string]map[string]any{
		tracker.GetWorkItemTool:   {"item": "ENG-1"},
		tracker.CommentOnWorkTool: {"item": "ENG-1", "body": "Yes — the clause stands."},
	} {
		got := callAs(t, surface[tool], boundTo("legal"), args)
		if !got.Failed || refusedForTracker(got) ||
			tools.RefusalOf(got) != tools.RefusalUnavailable ||
			!errors.Is(got.Cause, statelog.ErrUnavailable) {
			t.Errorf("%s by the person asked, on a node that cannot read the "+
				"asks = %+v, want the tracker's own condition and no refusal "+
				"naming the tracker", tool, got)
		}
	}
	if len(trk.patched) != 0 {
		t.Errorf("a call whose probe could not read wrote %d patches", len(trk.patched))
	}
	if got := callAs(t, surface[tracker.GetWorkItemTool], boundTo("pm", iam.GrantTracker),
		map[string]any{"item": "ENG-1"}); got.Failed {
		t.Errorf("a tracker holder, who needs no probe, was stopped by one: %s", got.Output)
	}
}

// ---- authors tidy their own work --------------------------------------- //

// tidyRig is the trash on the operator surface over a tracker whose ENG-1 was
// reported by the seat `ana`, decided by the real table over chart.
func tidyRig(t *testing.T, chart authz.Chart) (map[string]tools.Callable, *trashSpy) {
	t.Helper()
	trk := newFakeTracker()
	item := trk.tasks["ENG-1"]
	item.Task.Reporter = "ana"
	trk.tasks["ENG-1"] = item
	spy := &trashSpy{}
	out := map[string]tools.Callable{}
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
		Work: builtin.WorkDeps{
			Reader: trk, Writer: trk.as, Actor: builtin.PrincipalActor,
			TrashWriter: func(builtin.Actor) builtin.TrashWriter { return spy },
		},
		Authorize: builtin.Decide(chart),
	}) {
		out[tool.Name()] = tool
	}
	return out, spy
}

// WHOEVER FILED AN ITEM PUTS IT IN THE TRASH AND TAKES IT OUT AGAIN.
//
// The trash was the project lead's and an admin's alone, so somebody who filed
// an item by mistake could not take it back off the board. The reporter is
// admitted on both verbs — with the tracker, since every arm of a tracker verb
// needs it — and nobody else who leads nothing is; and a reporter's restore
// asks for the item ALONE, so what went in with it at somebody else's hand
// stays where they put it.
func TestAReporterRemovesAndRestoresTheirOwnItem(t *testing.T) {
	t.Parallel()
	surface, spy := tidyRig(t, chartRefuses)
	ana := boundTo("ana", iam.GrantTracker)
	for _, tool := range []string{tracker.RemoveWorkItemTool, tracker.RestoreWorkItemTool} {
		if got := callAs(t, surface[tool], ana, map[string]any{"item": "ENG-1"}); got.Failed {
			t.Fatalf("the reporter was refused %s: %s", tool, got.Output)
		}
	}
	if !slices.Equal(spy.removed, []string{"i1"}) || !slices.Equal(spy.restored, []string{"i1"}) {
		t.Fatalf("removed %v and restored %v, want the reporter's own item", spy.removed,
			spy.restored)
	}
	if !spy.alone {
		t.Error("the reporter's restore did not ask for the item alone, so it would " +
			"bring back what a lead took out with it")
	}

	for name, caller := range map[string]iam.Principal{
		"a colleague who did not file it":  boundTo("sam", iam.GrantTracker),
		"the reporter without the tracker": boundTo("ana", iam.GrantKnowledge),
	} {
		for _, tool := range []string{tracker.RemoveWorkItemTool, tracker.RestoreWorkItemTool} {
			got := callAs(t, surface[tool], caller, map[string]any{"item": "ENG-1"})
			if !got.Failed || !errors.Is(got.Cause, builtin.ErrRefused) {
				t.Errorf("%s was admitted to %s: %s", name, tool, got.Output)
			}
		}
	}

	// A LEAD RESTORES THE WHOLE GESTURE, as ever.
	leads, spy := tidyRig(t, chartLeads)
	if got := callAs(t, leads[tracker.RestoreWorkItemTool], boundTo("sam", iam.GrantTracker),
		map[string]any{"item": "ENG-1"}); got.Failed || spy.alone {
		t.Errorf("a project lead's restore = %+v, alone %v — want the whole gesture",
			got, spy.alone)
	}
}

// AN AUTHOR TIDIES THEIR OWN ITEM AND NOTHING BELOW IT.
//
// The items under it may be anybody's, so `subtree: true` from somebody
// admitted only as the reporter is refused — naming the project lead or an
// admin — with nothing removed; and a restore that would bring back what went
// with it is refused the same way when the tracker says the removal took
// others. The project's lead does both, and a node that cannot read the
// chart says so rather than guess.
func TestAnAuthorMayNotRemoveASubtree(t *testing.T) {
	t.Parallel()
	surface, spy := tidyRig(t, chartRefuses)
	ana := boundTo("ana", iam.GrantTracker)
	got := callAs(t, surface[tracker.RemoveWorkItemTool], ana,
		map[string]any{"item": "ENG-1", "subtree": true})
	var refused *builtin.DecisionRefused
	if !got.Failed || !errors.As(got.Cause, &refused) ||
		!slices.Equal(refused.Decision.Grants, []iam.Grant{iam.GrantAdmin}) {
		t.Fatalf("the reporter's subtree removal = %+v, want the table's refusal "+
			"naming admin", got)
	}
	if !strings.Contains(got.Output, "project lead's or an admin's") ||
		!strings.Contains(got.Output, "Leave `subtree` out") {
		t.Errorf("the refusal does not say whose the items below are and what to "+
			"do instead: %s", got.Output)
	}
	if len(spy.removed) != 0 {
		t.Errorf("a refused subtree removal removed %v", spy.removed)
	}

	spy.answers = []error{fmt.Errorf("tracker: 2 other tasks went into the trash "+
		"with task i1: %w", tracker.ErrRestoresOthers)}
	got = callAs(t, surface[tracker.RestoreWorkItemTool], ana, map[string]any{"item": "ENG-1"})
	if !got.Failed || tools.RefusalOf(got) != tools.RefusalForbidden ||
		!errors.Is(got.Cause, builtin.ErrRefused) ||
		!strings.Contains(got.Output, "2 other tasks") {
		t.Errorf("a reporter's restore that would bring back others = %+v, want a "+
			"refusal saying what went with it", got)
	}

	leads, spy := tidyRig(t, chartLeads)
	if got := callAs(t, leads[tracker.RemoveWorkItemTool], boundTo("sam", iam.GrantTracker),
		map[string]any{"item": "ENG-1", "subtree": true}); got.Failed || !spy.subtree {
		t.Errorf("the project's lead could not remove the subtree: %+v", got)
	}

	// A NODE THAT CANNOT TELL WHO LEADS THE PROJECT SAYS SO, and neither
	// gesture goes ahead: the reporter's own arm admits without the chart,
	// but whether the whole gesture is theirs is the chart's question, and
	// telling them to drop `subtree`, or restoring their item alone, would
	// be guessing they lead nothing.
	blind, spy := tidyRig(t, chartUnknown)
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{tracker.RemoveWorkItemTool, map[string]any{"item": "ENG-1", "subtree": true}},
		{tracker.RestoreWorkItemTool, map[string]any{"item": "ENG-1"}},
	} {
		got := callAs(t, blind[c.tool], ana, c.args)
		if !got.Failed || tools.RefusalOf(got) != tools.RefusalUnavailable ||
			strings.Contains(got.Output, "Whoever filed an item") {
			t.Errorf("%s on a node that cannot read the chart = %+v, want the "+
				"table's undecidable answer and nothing about authors", c.tool, got)
		}
	}
	if len(spy.removed) != 0 || len(spy.restored) != 0 {
		t.Errorf("an undecidable tidy wrote: removed %v, restored %v",
			spy.removed, spy.restored)
	}
}

// WHOEVER CREATED A PAGE RENAMES, TRASHES AND RESTORES IT.
//
// Tidying a page was its container lead's and an admin's alone. The creator is
// admitted to all three — the rename a `title` on save_page asks for, and the
// trash and restore the HTTP surface decides on the same object
// ([builtin.PageObject]) — and a colleague who neither created it nor leads
// its space is not.
func TestAPageCreatorRenamesTrashesAndRestoresIt(t *testing.T) {
	t.Parallel()
	kb := newFakeKB()
	kb.page.Page.Author = "tester"
	reg := tools.NewRegistry()
	if _, err := builtin.Register(reg, builtin.Deps{
		Pages:     builtin.PageDeps{Reader: kb, Writer: kb},
		Authorize: builtin.Decide(chartRefuses),
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	creator := callWorkAs(colleagueCaller(), t, reg, builtin.SavePageTool,
		map[string]any{"page": "p1", "base_version": 4, "title": "Deploy Runbook v2"})
	if creator.Failed || len(kb.renames) != 1 {
		t.Fatalf("the page's creator could not rename it: %s", creator.Output)
	}

	kb.page.Page.Author = "somebody-else"
	if other := callWorkAs(colleagueCaller(), t, reg, builtin.SavePageTool,
		map[string]any{"page": "p1", "base_version": 4, "title": "Deploy Runbook v3"}); !other.Failed {
		t.Fatalf("a colleague who neither created the page nor leads its space "+
			"renamed it: %s", other.Output)
	}

	page := pages.Page{ID: "p1", Container: "ENG", Author: "ana"}
	for _, action := range []authz.Action{authz.ActionPageRename, authz.ActionPageTrash,
		authz.ActionPageRestore} {
		for name, c := range map[string]struct {
			caller iam.Principal
			want   bool
		}{
			"the creator":                            {boundTo("ana", iam.GrantKnowledge), true},
			"somebody else":                          {boundTo("sam", iam.GrantKnowledge), false},
			"the creator without the knowledge base": {boundTo("ana", iam.GrantTracker), false},
		} {
			d := authz.Decide(t.Context(), c.caller, action, builtin.PageObject(page),
				chartRefuses, time.Now())
			if d.Allowed != c.want {
				t.Errorf("%s on %s = %v (%s), want %v", name, action, d.Allowed,
					d.Reason, c.want)
			}
		}
	}
}

package builtin_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// decisionRegistry is the seat surface with a tracker and a knowledge base, so
// a decision's task and page evidence both have something to be checked
// against.
func decisionRegistry(t *testing.T, trk *fakeTracker, kb builtin.PageReader) *tools.Registry {
	t.Helper()
	reg := tools.NewRegistry()
	if _, err := builtin.Register(reg, builtin.Deps{
		Work:  builtin.WorkDeps{Reader: trk, Writer: trk.as},
		Pages: builtin.PageDeps{Reader: kb},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	return reg
}

// decisionArg is a well-formed `decision` argument, as a model sends it.
func decisionArg() map[string]any {
	return map[string]any{
		"question": "Ship on Friday or hold for the audit?",
		"options": []any{
			map[string]any{"id": "ship", "label": "Ship Friday"},
			map[string]any{"id": "hold", "label": "Hold for the audit", "detail": "about a week"},
		},
		"recommended": "ship",
		"rationale":   "The audit reviews last quarter's code.",
		"evidence": []any{
			map[string]any{"kind": "task", "ref": "ENG-2", "label": "the audit"},
			map[string]any{"kind": "page", "ref": "ENG/Deploy Runbook"},
		},
		"role": "approver",
	}
}

// A COMMENT CAN ASK WITH A DECISION, and what is stored is what every node
// will hold for ever: the task and the page the evidence names are resolved
// to their IDS — a key is retired by a move and a title by a rename — and a
// decision whose keys a model misspelled is refused by name rather than
// stored without the part it misspelled.
func TestACommentCanAskWithADecision(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := decisionRegistry(t, trk, newFakeKB())
	got := callWork(t, reg, builtin.CommentOnWorkTool, map[string]any{
		"item": "ENG-1", "body": "The audit starts Monday.", "ask": "pm",
		"decision": decisionArg(),
	})
	if got.Failed {
		t.Fatalf("the ask failed: %s", got.Output)
	}
	if len(trk.patched) != 1 || trk.patched[0].Comment == nil {
		t.Fatalf("the ask did not ride the task's write: %+v", trk.patched)
	}
	comment := trk.patched[0].Comment
	if comment.Ask != "pm" || comment.Decision == nil {
		t.Fatalf("the comment is %+v, want an ask carrying its decision", comment)
	}
	decision := comment.Decision
	if len(decision.Options) != 2 || decision.Recommended != "ship" ||
		decision.Role != tracker.RoleApprover {
		t.Errorf("the decision is %+v", decision)
	}
	if decision.Evidence[0].Ref != "id-2" || decision.Evidence[1].Ref != "p1" {
		t.Errorf("the evidence is stored as %+v, want the task's and the page's ids",
			decision.Evidence)
	}
	if !strings.Contains(got.Output, `"decision"`) {
		t.Errorf("the answer does not show the decision as stored: %s", got.Output)
	}

	misspelled := decisionArg()
	misspelled["recommendation"] = misspelled["recommended"]
	delete(misspelled, "recommended")
	refused := callWork(t, decisionRegistry(t, newFakeTracker(), newFakeKB()),
		builtin.CommentOnWorkTool, map[string]any{
			"item": "ENG-1", "body": "?", "ask": "pm", "decision": misspelled,
		})
	if !refused.Failed || !strings.Contains(refused.Output, "recommendation") {
		t.Errorf("a misspelled key was not refused by name: %s", refused.Output)
	}
}

// A DECISION WITHOUT AN ASK IS REFUSED, on both tools, before anything is
// read or written: a decision is a question put to somebody, and one asked of
// nobody is a set of options nobody is woken to weigh. And a page cited on a
// company with no knowledge base of its own is refused as the caller's to
// change — cite it as a url — rather than stored as a link nothing checked.
func TestADecisionWithoutAnAskIsRefused(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := decisionRegistry(t, trk, newFakeKB())
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{builtin.CommentOnWorkTool, map[string]any{
			"item": "ENG-1", "body": "which?", "decision": decisionArg()}},
		{builtin.CreateWorkItemTool, map[string]any{
			"title": "which?", "project": "ENG", "decision": decisionArg()}},
	} {
		got := callWork(t, reg, c.tool, c.args)
		if !got.Failed || got.Refusal != tools.RefusalInvalid ||
			!strings.Contains(got.Output, "`ask`") {
			t.Errorf("%s: a decision asked of nobody gave %+v", c.tool, got)
		}
	}
	if len(trk.patched) != 0 || len(trk.created) != 0 {
		t.Errorf("a refused decision was written: %d patches, %d creates",
			len(trk.patched), len(trk.created))
	}

	noKB := newFakeTracker()
	got := callWork(t, decisionRegistry(t, noKB, nil), builtin.CommentOnWorkTool,
		map[string]any{"item": "ENG-1", "body": "?", "ask": "pm", "decision": decisionArg()})
	if !got.Failed || got.Refusal != tools.RefusalInvalid || !strings.Contains(got.Output, "url") {
		t.Errorf("a page cited with no knowledge base gave %+v", got)
	}
	if len(noKB.patched) != 0 {
		t.Error("a decision citing an unchecked page was written")
	}
}

// A CHOICE WITH NO BODY IS AN ANSWER: the option is what the asker was
// waiting for, and a reason is welcome but not owed. The choice rides the
// thread read — so the reader checks it against the ask's own options before
// anything is published — and the comment. A choice that answers nothing is
// refused, since there is no decision for it to be an option of.
func TestAChoiceWithNoBodyIsAnAnswer(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := decisionRegistry(t, trk, newFakeKB())
	got := callWork(t, reg, builtin.CommentOnWorkTool, map[string]any{
		"item": "ENG-1", "answers": "c-ask", "choice": "ship",
	})
	if got.Failed {
		t.Fatalf("a bare choice was refused: %s", got.Output)
	}
	if trk.threadQuery.Choice != "ship" {
		t.Errorf("the thread read was asked about choice %q, want it checked there",
			trk.threadQuery.Choice)
	}
	comment := trk.patched[0].Comment
	if comment.Choice != "ship" || comment.Answers == nil || *comment.Answers != "c-ask" ||
		comment.Body != "" {
		t.Errorf("the answer is %+v, want the choice answering c-ask with no body", comment)
	}

	loose := newFakeTracker()
	got = callWork(t, decisionRegistry(t, loose, newFakeKB()), builtin.CommentOnWorkTool,
		map[string]any{"item": "ENG-1", "choice": "ship"})
	if !got.Failed || !strings.Contains(got.Output, "`answers`") {
		t.Errorf("a choice answering nothing gave %+v", got)
	}
	if len(loose.patched) != 0 {
		t.Error("a choice answering nothing was posted")
	}
	if got := callWork(t, reg, builtin.CommentOnWorkTool, map[string]any{
		"item": "ENG-1",
	}); !got.Failed || !strings.Contains(got.Output, "`body`") {
		t.Errorf("a comment with neither a body nor a choice gave %+v", got)
	}
}

// A CREATE CAN CARRY ITS ASK IN ONE RECORD. "Ask" and "Message" file an item
// whose point is the question on it, so the question is the item's TITLE and
// it rides the create itself — the writer's one-record verb — and the wake it
// builds names the person asked, which is what routes them under `asked`.
func TestACreateCanCarryItsAskInOneRecord(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := decisionRegistry(t, trk, newFakeKB())
	got := callWork(t, reg, builtin.CreateWorkItemTool, map[string]any{
		"title": "Ship Friday or hold for the audit?", "project": "ENG",
		"body": "The audit starts Monday.", "assignee": "pm", "ask": "pm",
		"decision": decisionArg(),
	})
	if got.Failed {
		t.Fatalf("filing a question failed: %s", got.Output)
	}
	if len(trk.created) != 1 || len(trk.asks) != 1 {
		t.Fatalf("the create wrote %d tasks and %d asks, want one of each in "+
			"one call", len(trk.created), len(trk.asks))
	}
	ask := trk.asks[0]
	if ask.Ask != "pm" || ask.Body != trk.created[0].Title || ask.Decision == nil ||
		ask.Decision.Evidence[0].Ref != "id-2" {
		t.Errorf("the ask is %+v, want pm asked the title with the checked decision", ask)
	}
	notify := trk.notified[0]
	if notify == nil || notify.Snapshot.CommentAsk != "pm" || notify.CommentID != ask.ID {
		t.Errorf("the wake is %+v, want it to ask pm on comment %s", notify, ask.ID)
	}
	var answer map[string]any
	if err := json.Unmarshal([]byte(got.Output), &answer); err != nil {
		t.Fatalf("decode the answer: %v", err)
	}
	if answer["comment_id"] != ask.ID || answer["asked"] != "pm" {
		t.Errorf("the answer is %v, want the ask's comment id and who was asked", answer)
	}

	// AND WITHOUT `ask`, the plain create it always was.
	plain := newFakeTracker()
	if got := callWork(t, decisionRegistry(t, plain, newFakeKB()), builtin.CreateWorkItemTool,
		map[string]any{"title": "wire it", "project": "ENG"}); got.Failed {
		t.Fatalf("a plain create failed: %s", got.Output)
	}
	if len(plain.asks) != 0 || len(plain.created) != 1 {
		t.Errorf("a plain create carried an ask: %+v", plain.asks)
	}
}

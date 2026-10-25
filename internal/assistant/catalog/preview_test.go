package catalog_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/proposal"
	"github.com/crewlet/crewlet/internal/assistant/wire"
)

const uid = "5f0c5c8e-1a2b-4c3d-8e9f-0a1b2c3d4e5f"

// world is the company the previews read: the answers each route gives, by
// request path, in the routes' own shapes.
func world() map[string]any {
	return map[string]any{
		"/work/ENG-42": map[string]any{"task": map[string]any{"id": uid, "key": "ENG-42",
			"title": "Ship it", "status": "todo", "priority": "normal", "assignee": "pm",
			"body": "The old description.", "version": 3, "tags": []any{"release"}}},
		"/work/comments": map[string]any{"key": "ENG-42", "title": "Ship it",
			"comments": []any{map[string]any{"id": "c1", "ask": "pm", "body": "Which day?",
				"decision": map[string]any{"question": "Which day?", "options": []any{
					map[string]any{"id": "a", "label": "Monday"}}}}}},
		"/work/my-work":   map[string]any{"handle": "pm"},
		"/work/people/pm": map[string]any{"handle": "pm", "version": 7, "priorities": []any{"ENG-9"}},
		"/agents/pm": map[string]any{"handle": "pm", "role": "Product manager",
			"live": map[string]any{"activity": "working"}},
		"/sandbox-runs": map[string]any{"runs": []any{map[string]any{"turn_id": uid,
			"agent_handle": "dev", "work_key": "ENG-42", "question": "Which branch?"}}},
		"/pages/" + uid: map[string]any{"page": map[string]any{"id": uid, "title": "Runbook",
			"body": "Step one.", "version": 2}},
		"/artifacts/" + uid: map[string]any{"id": uid, "title": "Q3 deck", "head_number": 5},
	}
}

// reader is a turn grant over answers, refusing anything it does not hold.
func reader(answers map[string]any) proposal.Read {
	return func(_ context.Context, r exec.Request) (exec.Response, error) {
		body, ok := answers[r.Path]
		if !ok {
			return exec.Response{Status: 404,
				Body: []byte(`{"error":"not_found","message":"No such record."}`)}, nil
		}
		b, err := json.Marshal(body)
		return exec.Response{Status: 200, Body: b}, err
	}
}

// previewArgs are each proposing tool's call, against the world above.
var previewArgs = map[string]map[string]any{
	"create_task": {"title": "File the report", "assignee": "pm", "body": "Details."},
	"update_task": {"item": "ENG-42", "if_match": float64(3), "status": "in_progress",
		"assignee": "dev", "body": "A new description."},
	"comment_on_task":     {"item": "ENG-42", "body": "Done on my side."},
	"answer_decision":     {"item": "ENG-42", "choice": "a"},
	"set_my_priorities":   {"items": []any{"ENG-42", "ENG-9"}},
	"mark_inbox":          {"read": []any{"r1", "r2"}},
	"pause_agent":         {"handle": "pm", "reason": "Too many hand-offs.", "stop_running": true},
	"resume_agent":        {"handle": "pm"},
	"steer_turn":          {"turn_id": uid, "note": "Use the staging branch."},
	"answer_coding_run":   {"turn_id": uid, "answer": "main"},
	"create_page":         {"title": "Onboarding", "body": "Welcome."},
	"edit_page":           {"page": uid, "base_version": float64(2), "body": "Step one, then two."},
	"share_artifact":      {"artifact": uid, "to": "company", "role": "viewer"},
	"unshare_artifact":    {"artifact": uid, "from": "unit:platform"},
	"propose_page_change": {"action": "work.filter.apply", "args": map[string]any{"status": "blocked"}},
}

// EVERY WRITE IS PREVIEWED FROM THE OBJECT AS IT IS NOW, and its preview sends
// exactly the request the tool builds.
//
// Each proposing tool's preview reads what it changes through the turn's
// grant, answers a card the proposal package accepts (a title, an approve
// label, at least one change, an audience where visibility moves), binds the
// revision it read, and carries the request byte for byte. A share to the whole
// company asks a step-up (D09). Mutation: build the preview's request from
// other arguments than the tool's, or drop the share's audience, and this
// fails.
func TestEveryWriteIsPreviewedFromTheObjectAsItIsNow(t *testing.T) {
	t.Parallel()
	read := reader(world())
	for _, tool := range catalog.Tools() {
		if tool.Kind != catalog.KindWrite && tool.Kind != catalog.KindPageAction {
			continue
		}
		args, ok := previewArgs[tool.Name]
		if !ok {
			t.Errorf("%s proposes and this suite does not preview it", tool.Name)
			continue
		}
		p, err := tool.Preview(t.Context(), read, args)
		if err != nil {
			t.Errorf("%s: %v", tool.Name, err)
			continue
		}
		visibility := tool.Name == "share_artifact" || tool.Name == "unshare_artifact"
		if err := p.Check(visibility); err != nil {
			t.Errorf("%s's preview: %v", tool.Name, err)
		}
		want, err := tool.Request(args)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(p.Request, want) {
			t.Errorf("%s previews %+v and the tool sends %+v", tool.Name, p.Request, want)
		}
		if p.SourceRevision != "" {
			if _, err := proposal.ParseRevision(p.SourceRevision); err != nil {
				t.Errorf("%s binds %q: %v", tool.Name, p.SourceRevision, err)
			}
			now, err := catalog.Revision(t.Context(), read, p.SourceRevision)
			if err != nil || now != p.SourceRevision {
				t.Errorf("%s's revision %q re-reads as %q, %v", tool.Name, p.SourceRevision, now, err)
			}
		}
	}
	share, _ := catalog.Lookup("share_artifact")
	p, err := share.Preview(t.Context(), read, previewArgs["share_artifact"])
	if err != nil || !p.StepUp || len(p.Audience) != 1 {
		t.Errorf("a company-wide share previews step-up %v, audience %v, %v", p.StepUp, p.Audience, err)
	}
	update, _ := catalog.Lookup("update_task")
	p, err = update.Preview(t.Context(), read, previewArgs["update_task"])
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]wire.ChangeKind{}
	for _, c := range p.Changes {
		kinds[c.Field] = c.Kind
	}
	if kinds["status"] != wire.ChangeChanged || kinds["body"] != wire.ChangeChanged ||
		kinds["task"] != wire.ChangeFact || p.SourceRevision != "task:ENG-42@3" || p.Note == "" {
		t.Errorf("the update previews %+v at %q", p.Changes, p.SourceRevision)
	}
}

// A CHANGE PREPARED AGAINST AN OLD VERSION IS STALE, and a revision that moved
// re-reads as another, so an approval never applies what the person did not
// see. Mutation: drop the if_match comparison and this fails.
func TestAChangePreparedAgainstAnOldVersionIsStale(t *testing.T) {
	t.Parallel()
	read := reader(world())
	update, _ := catalog.Lookup("update_task")
	args := map[string]any{"item": "ENG-42", "if_match": float64(2), "status": "done"}
	if _, err := update.Preview(t.Context(), read, args); !errors.Is(err, catalog.ErrStale) {
		t.Errorf("an update against version 2 of a task at 3 = %v, want ErrStale", err)
	}
	edit, _ := catalog.Lookup("edit_page")
	args = map[string]any{"page": uid, "base_version": float64(1), "body": "x"}
	if _, err := edit.Preview(t.Context(), read, args); !errors.Is(err, catalog.ErrStale) {
		t.Errorf("an edit against version 1 of a page at 2 = %v, want ErrStale", err)
	}
	moved := world()
	moved["/work/ENG-42"].(map[string]any)["task"].(map[string]any)["version"] = 4
	if now, err := catalog.Revision(t.Context(), reader(moved), "task:ENG-42@3"); err != nil ||
		now != "task:ENG-42@4" {
		t.Errorf("a moved task re-reads as %q, %v", now, err)
	}
	if _, err := catalog.Revision(t.Context(), read, "task:ENG-404@1"); !errors.Is(err,
		catalog.ErrPreviewRead) {
		t.Errorf("a task the person may not read = %v, want ErrPreviewRead", err)
	}
	_ = assistanttest.Epoch
}

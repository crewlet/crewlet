package assistanttest

import (
	"encoding/json"
	"net/http"

	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/wire"
)

// RecordID is the one record id the examples and [World] name, so a call
// built from [ExampleArgs] or [ProposalArgs] reads a record World holds.
const RecordID = "5f0c5c8e-1a2b-4c3d-8e9f-0a1b2c3d4e5f"

// World is a small company as the routes a write's preview reads answer it,
// by request path and in each route's own shape: a task with a decision on its
// thread, a person's priorities, an agent, a parked coding run, a page and an
// artifact. Every proposing tool called with its [ProposalArgs] previews
// against it.
func World() map[string]any {
	return map[string]any{
		"/work/ENG-42": map[string]any{"task": map[string]any{"id": RecordID, "key": "ENG-42",
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
		"/sandbox-runs": map[string]any{"runs": []any{map[string]any{"turn_id": RecordID,
			"agent_handle": "dev", "work_key": "ENG-42", "question": "Which branch?"}}},
		"/pages/" + RecordID: map[string]any{"page": map[string]any{"id": RecordID,
			"title": "Runbook", "body": "Step one.", "version": 2}},
		"/artifacts/" + RecordID: map[string]any{"id": RecordID, "title": "Q3 deck",
			"head_number": 5},
	}
}

// ProposalArgs are each proposing tool's call against [World], the page
// change's against a page that accepts `work.filter.apply` ([FilterPage]).
var ProposalArgs = map[string]map[string]any{
	"create_task": {"title": "File the report", "assignee": "pm", "body": "Details."},
	"update_task": {"item": "ENG-42", "if_match": float64(3), "status": "in_progress",
		"assignee": "dev", "body": "A new description."},
	"comment_on_task":     {"item": "ENG-42", "body": "Done on my side."},
	"answer_decision":     {"item": "ENG-42", "choice": "a"},
	"set_my_priorities":   {"items": []any{"ENG-42", "ENG-9"}},
	"mark_inbox":          {"read": []any{"r1", "r2"}},
	"pause_agent":         {"handle": "pm", "reason": "Too many hand-offs.", "stop_running": true},
	"resume_agent":        {"handle": "pm"},
	"steer_turn":          {"turn_id": RecordID, "note": "Use the staging branch."},
	"answer_coding_run":   {"turn_id": RecordID, "answer": "main"},
	"create_page":         {"title": "Onboarding", "body": "Welcome."},
	"edit_page":           {"page": RecordID, "base_version": float64(2), "body": "Step one, then two."},
	"share_artifact":      {"artifact": RecordID, "to": "company", "role": "viewer"},
	"unshare_artifact":    {"artifact": RecordID, "from": "unit:platform"},
	"propose_page_change": {"action": "work.filter.apply", "args": map[string]any{"status": "blocked"}},
}

// Answer is what a route answers world's record at r's path: 200 with it, or
// 404 in the engine's envelope when world holds none.
func Answer(world map[string]any, r exec.Request) exec.Response {
	body, ok := world[r.Path]
	if !ok {
		return exec.Response{Status: http.StatusNotFound,
			Body: []byte(`{"error":"not_found","message":"There is no such record here."}`)}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		// A world is literals this package writes; one that does not encode
		// is a defect in the fixture, said as the route would say a fault.
		return exec.Response{Status: http.StatusInternalServerError,
			Body: []byte(`{"error":"internal","message":"The fixture does not encode."}`)}
	}
	return exec.Response{Status: http.StatusOK, Header: http.Header{}, Body: raw}
}

// FilterPage is the task list's page context, accepting the filter the page
// change in [ProposalArgs] applies.
func FilterPage() *wire.PageContext {
	return &wire.PageContext{Screen: "work-tasks", Title: "Work › Tasks",
		Params: map[string]string{}, Query: map[string]string{},
		Resources:      []wire.Resource{{Kind: wire.ResourceProject, ID: "ENG"}},
		SourceRevision: "url:3f9c1a0e", Accepts: []string{"work.filter.apply"}}
}

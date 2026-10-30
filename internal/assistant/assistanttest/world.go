package assistanttest

import (
	"crypto/sha256"
	"encoding/hex"
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
		"/worklet/ENG-42": map[string]any{"task": map[string]any{"id": RecordID, "key": "ENG-42",
			"title": "Ship it", "status": "todo", "priority": "normal", "assignee": "pm",
			"body": "The old description.", "version": 3, "tags": []any{"release"}}},
		"/worklet/comments": map[string]any{"key": "ENG-42", "title": "Ship it",
			"comments": []any{map[string]any{"id": "c1", "ask": "pm", "body": "Which day?",
				"decision": map[string]any{"question": "Which day?", "options": []any{
					map[string]any{"id": "a", "label": "Monday"}}}}}},
		"/worklet/my-work": map[string]any{"handle": "pm"},
		// WHO IS ASKING, and where a create of theirs that names no project or
		// container lands: what a create's preview resolves its default from.
		"/viewer": map[string]any{"handle": "pm", "project": "ENG", "space": "ENG"},
		"/worklet/inbox": map[string]any{"handle": "pm", "notices": []any{
			map[string]any{"record_id": "r1", "subject_key": "ENG-42", "excerpt": "Ship it moved to review."},
			map[string]any{"record_id": "r2", "subject_key": "SEC-7", "excerpt": "Rotate the signing key."}}},
		"/worklet/people/pm": map[string]any{"handle": "pm", "version": 7, "priorities": []any{"ENG-9"}},
		"/org": map[string]any{"timezone": "Europe/Berlin",
			"roles": []any{map[string]any{"handle": "pm", "name": "Product manager", "kind": "agent"},
				map[string]any{"handle": "qa", "name": "QA Engineer", "kind": "agent"}},
			"units": []any{map[string]any{"name": "Platform", "roles": []any{
				map[string]any{"handle": "ada", "name": "Ada Lovelace", "kind": "human"}},
				"children": []any{map[string]any{"name": "Backend", "roles": []any{
					map[string]any{"handle": "engineer", "name": "Backend engineer", "kind": "agent"},
					map[string]any{"handle": "dev", "name": "Developer", "kind": "agent"}}}}}}},
		"/agents/pm": map[string]any{"handle": "pm", "role": "Product manager",
			"live": map[string]any{"activity": "working"}},
		"/sandbox-runs": map[string]any{"runs": []any{map[string]any{"turn_id": RecordID,
			"launch_id": "launch-1", "agent_handle": "dev", "work_key": "ENG-42",
			"question": "Which branch?", "question_context": "",
			"decision": map[string]any{"question": "Which branch?", "role": "approver",
				"options": []any{map[string]any{"id": "main", "label": "Target main"},
					map[string]any{"id": "next", "label": "Target next"}}}}}},
		"/doclet/pages/" + RecordID: map[string]any{"page": map[string]any{"id": RecordID,
			"title": "Runbook", "body": "Step one.", "version": 2}},
		"/artifacts/" + RecordID: map[string]any{"id": RecordID, "title": "Q3 deck",
			"head_number": 5},
		"/config/charter": Charter(WorldMission, "Settling up takes seconds.",
			"Miles is the final authority.", "Money is an integer number of cents."),
		"/config/roles/qa": Entity(`"rev-4"`, map[string]any{"handle": "qa",
			"name": "QA Engineer", "goal": "Review the tests.",
			"token_budget": map[string]any{"day": 1000000}}),
		"/config": Entity(`"rev-4"`, map[string]any{"name": "FairSplit",
			"token_budget": map[string]any{"day": 10000000, "month": 120000000}}),
		// DATADOG'S AGENTS TAB: the QA engineer off, and the roles the
		// organization lists, under the company revision's tag.
		"/setup/integrations/datadog/access": Entity(`"rev-4"`, map[string]any{
			"kind": "datadog", "revision": "rev-4", "syncing": false,
			"options": []any{map[string]any{"lane": "datadog", "live": true, "levels": []any{
				map[string]any{"id": "role-ro", "name": "Datadog Read Only Role"},
				map[string]any{"id": "role-admin", "name": "Datadog Admin Role",
					"administers": true}}}},
			"seats": []any{map[string]any{"handle": "qa", "name": "QA Engineer",
				"lanes": map[string]any{"datadog": map[string]any{
					"desired": map[string]any{"enabled": false}, "observed": map[string]any{
						"exists": false, "enabled": false}, "plan": []any{}, "tool": "off"}}}}}),
	}
}

// Entity is a configuration read's answer: the record as JSON under the
// strong entity tag tag, as `GET /config` and `GET /config/{kind}/{id}`
// answer one.
func Entity(tag string, record map[string]any) exec.Response {
	raw, _ := json.Marshal(record)
	return exec.Response{Status: http.StatusOK, Header: http.Header{"Etag": {tag}}, Body: raw}
}

// WorldMission is the mission [World]'s principles state, with the stray word
// a person asks the assistant to remove.
const WorldMission = "Help friends share costs fairly. test"

// Charter is `GET /config/charter`'s answer for the three parts: the
// principles as JSON, and an entity tag that is a digest of them, as the route
// answers one (a strong tag naming exactly these bytes).
func Charter(mission, vision string, policies ...string) exec.Response {
	if policies == nil {
		policies = []string{}
	}
	raw, _ := json.Marshal(map[string]any{"mission": mission, "vision": vision,
		"policies": policies})
	sum := sha256.Sum256(raw)
	return exec.Response{Status: http.StatusOK,
		Header: http.Header{"Etag": {`"charter-` + hex.EncodeToString(sum[:16]) + `"`}},
		Body:   raw}
}

// ProposalArgs are each proposing tool's call against [World], the page
// change's against a page that accepts `work.filter.apply` ([FilterPage]).
var ProposalArgs = map[string]map[string]any{
	"create_task": {"title": "File the report", "assignee": "pm", "body": "Details."},
	"update_task": {"item": "ENG-42", "if_match": float64(3), "status": "in_progress",
		"assignee": "dev", "body": "A new description."},
	"comment_on_task":           {"item": "ENG-42", "body": "Done on my side."},
	"answer_decision":           {"item": "ENG-42", "choice": "a"},
	"set_my_priorities":         {"items": []any{"ENG-42", "ENG-9"}},
	"mark_inbox":                {"read": []any{"r1", "r2"}},
	"pause_agent":               {"handle": "pm", "reason": "Too many hand-offs.", "stop_running": true},
	"resume_agent":              {"handle": "pm"},
	"steer_turn":                {"turn_id": RecordID, "note": "Use the staging branch."},
	"answer_coding_run":         {"turn_id": RecordID, "launch_id": "launch-1", "choice": "main"},
	"create_page":               {"title": "Onboarding", "body": "Welcome."},
	"edit_page":                 {"page": RecordID, "base_version": float64(2), "body": "Step one, then two."},
	"share_artifact":            {"artifact": RecordID, "to": "company", "role": "viewer"},
	"unshare_artifact":          {"artifact": RecordID, "from": "unit:platform"},
	"propose_page_change":       {"action": "work.filter.apply", "args": map[string]any{"status": "blocked"}},
	"update_company_principles": {"mission": "Help friends share costs fairly."},
	"set_token_budget":          {"seat": "qa", "window": "day", "tokens": float64(1500000)},
	"set_seat_access": {"handle": "qa", "integration": "datadog", "enabled": true,
		"access": "Datadog Read Only Role"},
}

// Answer is what a route answers world's record at r's path: 200 with it, or
// 404 in the engine's envelope when world holds none. A record that is itself
// a response (one whose route answers a header, [Charter]) is answered as it
// is.
func Answer(world map[string]any, r exec.Request) exec.Response {
	body, ok := world[r.Path]
	if !ok {
		return exec.Response{Status: http.StatusNotFound,
			Body: []byte(`{"error":"not_found","message":"There is no such record here."}`)}
	}
	if resp, whole := body.(exec.Response); whole {
		return resp
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
	return &wire.PageContext{Screen: "worklet-tasks", Title: "Worklet › Tasks",
		Params: map[string]string{}, Query: map[string]string{},
		Resources:      []wire.Resource{{Kind: wire.ResourceProject, ID: "ENG"}},
		SourceRevision: "url:3f9c1a0e", Accepts: []string{"work.filter.apply"},
		ActionArgs: map[string]*wire.ActionArgs{"work.filter.apply": StatusFilterArgs()}}
}

// StatusFilterArgs are the arguments a task list's filter takes, as the
// screen declares them: a status, by one of its values, or null to clear it.
func StatusFilterArgs() *wire.ActionArgs {
	closed := false
	return &wire.ActionArgs{Type: wire.ArgTypes{wire.ArgObject}, AdditionalProperties: &closed,
		Properties: map[string]*wire.ActionArgs{"status": {
			Type: wire.ArgTypes{wire.ArgString, wire.ArgNull}, Description: "Status",
			Enum: []string{"todo", "in_progress", "in_review", "done", "blocked"}}}}
}

package queries_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tracker"
)

// parkedForAna is a coding run parked on a question put to Ana, carrying every
// machine detail its row can: the bridge's tool log, a branch, the coding CLI's
// session and command, a trace, an owner, a brief.
func parkedForAna(asked time.Time) sandbox.PendingRun {
	return sandbox.PendingRun{
		TurnID: "run-1", LaunchID: "launch-1", AgentHandle: "swe", Role: "SWE",
		Status: sandbox.StatusAwaiting, SandboxID: "box-1", CodingAgent: "claude-code",
		Placement: "e2b", CommandID: "cmd-7", Owner: "node-a@1",
		TaskDescription: "Port the parser to the new grammar",
		Question:        "Keep the v1 parser behind a flag, or drop it?",
		Audience:        "requester", AudienceHandles: []string{"ana"},
		AskedAt: asked, PausedAt: asked.Add(2 * time.Second), PauseTTLSeconds: 1800,
		UpdatedAt: asked.Add(3 * time.Second), CreatedAt: asked.Add(-time.Hour),
		ConversationKey: "slack:C0ENG:1718.003", Branch: "wip/parser", SessionID: "sess-9",
		TraceID: "trace-1", DelegationChain: []string{"lead"}, Reply: "requester",
		BridgeCalls: []sandbox.BridgeCall{{Name: "list_work_items", Args: `{"q":"parser"}`,
			Output: "ENG-412", At: asked.Add(-time.Minute)}},
		WorkItem: &types.WorkItem{Backend: types.WorkNative, ID: "t-412", Key: "ENG-412", Project: "ENG"},
	}
}

// decisionsAs asks `decisions` about Ana as caller, over one parked run whose
// task the tracker names, and decodes the answer as the wire carries it.
func decisionsAs(t *testing.T, caller auth.Principal, run sandbox.PendingRun) map[string]any {
	t.Helper()
	work := &stubWork{places: map[string]tracker.TurnPlace{
		"run-1": {TaskID: "t-412", Key: "ENG-412", Title: "Port the parser", Ordinal: 2},
	}}
	sources := partySources(t, work)
	sources.Sandbox = fixedRuns{run}
	got, err := askAs(t, sources, "decisions", map[string]any{"handle": "ana"}, caller)
	if err != nil {
		t.Fatalf("decisions: %v", err)
	}
	body, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	items, _ := decoded["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("decisions = %s, want the one parked run", body)
	}
	return items[0].(map[string]any)
}

// A PERSON ANSWERING A PARKED RUN READS THE DECISION, NOT THE RUN (ADR-0031).
// The member's item is exactly what the question needs — what was asked, about
// which task, by whom, of whom, since when, until when its place is kept and
// where a reply reaches it — and none of what the machine ran: no tool log, no
// branch, no session, no trace, no node, no brief, and no `run_detail`. An
// admin asking the same question is served the board's row beside it.
func TestAMemberReadsTheDecisionAParkedRunAsksAndNothingItRan(t *testing.T) {
	t.Parallel()
	asked := time.Date(2031, 4, 16, 10, 0, 0, 0, time.UTC)
	item := decisionsAs(t, asMember("ops-1"), parkedForAna(asked))
	if _, has := item["run_detail"]; has {
		t.Errorf("a member's decision carries the run's whole row: %v", item)
	}
	run, _ := item["run"].(map[string]any)
	want := map[string]any{
		"turn_id":            "run-1",
		"question":           "Keep the v1 parser behind a flag, or drop it?",
		"task":               map[string]any{"id": "t-412", "key": "ENG-412", "title": "Port the parser"},
		"agent_handle":       "swe",
		"asked":              []any{"ana"},
		"asked_at":           "2031-04-16T10:00:00Z",
		"keeps_place_until":  "2031-04-16T10:30:02Z",
		"answerable_in_chat": true,
		"chat_surface":       "slack",
	}
	got, _ := json.Marshal(run)
	wanted, _ := json.Marshal(want)
	if string(got) != string(wanted) {
		t.Errorf("a member's parked run reads\n\t%s\nwant exactly\n\t%s", got, wanted)
	}
	if item["at"] != "2031-04-16T10:00:00Z" {
		t.Errorf("the item is placed at %v, want when the question was put", item["at"])
	}

	admin := decisionsAs(t, asAdmin("ops-1"), parkedForAna(asked))
	detail, _ := admin["run_detail"].(map[string]any)
	if detail == nil || detail["branch"] != "wip/parser" || detail["bridge_calls"] == nil {
		t.Errorf("an admin's decision lost the board's row: %v", admin)
	}
	// ONE DEADLINE: the board's row states the same instant the decision
	// does, so no screen derives it from the stamp and the TTL beside it.
	if detail["keeps_place_until"] != "2031-04-16T10:30:02Z" || detail["chat_surface"] != "slack" {
		t.Errorf("the board's row says the box is held until %v on %v, want the decision's",
			detail["keeps_place_until"], detail["chat_surface"])
	}
	if run, _ := admin["run"].(map[string]any); run["keeps_place_until"] != "2031-04-16T10:30:02Z" {
		t.Errorf("an admin's decision reads %v — the same decision a member does, plus the row", run)
	}
}

// THE PLACE A RUN KEEPS IS THE ENGINE'S TO SAY, and once its box is reclaimed
// it keeps none: `keeps_place_until` is null — present, so a card says
// "answering restarts the work" rather than nothing — and a run on no task
// names none. Every list on the card is a list, empty included.
func TestAReclaimedRunKeepsNoPlace(t *testing.T) {
	t.Parallel()
	asked := time.Date(2031, 4, 16, 10, 0, 0, 0, time.UTC)
	run := parkedForAna(asked)
	run.Status, run.SandboxID, run.PausedAt = sandbox.StatusReseed, "", time.Time{}
	run.WorkItem, run.ConversationKey = nil, ""
	got := decisionsAs(t, asMember("ops-1"), run)["run"].(map[string]any)
	for _, key := range []string{"keeps_place_until", "task"} {
		if v, present := got[key]; !present || v != nil {
			t.Errorf("%s = %v (present %v), want null", key, v, present)
		}
	}
	if got["answerable_in_chat"] != false {
		t.Errorf("a run asked from no thread is answerable in chat: %v", got)
	}
	if _, has := got["chat_surface"]; has {
		t.Errorf("a run asked from no thread names a surface: %v", got)
	}
}

// A TITLE IS THE TRACKER'S, FOR THE ITEM THE ROW NAMES. A run whose turn the
// tracker places on another task (ADR-0022 says it cannot; a reader trusting
// that blindly would put one task's title on another's key) keeps its key and
// carries no title.
func TestADecisionNamesOnlyItsOwnTasksTitle(t *testing.T) {
	t.Parallel()
	run := parkedForAna(time.Date(2031, 4, 16, 10, 0, 0, 0, time.UTC))
	run.WorkItem.ID = "t-999"
	task := decisionsAs(t, asMember("ops-1"), run)["run"].(map[string]any)["task"].(map[string]any)
	if task["key"] != "ENG-412" || task["title"] != nil {
		t.Errorf("task = %v, want its own key and no title borrowed from another task", task)
	}
}

package queries

import (
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// decisionRunFields is every field a member's [DecisionRun] carries, and why a
// person answering the question may read it (ADR-0031: what the company
// published, and the decisions put to them).
var decisionRunFields = map[string]string{
	"turn_id":            "the run's address, which answer_run is called with — an id, not what it opens",
	"question":           "what the agent asked this person: the decision itself",
	"task":               "the work item the question is about, as the board names it",
	"agent_handle":       "the colleague who asked",
	"asked":              "who the question is put to, as the chart names them",
	"asked_fallback":     "that it was put to the lead chain because the agent named nobody the chart has",
	"asked_at":           "when it was asked — how long it has waited",
	"keeps_place_until":  "how long an answer keeps the work's place, by the reaper's own rule",
	"answerable_in_chat": "whether a reply in a thread reaches the run",
	"chat_surface":       "which surface that thread is on",
}

// runDetailServedAs is every key of the board's row ([serialiseRun]) a member
// reads in another shape, by the [DecisionRun] field that carries it.
var runDetailServedAs = map[string]string{
	"turn_id":            "turn_id",
	"question":           "question",
	"agent_handle":       "agent_handle",
	"work_item":          "task",
	"audience_handles":   "asked",
	"audience_fallback":  "asked_fallback",
	"answerable_in_chat": "answerable_in_chat",
	"chat_surface":       "chat_surface",
	"asked_at":           "asked_at",
	"keeps_place_until":  "keeps_place_until",
	"paused_at":          "keeps_place_until",
	"pause_ttl_seconds":  "keeps_place_until",
	"box_exists":         "keeps_place_until",
}

// runDetailWithheld is every key of the board's row a member is NOT served,
// and why it is the machine's rather than the decision's.
var runDetailWithheld = map[string]string{
	"work_key":            "the unit of work's ledger key, an id the engine dedupes writes on",
	"launch_id":           "the job the row holds, which the live tail is asked by",
	"role":                "the seat's role name; the decision names the seat by handle",
	"status":              "the run's lifecycle state; what it means for an answer is keeps_place_until",
	"coding_agent":        "which coding CLI ran the job",
	"placement":           "which configured box backend holds it — the deployment",
	"task_description":    "the brief the turn handed the coding agent: a prompt",
	"audience":            "the agent's own free label for its audience, which `asked` resolved",
	"branch":              "the pushed work-in-progress branch",
	"trace_id":            "the run's trace",
	"owner":               "the process incarnation that owns the seat — a node",
	"started_at":          "when the job launched",
	"updated_at":          "the row's last write",
	"reply":               "who the suspended turn owes a reply, the turn engine's record",
	"bridge_calls":        "the bridge's tool log: every call's arguments and output",
	"bridge_calls_elided": "how many tool calls the log dropped",
	"session_id":          "the coding CLI's own session id",
	"command_id":          "the command the engine launched",
	"delegation_chain":    "the chain of asks that led to the run",
}

// EVERY FIELD OF A MEMBER'S PARKED RUN IS CLASSIFIED, and so is every key of the
// row it is cut from — in both directions, so a field added to [DecisionRun]
// says why a person may read it, and a key added to the board's row
// ([serialiseRun]) is put on one side of the withholding rule in a diff
// somebody reads, rather than reaching a teammate because nobody decided.
func TestEveryDecisionRunFieldIsClassified(t *testing.T) {
	t.Parallel()
	carried := map[string]bool{}
	ty := reflect.TypeFor[DecisionRun]()
	for i := range ty.NumField() {
		name, _, _ := strings.Cut(ty.Field(i).Tag.Get("json"), ",")
		carried[name] = true
		if decisionRunFields[name] == "" {
			t.Errorf("DecisionRun carries %q, which nothing says a person answering the "+
				"question may read: classify it in decisionRunFields, or take it off", name)
		}
	}
	for name := range decisionRunFields {
		if !carried[name] {
			t.Errorf("decisionRunFields classifies %q, which DecisionRun no longer carries", name)
		}
	}

	run := sandbox.PendingRun{TurnID: "t", AudienceHandles: []string{"ana"},
		WorkItem: &types.WorkItem{ID: "i", Key: "K"}, AskedAt: time.Now()}
	row := serialiseRun(run)
	for _, key := range slices.Sorted(maps.Keys(row)) {
		served, isServed := runDetailServedAs[key]
		_, isWithheld := runDetailWithheld[key]
		switch {
		case isServed && isWithheld:
			t.Errorf("the row's %q is both served and withheld", key)
		case !isServed && !isWithheld:
			t.Errorf("the board's row carries %q, which nobody classified for a member: "+
				"put it in runDetailServedAs (naming the DecisionRun field that carries "+
				"it) or runDetailWithheld (saying why it is the machine's)", key)
		case isServed && !carried[served]:
			t.Errorf("the row's %q is said to be served as %q, which DecisionRun does not carry",
				key, served)
		case isWithheld && runDetailWithheld[key] == "":
			t.Errorf("the row's %q is withheld with no reason", key)
		}
	}
	for _, table := range []map[string]string{runDetailServedAs, runDetailWithheld} {
		for key := range table {
			if _, still := row[key]; !still {
				t.Errorf("%q is classified, and the board's row no longer carries it", key)
			}
		}
	}
}
